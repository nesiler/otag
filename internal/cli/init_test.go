package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/wizard"
)

// fresh is a machine where otag was just installed: no config, no database; stub
// agent CLIs that print a version; tmux and git (linked into a directory of their
// own, so no real agent CLI on the PATH is found); a git repository to work in.
type fresh struct {
	*harness
	stubs    string
	repo     string
	terminal bool
	ui       *uiHooks
}

func newFresh(t *testing.T, harnesses ...string) *fresh {
	t.Helper()
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	stubs := filepath.Join(root, "stubs")
	for _, d := range []string{tools, stubs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"tmux", "git"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not installed", tool)
		}
		if err := os.Symlink(p, filepath.Join(tools, tool)); err != nil {
			t.Fatal(err)
		}
	}
	f := &fresh{harness: &harness{t: t, root: root, cfg: filepath.Join(root, "cfg")}, stubs: stubs,
		repo: filepath.Join(root, "dev", "My App")}
	f.env = map[string]string{
		"HOME":            root,
		"USER":            "tester",
		"PATH":            stubs + ":" + tools + ":/usr/bin:/bin",
		"OTAG_DB":         filepath.Join(root, "data", "otag.db"),
		"OTAG_CONFIG_DIR": f.cfg,
		"OTAG_DATA_DIR":   filepath.Join(root, "data"),
		"OTAG_LAUNCHCTL":  filepath.Join(root, "no-launchctl"), // never the real one
	}
	for _, h := range harnesses {
		f.stub(h)
	}
	initRepo(t, f.repo)
	f.cwd = f.repo
	return f
}

// stub puts a fake agent CLI on the PATH.
func (f *fresh) stub(name string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.stubs, name), []byte("#!/bin/sh\necho '"+name+" 9.9.9 (stub)'\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fresh) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, Env{
		Stdout: &out, Stderr: &errb, Cwd: f.cwd,
		Getenv:   func(k string) string { return f.env[k] },
		Environ:  func() []string { return nil },
		Terminal: func() bool { return f.terminal },
		ui:       f.ui,
	})
	return code, out.String(), errb.String()
}

func (f *fresh) ok(args ...string) string {
	f.t.Helper()
	code, out, errs := f.run(args...)
	if code != 0 {
		f.t.Fatalf("otag %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

func (f *fresh) json(args ...string) (int, env) {
	f.t.Helper()
	code, out, _ := f.run(append([]string{"--json"}, args...)...)
	var e env
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		f.t.Fatalf("otag %v: bad json %q: %v", args, out, err)
	}
	if e.V != 1 || e.OK != (code == 0) {
		f.t.Fatalf("envelope %+v with exit %d", e, code)
	}
	return code, e
}

func (f *fresh) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.cfg, rel))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestInitYesFresh(t *testing.T) {
	f := newFresh(t, "claude", "codex")
	code, e := f.json("init", "--yes", "--no-daemon")
	if code != 0 {
		t.Fatalf("init: %+v", e.Error)
	}
	var d initData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Sync == nil || d.Sync.Projects.Added != 1 || d.Daemon != nil || d.Skill != nil {
		t.Fatalf("data %s", e.Data)
	}
	if d.Project == nil || d.Project.Name != "my-app" || d.Project.Review != "codex" {
		t.Fatalf("project %+v", d.Project)
	}
	if !slices.Equal(d.Profiles, []string{"claude-opus", "claude-sonnet", "claude-haiku", "codex", "codex-medium"}) {
		t.Fatalf("profiles %v", d.Profiles)
	}
	reg := f.read("registry.toml")
	if strings.Contains(reg, "config_dir =") || !strings.Contains(reg, "[profiles.codex-medium]") {
		t.Fatalf("registry:\n%s", reg)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Library", "LaunchAgents")); !os.IsNotExist(err) {
		t.Fatal("--no-daemon installed the launch agent")
	}
	if _, err := os.Stat(filepath.Join(f.root, ".agents")); !os.IsNotExist(err) {
		t.Fatal("--yes installed the skill")
	}
	out := f.ok("project", "list")
	if !strings.Contains(out, "my-app") || !strings.Contains(out, "claude-opus") {
		t.Fatalf("project list:\n%s", out)
	}

	// Idempotent: a second run adds nothing and rewrites nothing.
	before := reg
	code, e = f.json("init", "--yes", "--no-daemon")
	if code != 0 || !strings.Contains(string(e.Data), `"nothing_to_do":true`) {
		t.Fatalf("second init: %d %s", code, e.Data)
	}
	if f.read("registry.toml") != before {
		t.Fatal("the second init changed registry.toml")
	}
	if entries, _ := os.ReadDir(f.cfg); len(entries) != 3 { // registry.toml, projects/, the config lock
		t.Fatalf("config dir: %v", entries)
	}
	if out := f.ok("init", "--yes", "--no-daemon"); !strings.Contains(out, "nothing to add") {
		t.Fatalf("human: %s", out)
	}
}

func TestInitAddsNewHarness(t *testing.T) {
	f := newFresh(t, "claude")
	f.cwd = f.root // not a repository: no project
	f.ok("init", "--yes", "--no-daemon")
	edited := strings.Replace(f.read("registry.toml"), "[profiles.claude-sonnet]", "# keep me\n[profiles.claude-sonnet]", 1)
	if err := os.WriteFile(filepath.Join(f.cfg, "registry.toml"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	f.stub("opencode")
	out := f.ok("init", "--yes", "--no-daemon")
	if !strings.Contains(out, "added to") {
		t.Fatalf("init: %s", out)
	}
	reg := f.read("registry.toml")
	if !strings.HasPrefix(reg, edited) || !strings.Contains(reg, "[accounts.opencode]") || !strings.Contains(reg, "# keep me") {
		t.Fatalf("registry:\n%s", reg)
	}
	_, e := f.json("profiles")
	if !strings.Contains(string(e.Data), `"opencode"`) {
		t.Fatalf("profiles not synced: %s", e.Data)
	}
}

func TestInitForce(t *testing.T) {
	f := newFresh(t, "claude")
	f.ok("init", "--yes", "--no-daemon")
	old := f.read("registry.toml") + "\n# my edit\n"
	if err := os.WriteFile(filepath.Join(f.cfg, "registry.toml"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	f.stub("codex")
	_, e := f.json("init", "--yes", "--no-daemon", "--force")
	var d initData
	_ = json.Unmarshal(e.Data, &d)
	if len(d.Files) != 1 || d.Files[0].Action != setup.Replace || d.Files[0].Backup == "" {
		t.Fatalf("files %+v", d.Files)
	}
	b, err := os.ReadFile(d.Files[0].Backup)
	if err != nil || string(b) != old {
		t.Fatalf("backup %q %v", b, err)
	}
	if reg := f.read("registry.toml"); strings.Contains(reg, "# my edit") || !strings.Contains(reg, "[accounts.codex]") {
		t.Fatalf("registry:\n%s", reg)
	}
	// The project written by the first init still loads (same profile names).
	f.ok("sync")
}

func TestInitRefusals(t *testing.T) {
	f := newFresh(t, "claude")
	for _, c := range []struct {
		args []string
		code string
	}{
		{[]string{"init"}, "USAGE"},          // no terminal, no --yes
		{[]string{"init", "extra"}, "USAGE"}, // no arguments
	} {
		code, e := f.json(c.args...)
		wantCode(t, strings.Join(c.args, " "), code, e, c.code)
	}
	f.terminal = true
	called := false
	f.ui = &uiHooks{wizard: func(context.Context, wizard.Options) (wizard.Result, error) {
		called = true
		return wizard.Result{}, nil
	}}
	code, e := f.json("init") // --json never opens the wizard
	wantCode(t, "--json init", code, e, "USAGE")
	if called {
		t.Fatal("--json opened the wizard")
	}

	// tmux missing: say how to install it, write nothing.
	f.env["PATH"] = f.stubs + ":/nonexistent"
	code, e = f.json("init", "--yes")
	wantCode(t, "init without tmux", code, e, "UNAVAILABLE")
	if !strings.Contains(e.Error.Message, "brew install tmux") {
		t.Fatalf("message %q", e.Error.Message)
	}
	// No agent CLI at all.
	g := newFresh(t)
	code, e = g.json("init", "--yes")
	wantCode(t, "init without agents", code, e, "UNAVAILABLE")
	if !strings.Contains(e.Error.Message, "claude.ai/install.sh") {
		t.Fatalf("message %q", e.Error.Message)
	}
	for _, d := range []*fresh{f, g} {
		if _, err := os.Stat(d.cfg); !os.IsNotExist(err) {
			t.Fatal("a refused init wrote the config dir")
		}
		if _, err := os.Stat(d.env["OTAG_DB"]); !os.IsNotExist(err) {
			t.Fatal("a refused init created the database")
		}
	}
}

// The wizard gets the detected defaults and an Apply that does the real work; its
// "open the TUI" runs the TUI.
func TestInitWizard(t *testing.T) {
	f := newFresh(t, "claude", "codex")
	f.terminal = true
	tuiRan := false
	f.ui = &uiHooks{
		wizard: func(ctx context.Context, o wizard.Options) (wizard.Result, error) {
			if o.Mode != wizard.Init || !o.OfferTUI || o.Choices.Project == nil || !o.Choices.Daemon {
				t.Errorf("options %+v", o)
			}
			if p := o.Preview(o.Choices); p.Err != "" || len(p.Files) != 2 {
				t.Errorf("preview %+v", p)
			}
			c := o.Choices
			c.Daemon = false
			r, err := o.Apply(ctx, c)
			return wizard.Result{Applied: err == nil, OpenTUI: true, Choices: c, Report: r}, err
		},
		tui: func(context.Context, *app, []string) (result, error) {
			tuiRan = true
			return result{}, nil
		},
	}
	f.ok("init")
	if !tuiRan {
		t.Fatal("the TUI did not open")
	}
	if !strings.Contains(f.read("projects/my-app.toml"), `prefix = "mya"`) {
		t.Fatal("project file")
	}

	// Cancelled: nothing written, exit 0.
	g := newFresh(t, "claude")
	g.terminal = true
	g.ui = &uiHooks{wizard: func(context.Context, wizard.Options) (wizard.Result, error) { return wizard.Result{}, nil }}
	if out := g.ok("init"); !strings.Contains(out, "nothing was written") {
		t.Fatalf("cancel: %s", out)
	}
	if _, err := os.Stat(g.cfg); !os.IsNotExist(err) {
		t.Fatal("a cancelled wizard wrote the config dir")
	}
}

// Bare otag: the wizard on a fresh terminal, the TUI once configured, the usage and
// a hint without a terminal or with --json.
func TestBareOtag(t *testing.T) {
	f := newFresh(t, "claude")
	var ran []string
	f.ui = &uiHooks{
		wizard: func(context.Context, wizard.Options) (wizard.Result, error) {
			ran = append(ran, "wizard")
			return wizard.Result{}, nil
		},
		tui: func(context.Context, *app, []string) (result, error) {
			ran = append(ran, "tui")
			return result{}, nil
		},
	}
	code, out, errs := f.run()
	if code != ExitUsage || out != "" || !strings.Contains(errs, "usage: otag") || !strings.Contains(errs, "run `otag init` to set up") {
		t.Fatalf("no terminal, fresh: %d %q %q", code, out, errs)
	}
	f.terminal = true
	if code, _, errs := f.run("--json"); code != ExitUsage || !strings.Contains(errs, "usage: otag") {
		t.Fatalf("--json: %d %s", code, errs)
	}
	if code, _, errs := f.run(); code != 0 {
		t.Fatalf("terminal, fresh: %d %s", code, errs)
	}
	f.ok("init", "--yes", "--no-daemon")
	if code, _, errs := f.run(); code != 0 {
		t.Fatalf("terminal, configured: %d %s", code, errs)
	}
	if !slices.Equal(ran, []string{"wizard", "tui"}) {
		t.Fatalf("ran %v", ran)
	}
	f.terminal = false
	if _, _, errs := f.run(); !strings.Contains(errs, "hint: run `otag tui`") || strings.Contains(errs, "not set up yet") {
		t.Fatalf("no terminal, configured: %s", errs)
	}
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		code, out, errs := f.run(args...)
		if code != 0 || !strings.Contains(out, "  init ") || !strings.Contains(out, "  project ") || errs != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errs)
		}
	}
}

func TestProjectCommands(t *testing.T) {
	f := newFresh(t, "claude", "codex")
	f.cwd = f.root
	code, e := f.json("project", "list")
	wantCode(t, "list before init", code, e, "CONFIG_INVALID")
	if !strings.Contains(e.Error.Message, "otag init") {
		t.Fatalf("message %q", e.Error.Message)
	}
	f.ok("init", "--yes", "--no-daemon")
	if out := f.ok("project", "list"); !strings.Contains(out, "otag project add") {
		t.Fatalf("empty list: %s", out)
	}
	other := filepath.Join(f.root, "plain")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		code string
	}{
		{[]string{"project", "add", f.repo}, "USAGE"}, // no terminal, no --yes
		{[]string{"project", "add", other, "--yes"}, "INVALID_ARGUMENT"},
		{[]string{"project", "add", filepath.Join(f.root, "missing"), "--yes"}, "INVALID_ARGUMENT"},
		{[]string{"project", "add", f.repo, "--yes", "--guard", "loose"}, "INVALID_ARGUMENT"},
		{[]string{"project", "add", f.repo, "--yes", "--prefix", "9"}, "INVALID_ARGUMENT"},
		{[]string{"project", "frob"}, "USAGE"},
	} {
		code, e := f.json(c.args...)
		wantCode(t, strings.Join(c.args, " "), code, e, c.code)
	}
	// A subdirectory registers the repository's top level.
	sub := filepath.Join(f.repo, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.cwd = sub
	out := f.ok("project", "add", "--yes", "--name", "web", "--guard", "strict")
	if !strings.Contains(out, "added project web (prefix web)") {
		t.Fatalf("add: %s", out)
	}
	if p := f.read("projects/web.toml"); !strings.Contains(p, `guard = "strict"`) || !strings.Contains(p, `path = "~/dev/My App"`) {
		t.Fatalf("project file:\n%s", p)
	}
	code, e = f.json("project", "add", "--yes", "--name", "again")
	wantCode(t, "add twice", code, e, "CONFLICT")
	_, e = f.json("project", "list")
	var list []projectInfo
	_ = json.Unmarshal(e.Data, &list)
	if len(list) != 1 || list[0].Name != "web" || list[0].Guard != "strict" || list[0].ReviewProfile != "codex" {
		t.Fatalf("list %s", e.Data)
	}
	f.ok("--project", "web", "task", "add", "keep me")

	// Remove: the file is kept aside, the project retired, its task stays.
	out = f.ok("project", "remove", "web")
	if !strings.Contains(out, "removed project web") {
		t.Fatalf("remove: %s", out)
	}
	if _, err := os.Stat(filepath.Join(f.cfg, "projects", "web.toml")); !os.IsNotExist(err) {
		t.Fatal("the project file is still loaded")
	}
	kept, _ := filepath.Glob(filepath.Join(f.cfg, "projects", "web.toml.removed-*"))
	if len(kept) != 1 {
		t.Fatalf("kept %v", kept)
	}
	if out := f.ok("status"); strings.Contains(out, "web") {
		t.Fatalf("status still lists web:\n%s", out)
	}
	code, e = f.json("project", "remove", "web")
	wantCode(t, "remove twice", code, e, "NOT_FOUND")

	// Added again: the old prefix and the old task come back.
	f.ok("project", "add", f.repo, "--yes", "--name", "web")
	if out := f.ok("--project", "web", "task", "list"); !strings.Contains(out, "keep me") {
		t.Fatalf("task list:\n%s", out)
	}
}
