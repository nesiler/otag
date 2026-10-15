package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// Relative overrides become absolute, so children in other directories agree.
func TestAbsolutePaths(t *testing.T) {
	cwd, _ := os.Getwd()
	env := map[string]string{"OTAG_DB": "rel/otag.db", "OTAG_CONFIG_DIR": "cfg", "OTAG_BIN": "./bin/otag", "HOME": "/h"}
	getenv := func(k string) string { return env[k] }
	if got := DB(getenv); got != filepath.Join(cwd, "rel/otag.db") {
		t.Errorf("DB = %s", got)
	}
	if got := DataDir(getenv); got != filepath.Join(cwd, "rel") {
		t.Errorf("DataDir = %s", got)
	}
	if got := ConfigDir(getenv); got != filepath.Join(cwd, "cfg") {
		t.Errorf("ConfigDir = %s", got)
	}
	if got := OtagBin(getenv); got != filepath.Join(cwd, "bin/otag") {
		t.Errorf("OtagBin = %s", got)
	}
	env["OTAG_DATA_DIR"] = "~/data"
	if got := DataDir(getenv); got != "/h/data" {
		t.Errorf("DataDir ~ = %s", got)
	}
}

// Within is containment with symlinks resolved and case ignored, also for paths that do
// not exist yet.
func TestWithin(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "data", "worktrees")
	_ = os.MkdirAll(filepath.Join(wt, "p", "p-1"), 0o755)
	_ = os.Symlink(filepath.Join(wt, "p"), filepath.Join(root, "alias"))
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{wt, true},
		{filepath.Join(wt, "p", "p-1"), true},
		{filepath.Join(wt, "p", "p-1", "not", "yet"), true},
		{filepath.Join(root, "alias", "p-1"), true},
		{filepath.Join(root, "data", "WORKTREES", "p"), true},
		{filepath.Join(root, "data"), false},
		{filepath.Join(root, "data", "worktrees-other"), false},
		{"", false},
	} {
		if got := Within(c.dir, wt); got != c.want {
			t.Errorf("Within(%s) = %v", c.dir, got)
		}
	}
}
