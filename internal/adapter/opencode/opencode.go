// Package opencode is the otag adapter for OpenCode v2 (tested with v2.0.20).
//
// Topology (spike/results/opencode.md, docs/ADAPTERS.md): every run gets its own
// `opencode serve` on 127.0.0.1 with a random password, the account's XDG dirs (the
// same ones an account wrapper sets) and a per-launch OPENCODE_CONFIG_CONTENT that loads
// otag's v2 plugin (guards and the system note) and hard deny rules. ServerSetup
// creates the session over the API (the server picks a native ses_ id) and the pane
// runs the TUI as a client: `<account command> --server URL -s <id> --prompt ... dir`.
// Status comes from a sidecar that reads the server's SSE stream (`otag sidecar
// opencode`), not from hooks; the plugin's guard asks otag through
// `otag event opencode permission.evaluate`.
package opencode

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

//go:embed plugin/index.js
var pluginJS []byte

//go:embed plugin/package.json
var pluginPkg []byte

//go:embed tui.sh
var tuiScript []byte

//go:embed serve.sh
var serveScript []byte

// Files under RunDir.
const (
	dirName          = "opencode"
	pluginDir        = dirName + "/plugin/otag"
	passwordFile     = dirName + "/password"
	instructionsFile = dirName + "/instructions.md"
	tuiFile          = dirName + "/tui.sh"
	serveFile        = dirName + "/serve.sh"
)

// PluginID is the id of otag's OpenCode plugin (checked through /api/plugin).
const PluginID = "otag"

// Adapter is the OpenCode adapter. The zero value is not usable; use New.
type Adapter struct {
	// FreePort returns a free TCP port on 127.0.0.1 (tests replace it).
	FreePort func() (int, error)
	// Secret returns the server password (tests replace it).
	Secret func() (string, error)
	// LookPath resolves a command name (tests replace it).
	LookPath func(string) (string, error)
	// Home is the user's home directory (tests replace it).
	Home func() (string, error)
	// Getenv finds the run dir for Stop and Deliver (default os.Getenv).
	Getenv func(string) string
	// SetupTimeout bounds SetupServer's waits for the API and the plugin.
	SetupTimeout time.Duration
	// Heartbeat is how long the sidecar's stream may stay silent (0 = HeartbeatTimeout).
	Heartbeat time.Duration
}

// New returns the OpenCode adapter.
func New() *Adapter {
	return &Adapter{FreePort: freePort, Secret: secret, LookPath: exec.LookPath, Home: os.UserHomeDir,
		SetupTimeout: 20 * time.Second}
}

// Harness implements adapter.Adapter.
func (*Adapter) Harness() string { return "opencode" }

// NoopOutput implements adapter.Adapter: "{}" answers the plugin's guard question with
// "no opinion" (the plugin takes an empty answer, a nonzero exit or a signal as a
// failure and denies); SSE kinds ignore output.
func (*Adapter) NoopOutput(kind string) []byte {
	if kind == KindEvaluate {
		return []byte("{}")
	}
	return nil
}

// Prepare implements adapter.Adapter.
func (a *Adapter) Prepare(_ context.Context, spec adapter.RunSpec) (*adapter.LaunchSpec, error) {
	if spec.RunDir == "" || spec.Workdir == "" || spec.OtagBin == "" {
		return nil, fmt.Errorf("opencode: RunDir, Workdir and OtagBin are required")
	}
	if _, err := ParseModel(spec.Profile.Model); err != nil {
		return nil, err
	}
	port, err := a.FreePort()
	if err != nil {
		return nil, fmt.Errorf("opencode: free port: %w", err)
	}
	pw, err := a.Secret()
	if err != nil {
		return nil, fmt.Errorf("opencode: password: %w", err)
	}
	url := "http://127.0.0.1:" + strconv.Itoa(port)
	content, err := ConfigContent(spec)
	if err != nil {
		return nil, err
	}

	env := map[string]string{}
	for k, v := range spec.Account.Env {
		env[k] = v
	}
	for k, v := range spec.Profile.Env {
		env[k] = v
	}
	if d := spec.Account.ConfigDir; d != "" {
		// An account wrapper's layout: every OpenCode dir under the account dir.
		env["XDG_CONFIG_HOME"] = filepath.Join(d, "config")
		env["XDG_DATA_HOME"] = filepath.Join(d, "data")
		env["XDG_STATE_HOME"] = filepath.Join(d, "state")
		env["XDG_CACHE_HOME"] = filepath.Join(d, "cache")
		if _, ok := env["GH_CONFIG_DIR"]; !ok {
			// Tools started by OpenCode keep the user's normal gh config (as such a wrapper does).
			if home, err := a.Home(); err == nil && home != "" {
				env["GH_CONFIG_DIR"] = filepath.Join(home, ".config", "gh")
			}
		}
	}
	env["OPENCODE_PASSWORD"] = pw

	ls := &adapter.LaunchSpec{
		Argv: TUIArgv(spec, a.command(spec), url, spec.ResumeSessionID),
		Env:  env,
		Files: []adapter.File{
			{Path: pluginDir + "/package.json", Data: pluginPkg},
			{Path: pluginDir + "/index.js", Data: pluginJS},
			{Path: passwordFile, Data: []byte(pw)},
			{Path: instructionsFile, Data: []byte(spec.Instructions)},
			{Path: tuiFile, Data: tuiScript, Mode: 0o700},
			{Path: serveFile, Data: serveScript, Mode: 0o700},
		},
		HarnessSessionID: spec.ResumeSessionID,
		Server: &adapter.Process{
			Name: "server",
			Argv: []string{"/bin/sh", filepath.Join(spec.RunDir, serveFile), a.serverCommand(spec),
				"serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port)},
			Env:          map[string]string{"OPENCODE_CONFIG_CONTENT": content},
			Cwd:          spec.Workdir,
			Ready:        "tcp:127.0.0.1:" + strconv.Itoa(port),
			ReadyTimeout: 30 * time.Second,
			URL:          RecordedURL(url, spec.RunDir),
		},
	}
	ls.Sidecars = []adapter.Process{adapter.SidecarProcess(spec, "events", SidecarArgs(spec, url, spec.ResumeSessionID)...)}
	return ls, nil
}

// command is the TUI binary: the account's command (a wrapper such as a script setting XDG_*, resolved
// in the account dir's bin/ when it is not on PATH) or opencode.
func (a *Adapter) command(spec adapter.RunSpec) string {
	c := spec.Account.CommandOr("opencode")
	if spec.Account.Command == "" || strings.ContainsRune(c, '/') {
		return c
	}
	if _, err := a.LookPath(c); err == nil {
		return c
	}
	if d := spec.Account.ConfigDir; d != "" {
		p := filepath.Join(d, "bin", c)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return c
}

// serverCommand is the binary that runs `serve`: opencode itself. The account's XDG
// dirs are set explicitly (Prepare), so a wrapper is not needed; OTAG_OPENCODE_BIN in
// the account or profile env overrides it.
func (a *Adapter) serverCommand(spec adapter.RunSpec) string {
	if b := spec.Profile.Env["OTAG_OPENCODE_BIN"]; b != "" {
		return b
	}
	if b := spec.Account.Env["OTAG_OPENCODE_BIN"]; b != "" {
		return b
	}
	return "opencode"
}

// TUIArgv is the pane command: the TUI as a client of the run's server, wrapped by
// tui.sh, which reports the TUI's exit as session_end. Without a session id (before
// SetupServer) the TUI would create one itself; SetupServer always fills it in.
func TUIArgv(spec adapter.RunSpec, command, url, sessionID string) []string {
	argv := []string{"/bin/sh", filepath.Join(spec.RunDir, tuiFile), command, "--server", url}
	if sessionID != "" {
		argv = append(argv, "-s", sessionID)
	}
	if p := strings.TrimSpace(spec.Prompt); p != "" {
		argv = append(argv, "--prompt", spec.Prompt)
	}
	argv = append(argv, spec.Profile.Args...)
	return append(argv, spec.Workdir)
}

// RecordedURL is the server URL otag records for the run (sessions.server_url): the
// base URL with the run dir in the fragment, so Stop and Deliver find the server
// password where Prepare put it, whatever the caller's environment.
func RecordedURL(base, runDir string) string {
	return base + "#" + neturl.Values{"run_dir": {runDir}}.Encode()
}

// splitURL undoes RecordedURL.
func splitURL(recorded string) (base, runDir string) {
	base, frag, _ := strings.Cut(recorded, "#")
	if v, err := neturl.ParseQuery(frag); err == nil {
		runDir = v.Get("run_dir")
	}
	return base, runDir
}

// SidecarArgs are the arguments of the SSE sidecar for spec.
func SidecarArgs(spec adapter.RunSpec, url, sessionID string) []string {
	args := []string{"--url", url, "--dir", spec.Workdir}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if spec.Profile.Model != "" {
		args = append(args, "--model", spec.Profile.Model)
	}
	if spec.ResumeSessionID != "" {
		args = append(args, "--resume")
	}
	return args
}

// Model is an OpenCode model reference.
type Model struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	Variant    string `json:"variant,omitempty"`
}

// ParseModel splits "provider/model[#variant]". An empty string means the OpenCode
// default (nil).
func ParseModel(s string) (*Model, error) {
	if s == "" {
		return nil, nil
	}
	ref, variant, _ := strings.Cut(s, "#")
	provider, id, ok := strings.Cut(ref, "/")
	if !ok || provider == "" || id == "" {
		return nil, fmt.Errorf("opencode: model %q must be provider/model[#variant]", s)
	}
	return &Model{ProviderID: provider, ID: id, Variant: variant}, nil
}

// Rule is an OpenCode v2 permission rule (last match wins; "*" matches anything,
// including "/").
type Rule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// HardDenies are the rules otag puts after everything else (config content and the
// session's own rules), so they win over the user's config and saved approvals: a
// configured deny is decided before any plugin hook runs. They back up the plugin's
// guard, and hold without otag: pushes with git options,
// wrappers (env, command, sudo, VAR=...) or a git path are covered too. OpenCode's "*"
// matches anything, spaces included, so a rare false positive is possible (a commit
// message like `git -C . commit -m "a push b"`); the agent then rewords it.
func HardDenies(deny []string) []Rule {
	rules := []Rule{}
	for _, r := range []string{
		"git push *",      // git push, git push origin main
		"git -* push *",   // git -C dir push, git -c k=v push, git --git-dir=x push
		"* git push *",    // env git push, VAR=1 git push, sudo git push
		"* git -* push *", // env git -C . push
		"*/git push *",    // /usr/bin/git push
		"*/git -* push *",
	} {
		rules = append(rules, Rule{"shell", r, "deny"})
	}
	rules = append(rules, Rule{"edit", ".git", "deny"}, Rule{"edit", ".git/*", "deny"}, Rule{"edit", "*/.git/*", "deny"})
	for _, g := range deny {
		for _, r := range denyResources(g) {
			// Deny globs mark secrets too (.env*): no edits and no reads.
			rules = append(rules, Rule{"edit", r, "deny"}, Rule{"read", r, "deny"})
		}
	}
	return rules
}

// globClass matches a bracket expression ([ab], [!x], [^a-z]).
var globClass = regexp.MustCompile(`\[[^\]/]*\]`)

// denyResources turns a gitignore-style project deny glob into OpenCode edit
// resources (location-relative paths) that cover at least what the glob covers.
// OpenCode's wildcard knows only "*" (anything, "/" included) and "?", so: bracket
// classes become "*"; "**/" and "/**/" (zero or more directories) become "*", which
// also matches zero directories; a trailing "/**" is the directory itself, whose
// contents "<dir>/*" covers; a glob without "/" matches at any depth.
func denyResources(g string) []string {
	g = strings.TrimSpace(g)
	g = strings.TrimPrefix(g, "./")
	anchored := strings.Contains(strings.TrimSuffix(g, "/"), "/")
	g = strings.Trim(g, "/")
	if g == "" {
		return nil
	}
	g = strings.ReplaceAll(g, "\\", "")
	g = globClass.ReplaceAllString(g, "*")
	for strings.HasSuffix(g, "/**") {
		g = strings.TrimSuffix(g, "/**")
	}
	g = strings.ReplaceAll(g, "/**/", "*")
	if strings.HasPrefix(g, "**/") {
		g = "*" + strings.TrimPrefix(g, "**/")
	}
	for strings.Contains(g, "**") {
		g = strings.ReplaceAll(g, "**", "*")
	}
	if g == "" || g == "*" {
		return []string{"*"}
	}
	out := []string{g, g + "/*"}
	if !anchored {
		out = append(out, "*/"+g, "*/"+g+"/*")
	}
	return out
}

// ConfigContent is the per-launch OPENCODE_CONFIG_CONTENT: otag's plugin (a directory,
// as v2 requires) and the hard denies. The config `instructions` key is not used: v2
// accepts but never loads it; the plugin's context hook adds the system note.
func ConfigContent(spec adapter.RunSpec) (string, error) {
	cfg := map[string]any{
		"plugins": []any{map[string]any{
			"package": "file://" + filepath.Join(spec.RunDir, pluginDir),
			"options": map[string]any{
				"otag":         spec.OtagBin,
				"run":          spec.RunID,
				"instructions": filepath.Join(spec.RunDir, instructionsFile),
			},
		}},
		"permissions": HardDenies(spec.Deny),
	}
	b, err := json.Marshal(cfg)
	return string(b), err
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func secret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LaunchProvedByTurnStart implements adapter.LaunchTurnProver: the TUI submits the
// launch prompt itself (--prompt), with neither a receipt nor a hook carrying its
// text, so the first session.execution.started proves it.
func (*Adapter) LaunchProvedByTurnStart() bool { return true }

var (
	_ adapter.LaunchTurnProver = (*Adapter)(nil)
	_ adapter.Adapter          = (*Adapter)(nil)
	_ adapter.ServerSetup      = (*Adapter)(nil)
	_ adapter.SidecarRunner    = (*Adapter)(nil)
	_ adapter.Stopper          = (*Adapter)(nil)
	_ adapter.Deliverer        = (*Adapter)(nil)
)
