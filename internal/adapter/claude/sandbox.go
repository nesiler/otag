package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

// Claude Code's own sandbox (https://code.claude.com/docs/en/sandboxing) is the write
// boundary of a Claude run: Bash commands and everything they start run under
// Seatbelt, writable only in the workspace, Claude's per-user temp directory, the git
// metadata a commit needs and otag's database (for `otag done`). The otag guard stays as
// a fast, best-effort check in front of it.
//
// The per-run --settings turns it on with:
//   - failIfUnavailable: Claude Code exits at startup when the sandbox cannot start
//     (the run then fails to launch) instead of running commands unsandboxed;
//   - allowUnsandboxedCommands false: the dangerouslyDisableSandbox retry is ignored
//     (in bypass mode it would run without a prompt). A false from --settings also
//     makes the sandbox admin-required, so a repository's settings cannot loosen it;
//   - the network is left to the permission mode: in otag's default bypass mode every
//     host is allowed.
//
// Built-in file tools (Read, Edit, Write) and MCP servers run outside the sandbox; the
// permission rules (DenyRules) and the PreToolUse guard cover them.

// maxRefDenies bounds the per-ref write denies (repositories with very many branches).
const maxRefDenies = 2000

// sandboxSettings returns the settings' sandbox object: the profile's own (excludedCommands,
// extra allowWrite paths, network lists, ...) with otag's boundary added. A profile
// may not switch the sandbox, its filesystem layer or its strict mode off.
func sandboxSettings(ctx context.Context, spec adapter.RunSpec, user any) (map[string]any, error) {
	sb := map[string]any{}
	if user != nil {
		m, ok := user.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claude: profile --settings: sandbox is not an object")
		}
		for k, v := range m {
			sb[k] = v
		}
	}
	for _, k := range []string{"enabled", "failIfUnavailable"} {
		if v, ok := sb[k]; ok && v != true {
			return nil, fmt.Errorf("claude: profile --settings: sandbox.%s must stay true; the sandbox is a otag run's write boundary", k)
		}
	}
	if v, ok := sb["allowUnsandboxedCommands"]; ok && v != false {
		return nil, fmt.Errorf("claude: profile --settings: sandbox.allowUnsandboxedCommands must stay false")
	}
	sb["enabled"], sb["failIfUnavailable"], sb["allowUnsandboxedCommands"] = true, true, false

	fsys := map[string]any{}
	if v, ok := sb["filesystem"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claude: profile --settings: sandbox.filesystem is not an object")
		}
		for k, v := range m {
			fsys[k] = v
		}
	}
	if v, ok := fsys["disabled"]; ok && v != false {
		return nil, fmt.Errorf("claude: profile --settings: sandbox.filesystem.disabled must stay false")
	}
	// Explicit, so a user-settings `disabled: true` (which --settings outranks) cannot
	// switch filesystem isolation off.
	fsys["disabled"] = false
	allow, deny, err := gitWritePaths(ctx, spec.Workdir, spec.Branch)
	if err != nil {
		return nil, err
	}
	allow = append([]string{spec.Workdir}, allow...)
	if db := spec.Env["OTAG_DB"]; filepath.IsAbs(db) {
		// `otag done`/`otag fail` run in the sandbox and write the database (WAL mode).
		allow = append(allow, db, db+"-wal", db+"-shm")
	}
	for k, add := range map[string][]string{"allowWrite": allow, "denyWrite": deny} {
		var list []any
		if v, ok := fsys[k]; ok {
			l, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("claude: profile --settings: sandbox.filesystem.%s is not a list", k)
			}
			list = append(list, l...)
		}
		for _, p := range add {
			if !slices.Contains(list, any(p)) {
				list = append(list, p)
			}
		}
		if len(list) > 0 {
			fsys[k] = list
		}
	}
	sb["filesystem"] = fsys
	return sb, nil
}

// gitWritePaths returns the git metadata a commit in workdir writes (allow) and the
// metadata the sandbox would otherwise leave writable that must not change (deny).
//
// In a linked worktree Claude Code's sandbox allows writes to the main repository's
// shared .git (except hooks and config), so `git commit` can update objects and refs.
// That would also let a command move main or another task's branch; otag denies every
// other existing branch ref, tags, remote-tracking refs, packed-refs and the main
// checkout's HEAD and index, keeping the run's own branch writable. In the project
// checkout itself ("same" mode) the git dir is inside the workspace and stays writable.
// The list is never silently partial: a failed ref listing, or more branches than
// maxRefDenies, fails the launch. Branches created after the launch are not covered.
func gitWritePaths(ctx context.Context, workdir, branch string) (allow, deny []string, err error) {
	git := func(args ...string) ([]string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = workdir
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("claude: git %s in %s: %w", strings.Join(args, " "), workdir, err)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
	}
	if _, err := os.Lstat(filepath.Join(workdir, ".git")); err != nil {
		return nil, nil, nil // not a git checkout: no shared metadata to protect
	}
	dirs, err := git("rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return nil, nil, err
	}
	if len(dirs) != 2 || !filepath.IsAbs(dirs[0]) || !filepath.IsAbs(dirs[1]) {
		return nil, nil, fmt.Errorf("claude: unexpected git dirs of %s: %q", workdir, dirs)
	}
	gitDir, common := filepath.Clean(dirs[0]), filepath.Clean(dirs[1])
	if gitDir == common {
		return nil, nil, nil
	}
	allow = []string{gitDir, filepath.Join(common, "objects")}
	own := ""
	if branch != "" {
		own = "refs/heads/" + branch
		allow = append(allow, filepath.Join(common, own), filepath.Join(common, own+".lock"),
			filepath.Dir(filepath.Join(common, "logs", own)))
	}
	deny = []string{filepath.Join(common, "packed-refs"), filepath.Join(common, "HEAD"), filepath.Join(common, "index"),
		filepath.Join(common, "refs", "tags"), filepath.Join(common, "refs", "remotes")}
	refs, err := git("for-each-ref", "--format=%(refname)", "refs/heads/")
	if err != nil {
		return nil, nil, err
	}
	if len(refs) > maxRefDenies {
		return nil, nil, fmt.Errorf("claude: %d branches in %s, more than the %d otag protects one by one; delete stale branches",
			len(refs), common, maxRefDenies)
	}
	for _, r := range refs {
		if r != "" && r != own {
			deny = append(deny, filepath.Join(common, filepath.FromSlash(r)))
		}
	}
	if entries, err := os.ReadDir(filepath.Join(common, "worktrees")); err == nil {
		for _, e := range entries {
			if p := filepath.Join(common, "worktrees", e.Name()); p != gitDir {
				deny = append(deny, p) // other worktrees' HEAD, index and pointers
			}
		}
	}
	return allow, deny, nil
}

// managedSettingsDir is where macOS keeps Claude Code's file-based managed settings.
var managedSettingsDir = "/Library/Application Support/ClaudeCode"

// checkManaged refuses a run when managed settings, which no --settings can override,
// switch off the hooks otag's guard and status rely on or, for a strict run, the
// sandbox. Only the file-based managed settings are read; MDM profiles and
// server-managed settings are not visible here (docs/ADAPTERS.md).
func checkManaged(level guard.Level) error {
	files := []string{filepath.Join(managedSettingsDir, "managed-settings.json")}
	if more, err := filepath.Glob(filepath.Join(managedSettingsDir, "managed-settings.d", "*.json")); err == nil {
		files = append(files, more...)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			DisableAllHooks       *bool `json:"disableAllHooks"`
			AllowManagedHooksOnly *bool `json:"allowManagedHooksOnly"`
			Sandbox               *struct {
				Enabled                  *bool `json:"enabled"`
				FailIfUnavailable        *bool `json:"failIfUnavailable"`
				AllowUnsandboxedCommands *bool `json:"allowUnsandboxedCommands"`
				Filesystem               *struct {
					Disabled *bool `json:"disabled"`
				} `json:"filesystem"`
			} `json:"sandbox"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("claude: managed settings %s: %w", f, err)
		}
		switch {
		case s.DisableAllHooks != nil && *s.DisableAllHooks:
			return fmt.Errorf("claude: managed settings %s set disableAllHooks; otag's guard hooks would not run", f)
		case s.AllowManagedHooksOnly != nil && *s.AllowManagedHooksOnly:
			return fmt.Errorf("claude: managed settings %s set allowManagedHooksOnly; otag's guard hooks would not run", f)
		}
		if sb := s.Sandbox; sb != nil && !level.IsLight() {
			isFalse := func(b *bool) bool { return b != nil && !*b }
			isTrue := func(b *bool) bool { return b != nil && *b }
			switch {
			case isFalse(sb.Enabled):
				return fmt.Errorf("claude: managed settings %s turn the sandbox off; otag runs Claude sandboxed", f)
			case sb.Filesystem != nil && isTrue(sb.Filesystem.Disabled):
				return fmt.Errorf("claude: managed settings %s turn the sandbox's filesystem isolation off", f)
			case isTrue(sb.AllowUnsandboxedCommands) || isFalse(sb.FailIfUnavailable):
				return fmt.Errorf("claude: managed settings %s relax the sandbox's strict mode (allowUnsandboxedCommands, failIfUnavailable)", f)
			}
		}
	}
	return nil
}
