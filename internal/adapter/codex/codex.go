// Package codex connects a per-run Codex app-server and remote TUI to otag.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nesiler/otag/internal/adapter"
)

type Adapter struct{}

// A light run (guard level light) runs Codex with full access and no approvals.
const (
	lightApproval = "never"
	lightSandbox  = "danger-full-access"
)

// turnPolicy is the policy every turn/start of a thread carries (the listener's
// first turn and every TUI turn through the relay), derived from the thread's
// start parameters: a light thread (sandbox danger-full-access) gets full access and
// no approvals, every other one approvals on request under the "otag" permissions.
func turnPolicy(thread map[string]any) map[string]any {
	if thread["sandbox"] == lightSandbox {
		return map[string]any{"approvalPolicy": lightApproval, "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}}
	}
	return map[string]any{"approvalPolicy": "on-request", "approvalsReviewer": "user", "permissions": "otag"}
}

func New() *Adapter                       { return &Adapter{} }
func (*Adapter) Harness() string          { return "codex" }
func (*Adapter) NoopOutput(string) []byte { return nil }

// JSON strings are also valid TOML basic strings. Do not shell-escape argv values.
func quote(s string) string                              { b, _ := json.Marshal(s); return string(b) }
func shellQuote(s string) string                         { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func override(argv []string, key, value string) []string { return append(argv, "-c", key+"="+value) }
func binary(spec adapter.RunSpec) string                 { return spec.Account.CommandOr("codex") }
func launchEnv(spec adapter.RunSpec) map[string]string {
	env := map[string]string{}
	for _, src := range []map[string]string{spec.Account.Env, spec.Profile.Env, spec.Env} {
		for k, v := range src {
			env[k] = v
		}
	}
	if spec.Account.ConfigDir != "" {
		env["CODEX_HOME"] = spec.Account.ConfigDir
	}
	return env
}

func (*Adapter) Prepare(ctx context.Context, spec adapter.RunSpec) (*adapter.LaunchSpec, error) {
	if spec.Profile.Mode == "headless" {
		return nil, fmt.Errorf("codex adapter requires interactive mode")
	}
	for _, arg := range spec.Profile.Args {
		if !slices.Contains([]string{"--no-alt-screen", "--no-daemon"}, arg) {
			return nil, fmt.Errorf("codex profile argument %s is unsupported; use profile model, effort and env fields", arg)
		}
	}
	env := launchEnv(spec)
	mode := env["OTAG_CODEX_MODE"]
	if mode != "" && mode != "server" && mode != "hooks" {
		return nil, fmt.Errorf("unknown OTAG_CODEX_MODE %q", mode)
	}
	if mode == "" {
		mode = "server"
	}
	if mode == "server" {
		if len(filepath.Join(spec.RunDir, "as.sock")) >= 104 {
			return nil, fmt.Errorf("codex Unix socket path exceeds the portable 103-byte limit; shorten OTAG_DATA_DIR")
		}
	}
	env["OTAG_CODEX_MODE"] = mode
	env["OTAG_CODEX_RUN_DIR"] = spec.RunDir
	// Profile flags are applied first; otag's required hooks and isolation win.
	base := append([]string{binary(spec)}, spec.Profile.Args...)
	base = override(base, "projects", "{"+quote(spec.Workdir)+"={trust_level=\"trusted\"}}")
	base = override(base, "developer_instructions", quote(spec.Instructions))
	if spec.Profile.Model != "" {
		base = override(base, "model", quote(spec.Profile.Model))
	}
	if spec.Profile.Effort != "" {
		base = override(base, "model_reasoning_effort", quote(spec.Profile.Effort))
	}
	tuiBase := append([]string(nil), base...)
	var err error
	if spec.Guard.IsLight() {
		// A light run has full access and is never asked: the hook guard (pre-tool
		// hooks, the relay's turn policy) is what otag enforces there.
		base = override(base, "approval_policy", quote(lightApproval))
		base = override(base, "sandbox_mode", quote(lightSandbox))
	} else if base, err = strictPermissions(ctx, spec, env["OTAG_DB"], base); err != nil {
		return nil, err
	}
	base = override(base, "features.hooks", "true")
	// Commands run in a non-login shell: a login shell re-reads the user's profile and
	// puts their PATH (an installed otag, say) ahead of otag's, so `otag done` could reach
	// another binary than the one that launched the run.
	base = override(base, "allow_login_shell", "false")
	// UserPromptSubmit also fires for app-server turn/start and turn/steer input: in
	// mode B its prompt proves a delivered message's marker.
	kinds := []string{"PreToolUse", "PostToolUse", "PermissionRequest", "UserPromptSubmit"}
	if mode == "hooks" {
		kinds = append(kinds, "SessionStart", "Stop", "Interrupt", "SessionEnd")
	}
	commands := map[string]bool{}
	for _, kind := range kinds {
		command := shellQuote(spec.OtagBin) + " event codex " + kind
		if mode == "hooks" && (kind == "SessionStart" || kind == "UserPromptSubmit") {
			command = shellQuote(spec.OtagBin) + " sidecar codex -- track " + kind
		}
		commands[command] = true
		timeout := 10
		if kind == "Interrupt" || kind == "SessionEnd" {
			timeout = 1
		}
		base = override(base, "hooks."+kind, fmt.Sprintf("[{hooks=[{type=\"command\",command=%s,timeout=%d}]}]", quote(command), timeout))
	}
	state, err := hookTrust(ctx, spec, env, base, commands)
	if err != nil {
		return nil, err
	}
	base = override(base, "hooks.state", state)
	// Remote resume rejects argv permission overrides. A per-run TUI home
	// supplies the same policy as local defaults, without modifying account config.
	var tuiConfig map[string]any
	accountHome := env["CODEX_HOME"]
	if accountHome == "" {
		home, _ := os.UserHomeDir()
		accountHome = filepath.Join(home, ".codex")
	}
	if b, err := os.ReadFile(filepath.Join(accountHome, "config.toml")); err == nil {
		if _, err := toml.Decode(string(b), &tuiConfig); err != nil {
			return nil, fmt.Errorf("codex account config: %w", err)
		}
	}
	if tuiConfig == nil {
		tuiConfig = map[string]any{}
	}
	var configText strings.Builder
	for i, arg := range base {
		if arg == "-c" && i+1 < len(base) {
			configText.WriteString(base[i+1] + "\n")
		}
	}
	var required map[string]any
	if _, err := toml.Decode(configText.String(), &required); err != nil {
		return nil, err
	}
	for k, v := range required {
		tuiConfig[k] = v
	}
	if spec.Guard.IsLight() {
		// The account's own permission profile would compete with full access.
		delete(tuiConfig, "default_permissions")
	} else {
		delete(tuiConfig, "sandbox_mode")
		delete(tuiConfig, "sandbox_workspace_write")
	}
	var config bytes.Buffer
	if err := toml.NewEncoder(&config).Encode(tuiConfig); err != nil {
		return nil, err
	}
	base = scrubArgv(env, base)
	tuiEnv := launchEnv(spec)
	tuiEnv["CODEX_HOME"] = filepath.Join(spec.RunDir, "tui-home")
	tuiBase = scrubArgv(tuiEnv, tuiBase)
	ls := &adapter.LaunchSpec{Env: env, Cwd: spec.Workdir, StartupTimeout: 30 * time.Second}
	if mode == "hooks" {
		sessionFile := filepath.Join(spec.RunDir, "SessionStart.json")
		ls.Sidecars = []adapter.Process{adapter.SidecarProcess(spec, "rollout", "rollout", sessionFile, spec.Profile.Model)}
		ls.Argv = append(base, "--no-daemon", "-C", spec.Workdir)
		if spec.ResumeSessionID != "" {
			ls.Argv = append(ls.Argv, "resume", spec.ResumeSessionID)
		}
		ls.Argv = append(ls.Argv, spec.Prompt)
		ls.Argv = append([]string{spec.OtagBin, "sidecar", "codex", "--", "serve", filepath.Join(spec.RunDir, "tui.log")}, ls.Argv...)
		return ls, nil
	}
	ls.Files = append(ls.Files, adapter.File{Path: "tui-home/config.toml", Mode: 0o600, Data: config.Bytes()})
	tuiBase = append([]string{"/usr/bin/env", "CODEX_HOME=" + filepath.Join(spec.RunDir, "tui-home")}, tuiBase...)
	sock := filepath.Join(spec.RunDir, "as.sock")
	url := "unix://" + sock
	// Keep startup diagnostics after otag cleans up a failed auxiliary pane.
	ls.Files = append(ls.Files, adapter.File{Path: "listen.sh", Data: []byte("#!/bin/sh\nexec \"$@\" 2>" + shellQuote(filepath.Join(spec.RunDir, "listen.log")) + "\n"), Mode: 0o700})
	tuiScript := "#!/bin/sh\ndir=" + shellQuote(spec.RunDir) + `
read -r sid < "$dir/thread-id" || exit 1
: > "$dir/tui-ready"
n=0
while [ ! -f "$dir/turn-ready" ]; do
  n=$((n + 1))
  if [ "$n" -ge 600 ]; then
    echo 'Codex initial turn did not start; see listen.log' >&2
    exit 1
  fi
  sleep 0.05
done
exec "$@" resume "$sid"
`
	ls.Files = append(ls.Files, adapter.File{Path: "tui.sh", Mode: 0o700, Data: []byte(tuiScript)})
	server := adapter.SidecarProcess(spec, "server", append([]string{"serve", filepath.Join(spec.RunDir, "server.log")}, append(base, "app-server", "--listen", url)...)...)
	server.Env, server.Cwd = env, spec.Workdir
	server.Ready, server.ReadyTimeout, server.URL = "unix:"+sock, 30*time.Second, url
	ls.Server = &server
	ls.Argv = append(tuiBase, "--remote", "unix://"+filepath.Join(spec.RunDir, "t.sock"), "-C", spec.Workdir)
	// SetupServer wires the owning listener and the exact-thread TUI handoff.
	return ls, nil
}

// strictPermissions adds a strict run's policy to the argv overrides: approvals on
// request and the native permission profile "otag", the run's write boundary.
func strictPermissions(ctx context.Context, spec adapter.RunSpec, db string, base []string) ([]string, error) {
	base = override(base, "approval_policy", quote("on-request"))
	base = override(base, "approvals_reviewer", quote("user"))
	// Native filesystem rules are the write boundary behind the hook guard. Codex cannot load
	// per-launch execpolicy rules, so git push remains a hook-level deny.
	fsRules := []string{`":root"="read"`, `":workspace_roots"={"."="write",".codex"="read",".agents"="read"`}
	for _, glob := range spec.Deny {
		glob = strings.Trim(strings.TrimPrefix(glob, "./"), "/")
		if glob == "" {
			continue
		}
		if !strings.Contains(glob, "/") {
			glob = "**/" + glob
		}
		access := "read"
		// 0.161 only supports deny for wildcard patterns other than a
		// trailing subtree /**. Preserve the native backstop for those.
		if strings.ContainsAny(strings.TrimSuffix(glob, "/**"), "*?[") {
			access = "deny"
		}
		fsRules[1] += "," + quote(glob) + "=" + quote(access)
	}
	fsRules[1] += "}"
	seen := map[string]bool{}
	meta, err := metadataRules(ctx, spec.Workdir, spec.Branch, db)
	if err != nil {
		return nil, err
	}
	for _, r := range meta {
		if !seen[r[0]] { // TOML rejects duplicate keys; the first rule wins
			fsRules = append(fsRules, quote(r[0])+"="+quote(r[1]))
			seen[r[0]] = true
		}
	}
	base = override(base, "permissions.otag", "{filesystem={"+strings.Join(fsRules, ",")+"},network={enabled=false}}")
	return override(base, "default_permissions", quote("otag")), nil
}

// metadataRules returns the native filesystem rules outside the workspace that an
// agent commit and `otag done`/`otag fail` need, as ordered [path, access] pairs. With
// branch set in a linked worktree, only that branch's ref is writable; a detached
// linked worktree (a review checkout) writes no ref.
//
// Codex resolves overlapping rules by specificity: a nested "read" inside a "write"
// root stays read-only, including renames over it and paths that do not exist yet
// (verified with `codex sandbox -P` on 0.161.0). Anything not listed falls back to
// ":root"="read". The coordinator runs git unsandboxed in the same repository, so
// nothing git executes or redirects through (config, hooks, commondir/gitdir
// pointers, other worktrees, submodule repositories) may become writable.
func metadataRules(ctx context.Context, workdir, branch, db string) ([][2]string, error) {
	gitPath := func(option string) string {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", option)
		cmd.Dir = workdir
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		dir := strings.TrimSpace(string(out))
		if !filepath.IsAbs(dir) {
			return ""
		}
		return filepath.Clean(dir)
	}
	var rules [][2]string
	add := func(access, dir string, names ...string) {
		for _, name := range names {
			rules = append(rules, [2]string{filepath.Join(dir, name), access})
		}
	}
	gitDir, common := gitPath("--git-dir"), gitPath("--git-common-dir")
	_, reftableErr := os.Stat(filepath.Join(common, "reftable"))
	if common != "" && branch != "" && gitDir != common && reftableErr == nil {
		// reftable keeps every ref in one store: no way to grant only this branch.
		return nil, fmt.Errorf("codex: %s uses the reftable ref format; a worktree run there could move every branch, so otag refuses it (use workspace = \"same\" or a files-backend repository)", common)
	}
	switch {
	case common == "":
	case branch != "" && gitDir != common:
		// A linked worktree on its own branch: a commit writes objects, the branch
		// ref (through <ref>.lock) and its reflog. Every other ref, packed-refs and the
		// main checkout stay read-only, so the agent cannot move main or another
		// task's branch (a ref that is only packed is written as a new loose file,
		// which the read-only refs/ also refuses).
		ref := filepath.Join("refs", "heads", filepath.FromSlash(branch))
		add("write", common, "objects", ref, ref+".lock", filepath.Dir(filepath.Join("logs", ref)))
	case gitDir != common:
		// A detached linked worktree (a review checkout): a commit there writes objects
		// and the worktree's own HEAD (under gitDir, below); no ref is writable.
		add("write", common, "objects")
	default:
		// Same mode or no branch: shared repository
		// data a commit writes, refs included.
		add("write", common, "objects", "refs", "logs", "packed-refs", "packed-refs.lock", "reftable")
	}
	switch {
	case gitDir == "":
	case gitDir != common:
		// A linked worktree owns worktrees/<name> (HEAD, index, locks, merge
		// state), except the pointers and config git follows from there.
		add("write", gitDir, "")
		add("read", gitDir, "commondir", "gitdir", "config.worktree")
	default:
		// The project checkout itself: per-checkout state lives at the top of
		// the common directory beside config and hooks, so protect those instead.
		add("write", gitDir, "")
		add("read", gitDir, "config", "config.worktree", "hooks", "info", "worktrees", "modules", "remotes", "branches", "commondir")
	}
	// SQLite in WAL mode writes the database and recreates its -wal/-shm siblings.
	// The rest of the data directory (other worktrees, runs, tui-home) stays read-only.
	if filepath.IsAbs(db) {
		db = filepath.Clean(db)
		add("write", filepath.Dir(db), filepath.Base(db), filepath.Base(db)+"-wal", filepath.Base(db)+"-shm")
	}
	return rules, nil
}

func (*Adapter) SetupServer(ctx context.Context, spec adapter.RunSpec, launch *adapter.LaunchSpec) error {
	params := map[string]any{"cwd": spec.Workdir, "developerInstructions": spec.Instructions,
		"approvalPolicy": "on-request", "approvalsReviewer": "user", "permissions": "otag"}
	if spec.Guard.IsLight() {
		delete(params, "permissions") // cannot be combined with sandbox
		params["approvalPolicy"], params["sandbox"] = lightApproval, lightSandbox
	}
	if spec.Profile.Model != "" {
		params["model"] = spec.Profile.Model
	}
	if spec.Profile.Effort != "" {
		params["config"] = map[string]any{"model_reasoning_effort": spec.Profile.Effort}
	}
	method := "thread/start"
	if spec.ResumeSessionID != "" {
		method = "thread/resume"
		params["threadId"] = spec.ResumeSessionID
		// Metadata only (0.161.0 schema); reconnects page turns via thread/turns/list.
		params["excludeTurns"] = true
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	// The listener creates the thread and retains its client. A new empty thread
	// cannot be resumed by another client until its first turn creates a rollout.
	// The pane signals startup before the listener submits the initial turn.
	idFile := filepath.Join(spec.RunDir, "thread-id")
	launch.Argv = append([]string{"/bin/sh", filepath.Join(spec.RunDir, "tui.sh")}, launch.Argv...)
	sc := adapter.SidecarProcess(spec, "listen", "bootstrap", launch.Server.URL, method, string(encoded), idFile, spec.Profile.Model, spec.Prompt)
	sc.ReadyTimeout = 30 * time.Second
	sc.Argv = append([]string{"/bin/sh", filepath.Join(spec.RunDir, "listen.sh")}, sc.Argv...)
	sc.Env = map[string]string{"OTAG_CODEX_SOURCE": "startup"}
	if spec.ResumeSessionID != "" {
		sc.Env["OTAG_CODEX_SOURCE"] = "resume"
	}
	launch.Sidecars = []adapter.Process{sc}
	return nil
}

func (*Adapter) Stop(ctx context.Context, target adapter.Target) error {
	if target.Pane == nil {
		return nil
	}
	if err := target.Pane.SendKeys(ctx, "Escape"); err != nil {
		return err
	}
	if err := pause(ctx, 100*time.Millisecond); err != nil {
		return err
	}
	if err := target.Pane.SendText(ctx, "/exit"); err != nil {
		return err
	}
	// Codex treats an immediate Enter after literal text as part of a paste.
	if err := pause(ctx, 600*time.Millisecond); err != nil {
		return err
	}
	return target.Pane.SendKeys(ctx, "Enter")
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.ServerSetup   = (*Adapter)(nil)
	_ adapter.SidecarRunner = (*Adapter)(nil)
	_ adapter.Stopper       = (*Adapter)(nil)
)
