package cli

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/tui/wizard"
)

// /dev/null is a character device but no terminal: bare otag with it on stdin and
// stdout prints the usage, it never opens the wizard or the TUI.
func TestDevNullIsNoTerminal(t *testing.T) {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if isTerminal(null) {
		t.Fatal("/dev/null is a terminal")
	}
	f := newFresh(t, "claude")
	ui := &uiHooks{
		wizard: func(context.Context, wizard.Options) (wizard.Result, error) {
			t.Fatal("the wizard opened")
			return wizard.Result{}, nil
		},
		tui: func(context.Context, *app, []string) (result, error) { t.Fatal("the TUI opened"); return result{}, nil },
	}
	var errb strings.Builder
	code := Main(context.Background(), nil, Env{Stdin: null, Stdout: null, Stderr: &errb, Cwd: f.repo,
		Getenv: func(k string) string { return f.env[k] }, ui: ui})
	if code != ExitUsage || !strings.Contains(errb.String(), "run `otag init`") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// snapshot is every file of dir (name → sha256 and mode), to show nothing changed.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		st, _ := os.Stat(p)
		sum := sha256.Sum256(b)
		out[p] = string(sum[:]) + st.Mode().String() + st.ModTime().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Before the user agrees, the wizard only looks: the database is opened read-only
// (never migrated, created or chmod-ed) and the config dir is untouched.
func TestWizardWritesNothingBeforeApply(t *testing.T) {
	f := newFresh(t, "claude", "codex")
	f.cwd = f.root
	f.ok("init", "--yes", "--no-daemon")
	data, cfg := snapshot(t, f.env["OTAG_DATA_DIR"]), snapshot(t, f.cfg)
	f.cwd = f.repo // offers the repository: the wizard reads the known prefixes
	f.terminal = true
	var known map[string]string
	f.ui = &uiHooks{wizard: func(_ context.Context, o wizard.Options) (wizard.Result, error) {
		known = o.Base.Known
		return wizard.Result{}, nil // Esc
	}}
	if out := f.ok("init"); !strings.Contains(out, "nothing was written") {
		t.Fatalf("init: %s", out)
	}
	if known == nil {
		t.Fatal("the wizard did not get the known projects")
	}
	if got := snapshot(t, f.env["OTAG_DATA_DIR"]); !mapsEqual(got, data) {
		t.Fatalf("the data dir changed:\nbefore %v\nafter  %v", keys(data), keys(got))
	}
	if got := snapshot(t, f.cfg); !mapsEqual(got, cfg) {
		t.Fatalf("the config dir changed: %v", keys(got))
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, filepath.Base(k))
	}
	slices.Sort(out)
	return out
}

// An edit saved while the wizard is open is not overwritten by its Write.
func TestApplyRefusesAnEditDuringTheWizard(t *testing.T) {
	f := newFresh(t, "claude")
	f.cwd = f.root
	f.ok("init", "--yes", "--no-daemon")
	f.stub("codex")
	reg := filepath.Join(f.cfg, "registry.toml")
	edited := f.read("registry.toml") + "\n# saved in an editor meanwhile\n"
	f.terminal = true
	f.ui = &uiHooks{wizard: func(ctx context.Context, o wizard.Options) (wizard.Result, error) {
		if err := os.WriteFile(reg, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := o.Apply(ctx, o.Choices)
		return wizard.Result{Applied: err == nil, Report: r, Err: errString(err)}, nil
	}}
	code, _, errs := f.run("init", "--no-daemon")
	if code == 0 || !strings.Contains(errs, "changed since otag read it") {
		t.Fatalf("init: %d %s", code, errs)
	}
	if got := f.read("registry.toml"); got != edited {
		t.Fatalf("the edit was overwritten:\n%s", got)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// remove, add, remove within a second keeps both removed files.
func TestProjectRemoveKeepsEveryBackup(t *testing.T) {
	f := newFresh(t, "claude")
	f.cwd = f.root
	f.ok("init", "--yes", "--no-daemon")
	for range 2 {
		f.ok("project", "add", f.repo, "--yes", "--name", "web")
		f.ok("project", "remove", "web")
	}
	kept, _ := filepath.Glob(filepath.Join(f.cfg, "projects", "web.toml.removed-*"))
	if len(kept) != 2 {
		t.Fatalf("kept %v", kept)
	}
}

// A run that starts after remove's first check but before the project is retired
// is seen by the check inside the retiring transaction: nothing is retired and the
// file comes back.
func TestProjectRemoveRaceWithRunStart(t *testing.T) {
	h := newRunHarness(t)
	id := h.addTask("beta", "keep going")
	beforeRetire = func() { h.start(id, "--profile", "manual") }
	t.Cleanup(func() { beforeRetire = nil })
	code, e := h.json("project", "remove", "beta")
	wantCode(t, "remove with a run started meanwhile", code, e, "CONFLICT")
	if _, err := os.Stat(filepath.Join(h.cfg, "projects", "beta.toml")); err != nil {
		t.Fatalf("the project file was not restored: %v", err)
	}
	if kept, _ := filepath.Glob(filepath.Join(h.cfg, "projects", "beta.toml.removed-*")); len(kept) != 0 {
		t.Fatalf("left behind: %v", kept)
	}
	if n := h.count(`SELECT count(*) FROM projects WHERE name = 'beta' AND retired_at IS NULL`); n != 1 {
		t.Fatal("beta was retired with a live run")
	}
	beforeRetire = nil
	code, e = h.json("project", "remove", "beta")
	wantCode(t, "remove with a live run", code, e, "CONFLICT")
}

// Fixes quote paths, so they can be pasted.
func TestDoctorQuotesPaths(t *testing.T) {
	f := newFresh(t, "claude")
	f.cwd = f.root
	f.ok("init", "--yes", "--no-daemon")
	dir := filepath.Join(f.root, "claude work")
	if err := os.WriteFile(filepath.Join(f.cfg, "registry.toml"), []byte(f.read("registry.toml")+
		"\n[accounts.work]\nharness = \"claude\"\nconfig_dir = \""+dir+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, e := f.json("doctor")
	c := checksOf(t, e.Data)["account work"]
	if want := "CLAUDE_CONFIG_DIR='" + dir + "' claude"; !strings.Contains(c.Fix, want) {
		t.Fatalf("fix %q, want %q", c.Fix, want)
	}
}

// A non-default $CODEX_HOME becomes the codex account's config_dir.
func TestInitKeepsCustomCodexHome(t *testing.T) {
	f := newFresh(t, "codex")
	f.cwd = f.root
	f.env["CODEX_HOME"] = filepath.Join(f.root, "codex-b")
	f.ok("init", "--yes", "--no-daemon")
	if reg := f.read("registry.toml"); !strings.Contains(reg, `config_dir = "~/codex-b"`) {
		t.Fatalf("registry:\n%s", reg)
	}
}
