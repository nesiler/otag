package setup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/registry"
)

// existingSetup writes a registry with a claude account and returns a Base that
// read it (as setupBase does) and the choices adding codex.
func existingSetup(t *testing.T) (Base, Choices, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "cfg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := RegistryFile(dir)
	if err := os.WriteFile(reg, []byte("[accounts.claude]\nharness = \"claude\"\n[profiles.mine]\naccount = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadExisting(dir, getenv(home))
	if err != nil {
		t.Fatal(err)
	}
	b := Base{ConfigDir: dir, Home: home, Detect: detection("claude", "codex"), Existing: cfg, RegistryExists: true,
		RegistrySum: Sum([]byte(mustRead(t, reg)))}
	return b, b.Defaults(), reg
}

func wantConflict(t *testing.T, what string, err error) {
	t.Helper()
	if e, ok := apperr.As(err); !ok || e.Code != apperr.Conflict {
		t.Fatalf("%s: %v, want CONFLICT", what, err)
	}
}

// An edit made after setup read the registry is never overwritten: Files refuses
// when the file is not the one the setup started from, Write when it changed after
// Files.
func TestWriteRefusesAChangedRegistry(t *testing.T) {
	b, c, reg := existingSetup(t)
	edited := mustRead(t, reg) + "# edited in an editor\n"

	if err := os.WriteFile(reg, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := b.Files(c, now)
	wantConflict(t, "Files after an edit", err)

	b.RegistrySum = Sum([]byte(edited))
	files, err := b.Files(c, now)
	if err != nil || files[0].Action != Append || files[0].Base != b.RegistrySum {
		t.Fatalf("files %+v %v", files, err)
	}
	again := edited + "# saved once more\n"
	if err := os.WriteFile(reg, []byte(again), 0o644); err != nil {
		t.Fatal(err)
	}
	wantConflict(t, "Write after an edit", Write(files))
	if got := mustRead(t, reg); got != again {
		t.Fatalf("the edit was overwritten:\n%s", got)
	}
}

// A file that appears between Files and Write (another setup) is not replaced.
func TestCreateNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	b := Base{ConfigDir: dir, Detect: detection("claude")}
	files, err := b.Files(b.Defaults(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0].Path, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantConflict(t, "Write over a new file", Write(files))
	if got := mustRead(t, files[0].Path); got != "theirs" {
		t.Fatalf("replaced: %q", got)
	}
	if err := CreateNew(files[0].Path, []byte("x"), 0o644); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CreateNew over a file: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

// Appending keeps the registry's mode; new files are 0644.
func TestWriteKeepsMode(t *testing.T) {
	b, _, reg := existingSetup(t)
	if err := os.Chmod(reg, 0o600); err != nil {
		t.Fatal(err)
	}
	d := detection("claude", "codex")
	d.Repo = b.Home
	b.Detect = d
	c := b.Defaults()
	files, err := b.Files(c, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(files); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{reg: 0o600, files[1].Path: 0o644} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: mode %v, want %v (%v)", path, st.Mode().Perm(), want, err)
		}
	}
}

// Backups and removed project files get unique names and never overwrite one.
func TestUniqueNames(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "registry.toml.bak-20261015-123000")
	for i, want := range []string{base, base + "-2", base + "-3"} {
		got, err := CreateUnique(base, []byte{byte('a' + i)}, 0o600)
		if err != nil || got != want {
			t.Fatalf("CreateUnique %d: %s %v", i, got, err)
		}
	}
	if mustRead(t, base) != "a" || mustRead(t, base+"-2") != "b" {
		t.Fatal("an earlier backup was overwritten")
	}
	src := filepath.Join(dir, "p.toml")
	for i, want := range []string{"p.toml.removed", "p.toml.removed-2"} {
		if err := os.WriteFile(src, []byte{byte('x' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := RenameUnique(src, filepath.Join(dir, "p.toml.removed"))
		if err != nil || got != filepath.Join(dir, want) {
			t.Fatalf("RenameUnique %d: %s %v", i, got, err)
		}
	}
	if mustRead(t, filepath.Join(dir, "p.toml.removed")) != "x" {
		t.Fatal("the first removed file was overwritten")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("the source is still there")
	}

	// Two --force runs in the same second keep both old registries.
	home := t.TempDir()
	cfg := filepath.Join(home, "cfg")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		old := []byte("# version " + string(rune('1'+i)) + "\n")
		if err := os.WriteFile(RegistryFile(cfg), old, 0o644); err != nil {
			t.Fatal(err)
		}
		b := Base{ConfigDir: cfg, Home: home, Detect: detection("claude"), RegistryExists: true, Force: true,
			Existing: &registry.Config{}, RegistrySum: Sum(old)}
		files, err := b.Files(b.Defaults(), now)
		if err != nil {
			t.Fatal(err)
		}
		if err := Write(files); err != nil {
			t.Fatal(err)
		}
		want := RegistryFile(cfg) + ".bak-20261015-123000"
		if i == 1 {
			want += "-2"
		}
		if files[0].Backup != want || mustRead(t, want) != string(old) {
			t.Fatalf("run %d: backup %s", i, files[0].Backup)
		}
	}
}

func TestLockConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	unlock, err := LockConfig(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LockConfig(dir, 100*time.Millisecond)
	wantConflict(t, "a second lock", err)
	unlock()
	unlock2, err := LockConfig(dir, time.Second)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	unlock2()
}

// A login chosen in the environment ($CODEX_HOME, $CLAUDE_CONFIG_DIR) other than the
// default becomes the account's config_dir: the adapters scrub those variables.
func TestCustomConfigDir(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "codex work")
	bin := stubs(t, "claude", "codex", "tmux", "git")
	run := func(context.Context, string, ...string) (string, error) { return "1.0\n", nil }
	p := Prober{PATH: bin, Home: home, CodexHome: work, ClaudeConfigDir: filepath.Join(home, ".claude"), Run: run}
	d := p.Detect(context.Background(), "")
	if got := d.Harness("codex").CustomConfigDir; got != work {
		t.Fatalf("codex custom dir %q", got)
	}
	if got := d.Harness("claude").CustomConfigDir; got != "" {
		t.Fatalf("$CLAUDE_CONFIG_DIR = ~/.claude is the default, got %q", got)
	}
	if got := (Prober{Home: home, CodexHome: filepath.Join(home, ".codex")}).customDir("codex"); got != "" {
		t.Fatalf("$CODEX_HOME = ~/.codex is the default, got %q", got)
	}
	if got := (Prober{Home: home, ClaudeConfigDir: "/opt/claude-b"}).customDir("claude"); got != "/opt/claude-b" {
		t.Fatalf("claude custom dir %q", got)
	}

	dir := filepath.Join(home, "cfg")
	b := Base{ConfigDir: dir, Home: home, Detect: d}
	c := b.Defaults()
	files, err := b.Files(c, now)
	if err != nil {
		t.Fatal(err)
	}
	if body := string(files[0].Content); !strings.Contains(body, `config_dir = "~/codex work"`) ||
		strings.Count(body, "config_dir = ") != 1 {
		t.Fatalf("registry:\n%s", body)
	}
	if err := Write(files); err != nil {
		t.Fatal(err)
	}
	cfg := loadSync(t, openDB(t), dir, home)
	if got := cfg.Accounts["codex"].ConfigDir; got != work {
		t.Fatalf("loaded codex config_dir %q", got)
	}
	if got := cfg.Accounts["claude"].ConfigDir; got != "" {
		t.Fatalf("loaded claude config_dir %q", got)
	}
}
