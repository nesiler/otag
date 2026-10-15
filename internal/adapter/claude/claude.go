// Package claude is the Claude Code adapter. A run is an interactive `claude` TUI in a
// tmux pane, wired up per launch only:
//
//   - one --settings file (RunDir/settings.json) with otag's command hooks, hard
//     permission denies and Claude Code's sandbox (sandbox.go), merged with a
//     --settings the profile passes itself (Claude Code uses only the last --settings
//     flag);
//   - a --plugin-dir mod (RunDir/mod) whose turn.complete hook reports the end of every
//     turn, including the ones no settings hook sees (Esc, a refused permission);
//   - --append-system-prompt-file RunDir/instructions.md for otag's rules;
//   - --session-id chosen up front, CLAUDE_CONFIG_DIR from the account (unset for the
//     default ~/.claude).
//
// The folder-trust dialog cannot be pre-accepted per launch; AcceptTrust answers it.
// See docs/ADAPTERS.md and spike/results/claude.md.
package claude

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// Harness is the harness name.
const Harness = "claude"

// Files written under the run dir.
const (
	settingsFile     = "settings.json"
	instructionsFile = "instructions.md"
	modDir           = "mod"
)

// Efforts are the values `claude --effort` accepts.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// hookKinds are the settings hooks otag registers; each runs `otag event claude <kind>`.
// Tool hooks get the "*" matcher.
var hookKinds = []string{
	"SessionStart", "UserPromptSubmit",
	"PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure",
	"Notification", "Elicitation",
	"Stop", "StopFailure", "SubagentStop", "SessionEnd",
}

var toolHooks = []string{"PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure"}

// hookTimeout is the per-hook timeout in seconds. The bridge stops itself after 5 s;
// for SessionEnd a timeout also raises Claude Code's 1.5 s budget for the event.
const hookTimeout = 10

// scrubEnv are variables a Claude Code session exports to its children. otag is often
// started from inside a Claude session (a coordinator), and otag's tmux server keeps the
// environment of whoever started it, so a child would otherwise look like a nested
// session of the coordinator (CLAUDECODE, session id, messaging socket, effort).
//
// The second group selects credentials, provider, endpoint, model or effort and would
// override the run's account (CLAUDE_CONFIG_DIR login) and profile (--model, --effort):
// an inherited ANTHROPIC_API_KEY or ANTHROPIC_BASE_URL silently bills another account
// or routes through another gateway. An account or profile that needs one sets it in
// its env, which wins over this list.
var scrubEnv = []string{
	"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_EXECPATH", "CLAUDE_PID", "CLAUDE_EFFORT", "CLAUDE_PROJECT_DIR", "CLAUDE_ENV_FILE",

	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_PROFILE", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_WORKSPACE_ID", "ANTHROPIC_BETAS",
	"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL",
	"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_MANTLE",
	"ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "AWS_BEARER_TOKEN_BEDROCK",
	"ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_VERTEX_PROJECT_ID",
	"ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_AUTH_TOKEN", "ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_FOUNDRY_RESOURCE",
	"ANTHROPIC_AWS_API_KEY", "ANTHROPIC_AWS_BASE_URL", "ANTHROPIC_AWS_WORKSPACE_ID",
	"CLAUDE_CODE_EFFORT_LEVEL",
}

// Adapter is the Claude Code adapter.
type Adapter struct{}

// New returns the Claude Code adapter.
func New() *Adapter { return &Adapter{} }

// Harness implements adapter.Adapter.
func (*Adapter) Harness() string { return Harness }

// Prepare implements adapter.Adapter.
func (*Adapter) Prepare(ctx context.Context, spec adapter.RunSpec) (*adapter.LaunchSpec, error) {
	if spec.OtagBin == "" || !filepath.IsAbs(spec.OtagBin) {
		return nil, fmt.Errorf("claude: OtagBin must be an absolute path, got %q", spec.OtagBin)
	}
	if spec.RunDir == "" {
		return nil, fmt.Errorf("claude: no run dir")
	}
	if e := spec.Profile.Effort; e != "" && !slices.Contains(Efforts, e) {
		return nil, fmt.Errorf("claude: effort %q is not one of %s", e, strings.Join(Efforts, ", "))
	}
	userArgs, userSettings, anchor, err := splitSettings(spec.Profile.Args, spec.Workdir)
	if err != nil {
		return nil, err
	}
	if err := checkManaged(spec.Guard); err != nil {
		return nil, err
	}
	settings, err := buildSettings(ctx, spec, userSettings, anchor)
	if err != nil {
		return nil, err
	}

	sid := SessionID(spec)
	if spec.ResumeSessionID != "" {
		sid = spec.ResumeSessionID
	}

	env := map[string]string{}
	for _, m := range []map[string]string{spec.Account.Env, spec.Profile.Env} {
		for k, v := range m {
			env[k] = v
		}
	}
	// The default ~/.claude must be left unset: with CLAUDE_CONFIG_DIR set Claude Code
	// reads another Keychain item ("Claude Code-credentials-<hash>") and another global
	// config (<dir>/.claude.json), so the account would look logged out.
	configDir := accountConfigDir(spec.Account.ConfigDir)
	if configDir != "" {
		env["CLAUDE_CONFIG_DIR"] = configDir
	}
	// Variables otag sets win over the scrub list; everything else is removed by env -u.
	set := func(k string) bool {
		_, a := env[k]
		_, b := spec.Env[k]
		return a || b
	}
	argv := []string{"/usr/bin/env"}
	unset := scrubEnv
	if configDir == "" {
		unset = append(slices.Clone(scrubEnv), "CLAUDE_CONFIG_DIR") // the default account
	}
	for _, k := range unset {
		if !set(k) {
			argv = append(argv, "-u", k)
		}
	}

	argv = append(argv, spec.Account.CommandOr("claude"))
	if spec.Profile.Model != "" {
		argv = append(argv, "--model", spec.Profile.Model)
	}
	if spec.Profile.Effort != "" {
		argv = append(argv, "--effort", spec.Profile.Effort)
	}
	if spec.ResumeSessionID != "" {
		argv = append(argv, "--resume", sid)
	} else {
		argv = append(argv, "--session-id", sid)
	}
	argv = append(argv,
		"--settings", filepath.Join(spec.RunDir, settingsFile),
		"--plugin-dir", filepath.Join(spec.RunDir, modDir),
	)
	files := []adapter.File{{Path: settingsFile, Data: settings}}
	if strings.TrimSpace(spec.Instructions) != "" {
		argv = append(argv, "--append-system-prompt-file", filepath.Join(spec.RunDir, instructionsFile))
		files = append(files, adapter.File{Path: instructionsFile, Data: []byte(spec.Instructions)})
	}
	if !hasPermissionFlag(userArgs) {
		// Guards: the sandbox (Bash), permissions.deny and the PreToolUse hook;
		// all apply in bypass mode.
		argv = append(argv, "--dangerously-skip-permissions")
	}
	argv = append(argv, userArgs...)
	if spec.Prompt != "" {
		// "--" keeps a variadic flag in the profile args (--add-dir a b) from taking
		// the prompt as one of its values.
		argv = append(argv, "--", spec.Prompt)
	}
	modFiles, err := modFiles(spec)
	if err != nil {
		return nil, err
	}
	files = append(files, modFiles...)
	return &adapter.LaunchSpec{
		Argv:             argv,
		Env:              env,
		Files:            files,
		HarnessSessionID: sid,
		StartupTimeout:   30 * time.Second,
	}, nil
}

// SessionID derives the run's Claude session id (a UUID, as --session-id requires)
// from the run, so Prepare stays deterministic. The run dir is part of the input, so
// two otag installations (or test databases) never hand out the same id for "p-1-r1".
func SessionID(spec adapter.RunSpec) string {
	h := sha1.New()
	for _, s := range []string{"otag/claude", spec.RunDir, spec.RunID, spec.Workdir, spec.ProjectPath} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	b := h.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x50 // version 5 (name-based, SHA-1)
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// hasPermissionFlag reports whether the profile chose the permission mode itself.
func hasPermissionFlag(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "--permission-mode", "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions":
			return true
		}
	}
	return false
}

// splitSettings removes --settings from the profile args and loads it: Claude Code
// ignores every --settings but the last, so otag merges the profile's into its own.
// The value is a JSON object or a file (relative paths are taken from workdir). anchor is
// where the profile's `/path` permission rules point: the file's directory, or the
// workdir for an inline object (Claude Code anchors CLI-given rules at the primary
// working directory).
func splitSettings(args []string, workdir string) (rest []string, user map[string]any, anchor string, err error) {
	var vals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--settings":
			if i+1 >= len(args) {
				return nil, nil, "", fmt.Errorf("claude: --settings in profile args needs a value")
			}
			vals = append(vals, args[i+1])
			i++
		case strings.HasPrefix(a, "--settings="):
			vals = append(vals, strings.TrimPrefix(a, "--settings="))
		default:
			rest = append(rest, a)
		}
	}
	if len(vals) == 0 {
		return rest, nil, "", nil
	}
	v := strings.TrimSpace(vals[len(vals)-1]) // the last one is what claude would use
	data := []byte(v)
	anchor = workdir
	if !strings.HasPrefix(v, "{") {
		p := v
		if !filepath.IsAbs(p) {
			p = filepath.Join(workdir, p)
		}
		if data, err = os.ReadFile(p); err != nil {
			return nil, nil, "", fmt.Errorf("claude: profile --settings: %w", err)
		}
		anchor = filepath.Dir(p)
	}
	if err := json.Unmarshal(data, &user); err != nil {
		return nil, nil, "", fmt.Errorf("claude: profile --settings %s: %w", v, err)
	}
	return rest, user, anchor, nil
}

// pathRuleTools take gitignore-style path rules (permissions docs, "Read and Edit").
var pathRuleTools = []string{"Read", "Edit", "Write", "NotebookEdit", "MultiEdit", "Glob"}

// reanchor rewrites `Tool(/path)` rules (anchored at the settings source) to the
// absolute `Tool(//<anchor>/path)`, since the merged file lives in the run dir and a
// `/path` rule there would point into it: a profile's Read(/secrets/**) deny must keep
// protecting <anchor>/secrets. `//`, `~/` and cwd-relative rules are left alone.
func reanchor(rules []any, anchor string) []any {
	if anchor == "" {
		return rules
	}
	out := make([]any, len(rules))
	for i, r := range rules {
		out[i] = r
		s, ok := r.(string)
		if !ok {
			continue
		}
		tool, rest, ok := strings.Cut(s, "(")
		if !ok || !strings.HasSuffix(rest, ")") || !slices.Contains(pathRuleTools, strings.TrimSpace(tool)) {
			continue
		}
		p := strings.TrimSuffix(rest, ")")
		if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
			continue
		}
		out[i] = tool + "(/" + filepath.ToSlash(filepath.Clean(anchor)) + p + ")"
	}
	return out
}

// buildSettings returns the per-launch settings file: the profile's own --settings (if
// any) with otag's hooks appended per event, otag's hard denies added to
// permissions.deny, hooks forced on (disableAllHooks false), Claude Code's sandbox on
// (sandboxSettings; not for a light run, which keeps only a profile's own sandbox
// settings), and the prompt-suggestion agent turned off (it costs a model call after
// every turn and its SubagentStop is noise).
func buildSettings(ctx context.Context, spec adapter.RunSpec, user map[string]any, anchor string) ([]byte, error) {
	s := map[string]any{}
	for k, v := range user {
		s[k] = v
	}
	hooks := map[string]any{}
	if h, ok := s["hooks"]; ok {
		m, ok := h.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claude: profile --settings: hooks is not an object")
		}
		for k, v := range m {
			hooks[k] = v
		}
	}
	for _, kind := range hookKinds {
		group := map[string]any{"hooks": []any{map[string]any{
			"type":    "command",
			"command": spec.OtagBin,
			"args":    []string{"event", Harness, kind},
			"timeout": hookTimeout,
		}}}
		if slices.Contains(toolHooks, kind) {
			group["matcher"] = "*"
		}
		var list []any
		if prev, ok := hooks[kind]; ok {
			l, ok := prev.([]any)
			if !ok {
				return nil, fmt.Errorf("claude: profile --settings: hooks.%s is not a list", kind)
			}
			list = append(list, l...)
		}
		hooks[kind] = append(list, group)
	}
	s["hooks"] = hooks

	perms := map[string]any{}
	if p, ok := s["permissions"]; ok {
		m, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claude: profile --settings: permissions is not an object")
		}
		for k, v := range m {
			perms[k] = v
		}
	}
	for _, k := range []string{"allow", "ask", "deny"} {
		if v, ok := perms[k]; ok {
			l, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("claude: profile --settings: permissions.%s is not a list", k)
			}
			perms[k] = reanchor(l, anchor)
		}
	}
	var deny []any
	if d, ok := perms["deny"]; ok {
		deny = append(deny, d.([]any)...)
	}
	for _, r := range DenyRules(spec.Workdir, spec.Deny, spec.Branch == "") {
		if !slices.Contains(deny, any(r)) {
			deny = append(deny, r)
		}
	}
	perms["deny"] = deny
	s["permissions"] = perms
	// --settings outranks user, project and local settings, so an explicit false keeps
	// a repository's `disableAllHooks: true` from switching otag's guard off (only
	// managed settings can; checkManaged refuses those).
	if v, ok := s["disableAllHooks"]; ok && v != false {
		return nil, fmt.Errorf("claude: profile --settings sets disableAllHooks; otag's guard and status run as hooks")
	}
	s["disableAllHooks"] = false
	if !spec.Guard.IsLight() {
		sb, err := sandboxSettings(ctx, spec, s["sandbox"])
		if err != nil {
			return nil, err
		}
		s["sandbox"] = sb
	}

	if _, ok := s["promptSuggestionEnabled"]; !ok {
		s["promptSuggestionEnabled"] = false
	}
	// A --settings statusLine replaces the user's, so otag's wrapper records the quota
	// and then runs the user's own command (the profile's, or the one from the
	// project/account settings, looked up at run time).
	sl := map[string]any{}
	userCmd := ""
	if v, ok := s["statusLine"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claude: profile --settings: statusLine is not an object")
		}
		for k, v := range m {
			sl[k] = v
		}
		userCmd, _ = m["command"].(string)
	}
	sl["type"] = "command"
	sl["command"] = StatusLineCommand(spec, userCmd)
	s["statusLine"] = sl
	return json.MarshalIndent(s, "", "  ")
}

// userHome is the home directory; a variable for tests.
var userHome = os.UserHomeDir

// accountConfigDir is the CLAUDE_CONFIG_DIR for an account's config_dir: "" for none
// and for the default ~/.claude.
func accountConfigDir(dir string) string {
	if dir == "" {
		return ""
	}
	if home, err := userHome(); err == nil && home != "" && filepath.Clean(dir) == filepath.Join(home, ".claude") {
		return ""
	}
	return dir
}

// StatusLineCommand is the statusLine command of a run: `otag statusline` for the run's
// account, wrapping userCmd (when set) or the user's configured statusLine.
func StatusLineCommand(spec adapter.RunSpec, userCmd string) string {
	parts := []string{shellQuote(spec.OtagBin), "statusline", "--harness", Harness}
	if spec.Account.Name != "" {
		parts = append(parts, "--account", shellQuote(spec.Account.Name))
	}
	if dir := accountConfigDir(spec.Account.ConfigDir); dir != "" {
		parts = append(parts, "--config-dir", shellQuote(dir))
	}
	if strings.TrimSpace(userCmd) != "" {
		parts = append(parts, "--command", shellQuote(userCmd))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes s for /bin/sh.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_./=:@,+", r)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitPushRules deny git push in the spellings a Bash rule can see: plain, after global
// options (`git -C . push`, `git -c push.default=current push`, `git --git-dir=x push`)
// and by path (`/usr/bin/git push`). A rule's `*` spans words, so the option forms also
// refuse an unusual `git -C dir commit -m "... push ..."`; that is the price of a
// fallback that holds without the hook (a broken otag). Quoted (`git 'push'`) or wrapped
// (`sh -c`) forms are left to the hook guard (permissions docs, "What a Bash rule
// doesn't match").
var gitPushRules = []string{
	"Bash(git push)", "Bash(git push *)",
	"Bash(git -* push)", "Bash(git -* push *)",
	"Bash(*/git push)", "Bash(*/git push *)",
	"Bash(*/git -* push)", "Bash(*/git -* push *)",
}

// DenyRules are the hard permission rules of a run, enforced by Claude Code itself
// in front of the hook guard. Bash rules are gitPushRules; Edit rules cover every
// built-in file-editing tool and Read rules the reading ones (Read, Grep, Glob): a
// deny glob marks secrets too. Claude Code also turns Edit denies into sandbox
// denyWrite and Read denies into denyRead, so they hold for Bash commands. Path rules
// are anchored at the absolute workdir ("//" prefix); they are skipped when the
// workdir itself contains glob characters. The .git Edit rules are left out when the
// git dir is the workdir's own (sameMode): as a sandbox denyWrite they would stop
// `git commit` there; the hook guard still refuses Edit/Write on .git.
func DenyRules(workdir string, deny []string, sameMode bool) []string {
	rules := slices.Clone(gitPushRules)
	if workdir == "" || strings.ContainsAny(workdir, "*?[]{}()") {
		return rules
	}
	root := "/" + filepath.ToSlash(filepath.Clean(workdir)) // "//abs/path"
	tool := "Edit"
	add := func(p string) {
		r := tool + "(" + root + "/" + p + ")"
		if !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	if !sameMode {
		add(".git")
		add(".git/**")
	}
	globs := slices.Clone(deny)
	sort.Strings(globs)
	for _, tool = range []string{"Edit", "Read"} {
		for _, g := range globs {
			g = strings.TrimSuffix(strings.TrimSpace(g), "/")
			if g == "" {
				continue
			}
			if strings.Contains(g, "/") {
				g = "/" + strings.TrimPrefix(g, "/")
			} else {
				g = "/**/" + g
			}
			add(g[1:])
			if !strings.HasSuffix(g, "/**") {
				add(g[1:] + "/**")
			}
		}
	}
	return rules
}

var (
	_ adapter.Adapter      = (*Adapter)(nil)
	_ adapter.TrustHandler = (*Adapter)(nil)
	_ adapter.Stopper      = (*Adapter)(nil)
	_ adapter.Guarder      = (*Adapter)(nil)
)
