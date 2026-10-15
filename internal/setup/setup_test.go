package setup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/registry"
)

var now = time.Date(2026, 10, 15, 12, 30, 0, 0, time.UTC)

// stubs writes executable scripts named names into a new directory and returns it.
func stubs(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\necho "+n+" 1.0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLookPath(t *testing.T) {
	dir := stubs(t, "claude")
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("x"), 0o644); err != nil { // not executable
		t.Fatal(err)
	}
	if p, ok := LookPath("claude", "relative:"+dir); !ok || p != filepath.Join(dir, "claude") {
		t.Fatalf("claude: %q %v", p, ok)
	}
	if _, ok := LookPath("codex", dir); ok {
		t.Fatal("a file without x bits was found")
	}
	if _, ok := LookPath("claude", "relative"); ok {
		t.Fatal("a relative PATH entry was searched")
	}
}

func TestDetect(t *testing.T) {
	bin := stubs(t, "claude", "opencode", "tmux", "git")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Prober{PATH: bin, Home: home, Run: func(_ context.Context, b string, args ...string) (string, error) {
		name := filepath.Base(b)
		switch {
		case name == "git" && args[0] == "-C":
			if args[1] == "/repo/sub" {
				return "/repo\n", nil
			}
			return "fatal: not a git repository\n", fmt.Errorf("exit 128")
		case name == "opencode":
			return "", fmt.Errorf("broken")
		}
		return "\n" + name + " 1.2.3\nmore\n", nil
	}}
	d := p.Detect(context.Background(), "/repo/sub")
	if got := d.Found(); !slices.Equal(got, []string{"claude", "opencode"}) {
		t.Fatalf("found %v", got)
	}
	c := d.Harness("claude")
	if c.Version != "claude 1.2.3" || c.ConfigDir != filepath.Join(home, ".claude") || c.Path != filepath.Join(bin, "claude") {
		t.Fatalf("claude %+v", c)
	}
	if o := d.Harness("opencode"); !o.Found() || o.Version != "" || o.ConfigDir != "" {
		t.Fatalf("opencode %+v", o)
	}
	if d.Harness("codex").Found() || len(d.Missing()) != 0 || d.Repo != "/repo" {
		t.Fatalf("detection %+v", d)
	}
	if d := p.Detect(context.Background(), "/elsewhere"); d.Repo != "" {
		t.Fatalf("repo outside a git checkout: %q", d.Repo)
	}
	if d := (Prober{PATH: stubs(t, "claude")}).Detect(context.Background(), ""); !slices.Equal(d.Missing(), []string{"tmux", "git"}) {
		t.Fatalf("missing %v", d.Missing())
	}
}

func detection(harnesses ...string) Detection {
	d := Detection{Tmux: Tool{Name: "tmux", Path: "/bin/tmux"}, Git: Tool{Name: "git", Path: "/bin/git"}}
	for _, h := range registry.Harnesses {
		t := Tool{Name: h}
		if slices.Contains(harnesses, h) {
			t.Path, t.Version = "/bin/"+h, h+" 1.0"
		}
		d.Harnesses = append(d.Harnesses, t)
	}
	return d
}

func profile(c Choices, name string) Profile {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p
		}
	}
	return Profile{}
}

func TestDefaultsAllHarnesses(t *testing.T) {
	d := detection("claude", "codex", "opencode")
	d.Repo = "/dev/My App!"
	b := Base{ConfigDir: "/cfg", Home: "/home", Detect: d}
	c := b.Defaults()
	var names []string
	for _, p := range c.Active() {
		names = append(names, p.Name)
	}
	want := []string{"claude-opus", "claude-sonnet", "claude-haiku", "codex", "codex-medium", "opencode"}
	if !slices.Equal(names, want) {
		t.Fatalf("profiles %v", names)
	}
	for name, fb := range map[string][]string{
		"claude-opus":   {"codex", "opencode", "claude-sonnet"},
		"claude-sonnet": {"codex-medium", "opencode", "claude-haiku"},
		"claude-haiku":  {"codex-medium", "opencode"},
		"codex":         {"claude-opus", "opencode", "codex-medium"},
		"codex-medium":  {"claude-sonnet", "opencode"},
		"opencode":      {"claude-sonnet", "codex-medium"},
	} {
		if got := profile(c, name).Fallback; !slices.Equal(got, fb) {
			t.Errorf("%s falls back to %v, want %v", name, got, fb)
		}
	}
	if p := profile(c, "claude-opus"); p.Model != "opus" || p.Effort != "high" {
		t.Errorf("claude-opus %+v", p)
	}
	if p := profile(c, "codex"); p.Model != "" || p.Effort != "high" {
		t.Errorf("codex %+v (no model: the harness default)", p)
	}
	if p := profile(c, "opencode"); p.Model != "" || p.Effort != "" {
		t.Errorf("opencode %+v", p)
	}
	p := c.Project
	if p == nil || p.Name != "my-app" || p.Prefix != "mya" || p.Path != "/dev/My App!" || p.Workspace != "worktree" ||
		p.Guard != "light" || !slices.Equal(p.Deny, []string{".env*", "*.pem"}) {
		t.Fatalf("project %+v", p)
	}
	if p.Coordinator != "claude-opus" || p.Default != "claude-sonnet" || p.Review != "codex" {
		t.Fatalf("project profiles %+v", p)
	}
	if !c.Daemon || c.Skill {
		t.Fatalf("daemon %v skill %v", c.Daemon, c.Skill)
	}
}

func TestDefaultsSingleHarness(t *testing.T) {
	b := Base{Detect: detection("claude")}
	c := b.Defaults()
	if fb := profile(c, "claude-opus").Fallback; !slices.Equal(fb, []string{"claude-sonnet"}) {
		t.Fatalf("claude-opus falls back to %v", fb)
	}
	coord, def, review := DefaultProfiles(b.Refs(c))
	if coord != "claude-opus" || def != "claude-sonnet" || review != "claude-opus" {
		t.Fatalf("claude only: %s %s %s", coord, def, review)
	}

	b = Base{Detect: detection("codex")}
	c = b.Defaults()
	coord, def, review = DefaultProfiles(b.Refs(c))
	if coord != "" || def != "codex-medium" || review != "codex" {
		t.Fatalf("codex only: %q %s %s (no Claude: no coordinator)", coord, def, review)
	}

	// Turning a harness off rewires the others.
	b = Base{Detect: detection("claude", "codex")}
	c = b.Defaults()
	c.Accounts[1].On = false
	c.Wire(b)
	if fb := profile(c, "claude-opus").Fallback; !slices.Equal(fb, []string{"claude-sonnet"}) {
		t.Fatalf("codex off: claude-opus falls back to %v", fb)
	}
	if fb := profile(c, "codex").Fallback; fb != nil {
		t.Fatalf("an inactive profile keeps fallbacks %v", fb)
	}
}

func TestExistingRegistry(t *testing.T) {
	existing := &registry.Config{
		Accounts: map[string]registry.Account{"claude": {Harness: "claude"}, "codex": {Harness: "opencode"}},
		Profiles: map[string]registry.Profile{
			"claude-opus": {Account: "claude", Tags: []string{"coordinator"}},
			"cheap":       {Account: "claude", Tags: []string{"fast"}},
		},
		Projects: []registry.Project{{Name: "app", Prefix: "app", Path: "/dev/app"}},
	}
	d := detection("claude", "codex", "opencode")
	d.Repo = "/dev/app"
	b := Base{Detect: d, Existing: existing, RegistryExists: true, DaemonInstalled: true,
		Known: map[string]string{"old": "cod"}}
	if got := b.Offered(); !slices.Equal(got, []string{"codex"}) {
		t.Fatalf("offered %v (claude and opencode have accounts)", got)
	}
	c := b.Defaults()
	if len(c.Accounts) != 1 || c.Accounts[0].Name != "codex-2" {
		t.Fatalf("accounts %+v (the name codex is taken)", c.Accounts)
	}
	if c.Project != nil || c.Daemon {
		t.Fatalf("project %+v daemon %v: the repository is registered, the daemon installed", c.Project, c.Daemon)
	}
	if fb := profile(c, "codex").Fallback; !slices.Equal(fb, []string{"claude-opus", "codex-medium"}) {
		t.Fatalf("codex falls back to %v", fb)
	}
	if got := b.UniquePrefix("codex"); got != "code" {
		t.Fatalf("prefix %q: cod is an old project's", got)
	}
	if got := b.UniquePrefix("old"); got != "cod" {
		t.Fatalf("prefix %q: a project added again keeps its old prefix", got)
	}
	if got := b.UniquePrefix("app"); got != "app" {
		t.Fatalf("prefix %q for app itself", got)
	}
	if got := b.UniquePrefix("apple"); got != "appl" {
		t.Fatalf("prefix %q for apple (app is taken)", got)
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{"My App!": "my-app", "--x--": "x", "ÜBER": "ber", "...": "project", "a.b_c": "a.b_c"} {
		if got := SanitizeName(in); got != want || !ValidName(got) {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	b := Base{}
	for in, want := range map[string]string{"otag": "otag", "myapp": "mya", "9lives": "liv", "x": "x", "a-b-c-d": "abcd", "projectx": "pro", "123": "p"} {
		if got := b.UniquePrefix(in); got != want || !ValidPrefix(got) {
			t.Errorf("UniquePrefix(%q) = %q, want %q", in, got, want)
		}
	}
	taken := Base{Known: map[string]string{}}
	for _, p := range []string{"abc", "abcd", "abcde", "abcdef"} {
		taken.Known["x"+p] = p
	}
	if got := taken.UniquePrefix("abcdef"); got != "abc2" {
		t.Fatalf("all taken: %q", got)
	}
}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func getenv(home string) func(string) string {
	return func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
}

// loadSync loads dir as `otag sync` does: no warnings, then a sync.
func loadSync(t *testing.T, d *db.DB, dir, home string) *registry.Config {
	t.Helper()
	cfg, err := registry.Load(dir, getenv(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) > 0 {
		t.Fatalf("generated files produce warnings: %q", cfg.Warnings)
	}
	err = d.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := registry.Sync(context.Background(), tx, cfg, "test")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The generated files load warning-free and sync, fresh and appended to.
func TestRenderLoadsAndSyncs(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "dev", "My App")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "cfg")
	d := detection("claude", "codex")
	d.Repo = repo
	b := Base{ConfigDir: dir, Home: home, Detect: d}
	files, err := b.Files(b.Defaults(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Action != Create || files[1].Path != ProjectFile(dir, "my-app") {
		t.Fatalf("files %+v", files)
	}
	if _, err := Preflight(dir, files, getenv(home)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("Preflight wrote into the config dir")
	}
	if err := Write(files); err != nil {
		t.Fatal(err)
	}
	if proj := mustRead(t, files[1].Path); !strings.Contains(proj, `path = "~/dev/My App"`) {
		t.Fatalf("project path not under ~:\n%s", proj)
	}
	database := openDB(t)
	cfg := loadSync(t, database, dir, home)
	if len(cfg.Accounts) != 2 || len(cfg.Profiles) != 5 || len(cfg.Projects) != 1 {
		t.Fatalf("loaded %d accounts, %d profiles, %d projects", len(cfg.Accounts), len(cfg.Profiles), len(cfg.Projects))
	}
	for _, a := range cfg.Accounts {
		if a.ConfigDir != "" {
			t.Fatalf("account with config_dir %+v", a)
		}
	}
	if p := cfg.Profiles["claude-opus"]; p.Model != "opus" || p.Effort != "high" ||
		!slices.Equal(p.Fallback, []string{"codex", "claude-sonnet"}) {
		t.Fatalf("claude-opus %+v", p)
	}
	if p := cfg.Projects[0]; p.Path != repo || p.CoordinatorProfile != "claude-opus" || p.ReviewProfile != "codex" {
		t.Fatalf("project %+v", p)
	}

	// OpenCode installed later: init appends its account and profile and keeps the
	// user's text (an edit, no final newline) byte for byte.
	reg := RegistryFile(dir)
	edited := strings.TrimSuffix(strings.Replace(mustRead(t, reg), "# Default worker.", "# my own note\n# Default worker.", 1), "\n")
	if err := os.WriteFile(reg, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	existing, exists, err := LoadExisting(dir, getenv(home))
	if err != nil || !exists {
		t.Fatal(exists, err)
	}
	d2 := detection("claude", "codex", "opencode")
	d2.Repo = repo
	b2 := Base{ConfigDir: dir, Home: home, Detect: d2, Existing: existing, RegistryExists: true}
	c2 := b2.Defaults()
	if len(c2.Accounts) != 1 || c2.Accounts[0].Harness != "opencode" || c2.Project != nil {
		t.Fatalf("choices %+v (only opencode is new; the repository is registered)", c2)
	}
	files, err = b2.Files(c2, now)
	if err != nil || len(files) != 1 || files[0].Action != Append {
		t.Fatalf("files %+v %v", files, err)
	}
	if !strings.HasPrefix(string(files[0].Content), edited+"\n") {
		t.Fatal("the appended file does not start with the user's text")
	}
	if err := Write(files); err != nil {
		t.Fatal(err)
	}
	cfg = loadSync(t, database, dir, home)
	if fb := cfg.Profiles["opencode"].Fallback; !slices.Equal(fb, []string{"claude-sonnet", "codex-medium"}) {
		t.Fatalf("appended opencode falls back to %v", fb)
	}
	if fb := cfg.Profiles["claude-opus"].Fallback; !slices.Equal(fb, []string{"codex", "claude-sonnet"}) {
		t.Fatalf("an existing profile changed: %v", fb)
	}
	if !strings.Contains(mustRead(t, reg), "# my own note") {
		t.Fatal("the user's edit is gone")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestForceBacksUp(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "cfg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "# mine\n[accounts.c]\nharness = \"claude\"\n"
	if err := os.WriteFile(RegistryFile(dir), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	b := Base{ConfigDir: dir, Home: home, Detect: detection("claude"), RegistryExists: true, Force: true,
		Existing: &registry.Config{}}
	files, err := b.Files(b.Defaults(), now)
	if err != nil {
		t.Fatal(err)
	}
	f := files[0]
	if f.Action != Replace || f.Backup != RegistryFile(dir)+".bak-20261015-123000" {
		t.Fatalf("file %+v", f)
	}
	if err := Write(files); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, f.Backup); got != old {
		t.Fatalf("backup %q", got)
	}
	if got := mustRead(t, f.Path); !strings.Contains(got, "[profiles.claude-opus]") {
		t.Fatalf("new registry:\n%s", got)
	}
	// Writing the same files again is refused: registry.toml is no longer the file they were computed from.
	if err := Write(files); err == nil {
		t.Fatal("a stale change was written")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestChecks(t *testing.T) {
	d := detection("claude")
	b := Base{ConfigDir: t.TempDir(), Detect: d, Existing: &registry.Config{
		Projects: []registry.Project{{Name: "taken", Prefix: "tk", Path: "/dev/taken"}}}}
	c := b.Defaults()
	ok := Project{Name: "new", Path: "/dev/new", Prefix: "nw", Workspace: "worktree", Guard: "light", Default: "claude-sonnet",
		BranchPrefix: "feature/", Push: "coordinator"}
	if err := b.CheckProject(ok, c); err != nil {
		t.Fatal(err)
	}
	for _, good := range []func(p *Project){
		func(p *Project) { p.BranchPrefix = "" },
		func(p *Project) {
			p.BranchPrefix, p.Base, p.Remote, p.Push = "team/x-", "origin/dev", "upstream", "child"
		},
		func(p *Project) { p.Push = "none" },
	} {
		p := ok
		good(&p)
		if err := b.CheckProject(p, c); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for _, bad := range []func(p *Project){
		func(p *Project) { p.Name = "Bad Name" },
		func(p *Project) { p.Name = "taken" },
		func(p *Project) { p.Prefix = "tk" },
		func(p *Project) { p.Prefix = "1x" },
		func(p *Project) { p.Path = "rel" },
		func(p *Project) { p.Path = "/dev/taken" },
		func(p *Project) { p.Guard = "loose" },
		func(p *Project) { p.Workspace = "tmp" },
		func(p *Project) { p.Review = "nope" },
		func(p *Project) { p.BranchPrefix = "feature//" },
		func(p *Project) { p.BranchPrefix = "-x/" },
		func(p *Project) { p.Base = "--upload-pack=x" },
		func(p *Project) { p.Base = "a..b" },
		func(p *Project) { p.Remote = "origin/x" },
		func(p *Project) { p.Push = "everyone" },
		func(p *Project) { p.Push = "" },
	} {
		p := ok
		bad(&p)
		if err := b.CheckProject(p, c); err == nil {
			t.Errorf("%+v passed", p)
		}
	}
	// Nothing chosen on a fresh machine: refused.
	for i := range c.Profiles {
		c.Profiles[i].On = false
	}
	if _, err := (Base{ConfigDir: b.ConfigDir}).Files(c, now); err == nil {
		t.Fatal("a registry without profiles")
	}
	// Preflight refuses what sync would refuse, and writes nothing.
	bad := []File{{Path: RegistryFile(b.ConfigDir), Action: Create, Content: []byte("[profiles.x]\naccount = \"none\"\n")}}
	if _, err := Preflight(b.ConfigDir, bad, getenv(t.TempDir())); err == nil || strings.Contains(err.Error(), "otag-setup-") {
		t.Fatalf("preflight: %v", err)
	}
}
