package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsTemp(t *testing.T) {
	for name, want := range map[string]bool{
		".registry.toml.otag-tmp-123456":        true,
		".web.toml.otag-tmp-9":                  true,
		".web.otag-tmp-prod.toml.otag-removing": false,
		".registry.toml.otag-old":               false,
		".web.otag-tmp-prod.toml.otag-tmp-1":    true,
		".web.otag-tmp-prod.toml":               false,
		".registry.toml.otag-tmp-":              false,
		".registry.toml.otag-tmp-12ab":          false,
		"registry.toml.otag-tmp-1":              false,
	} {
		if got := isTemp(name); got != want {
			t.Errorf("isTemp(%q) = %v, want %v", name, got, want)
		}
	}
}

// A project whose name contains the temp mark: an interrupted removal with both versions
// present must keep both files, not clean the aside copy up as a temp file.
func TestRecoverKeepsLeftoverWithTempMarkInName(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(projects, "web.otag-tmp-prod.toml")
	aside := filepath.Join(projects, ".web.otag-tmp-prod.toml"+removingMark)
	for _, p := range []string{orig, aside} {
		if err := os.WriteFile(p, []byte("path = \"/x\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Recover(dir); err == nil {
		t.Fatal("Recover with two versions: want CONFLICT")
	}
	for _, p := range []string{orig, aside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}
