package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShippedExamples loads examples/ the way `otag sync` does: the files must be valid,
// warning-free (no unknown or misspelled keys) and sync into a database.
func TestShippedExamples(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "dev", "projects", "myapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join("..", "..", "examples", "registry.toml"), filepath.Join(dir, "registry.toml"))
	examples, err := filepath.Glob(filepath.Join("..", "..", "examples", "projects", "*.toml"))
	if err != nil || len(examples) == 0 {
		t.Fatalf("no project examples: %v", err)
	}
	for _, f := range examples {
		copyFile(t, f, filepath.Join(dir, "projects", filepath.Base(f)))
	}

	cfg, err := Load(dir, getenv(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) > 0 {
		t.Fatalf("examples produce warnings: %q", cfg.Warnings)
	}
	if len(cfg.Accounts) == 0 || len(cfg.Profiles) == 0 || len(cfg.Projects) == 0 {
		t.Fatalf("examples are incomplete: %d accounts, %d profiles, %d projects",
			len(cfg.Accounts), len(cfg.Profiles), len(cfg.Projects))
	}
	for _, a := range []string{"claude", "codex", "opencode", "fake"} {
		if _, ok := cfg.Accounts[a]; !ok {
			t.Errorf("example registry lacks account %q", a)
		}
	}
	sync(t, openDB(t), dir)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
