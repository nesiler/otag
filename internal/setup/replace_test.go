package setup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// appendFiles returns the append of codex to an existing registry and the path.
func appendFiles(t *testing.T) ([]File, string) {
	t.Helper()
	b, c, reg := existingSetup(t)
	files, err := b.Files(c, now)
	if err != nil || len(files) != 1 || files[0].Action != Append {
		t.Fatalf("files %+v %v", files, err)
	}
	return files, reg
}

func setHook(t *testing.T, fn func(step, path string)) {
	t.Helper()
	writeHook = fn
	t.Cleanup(func() { writeHook = nil })
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "registry.toml" {
			out = append(out, e.Name())
		}
	}
	return out
}

// A save right before the file is moved aside: the moved file is not the expected
// one, so it goes back and nothing is written.
func TestReplaceSaveBeforeMoveAside(t *testing.T) {
	files, reg := appendFiles(t)
	saved := mustRead(t, reg) + "# saved just before\n"
	setHook(t, func(step, _ string) {
		if step == "aside" {
			if err := os.WriteFile(reg, []byte(saved), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	wantConflict(t, "write", Write(files))
	if got := mustRead(t, reg); got != saved {
		t.Fatalf("the save is gone:\n%s", got)
	}
	if l := leftovers(t, filepath.Dir(reg)); len(l) != 0 {
		t.Fatalf("left behind: %v", l)
	}
}

// A save after the file was moved aside recreates it: the new content is not placed
// over it, and the version before the save stays aside, named in the error.
func TestReplaceSaveAfterMoveAside(t *testing.T) {
	files, reg := appendFiles(t)
	before := mustRead(t, reg)
	setHook(t, func(step, _ string) {
		if step == "place" {
			if err := os.WriteFile(reg, []byte("# the editor's save\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	err := Write(files)
	wantConflict(t, "write", err)
	if got := mustRead(t, reg); got != "# the editor's save\n" {
		t.Fatalf("the save was replaced:\n%s", got)
	}
	l := leftovers(t, filepath.Dir(reg))
	if len(l) != 1 || !strings.Contains(err.Error(), l[0]) {
		t.Fatalf("aside %v, error %v", l, err)
	}
	if got := mustRead(t, filepath.Join(filepath.Dir(reg), l[0])); got != before {
		t.Fatalf("the version before the save is lost: %q", got)
	}
}

// Without interference the aside copy of an append is removed (a replace keeps it as
// the backup: TestUniqueNames).
func TestReplaceCleansUp(t *testing.T) {
	files, reg := appendFiles(t)
	if err := Write(files); err != nil {
		t.Fatal(err)
	}
	if l := leftovers(t, filepath.Dir(reg)); len(l) != 0 {
		t.Fatalf("left behind: %v", l)
	}
	if !strings.Contains(mustRead(t, reg), "[accounts.codex]") {
		t.Fatal("not appended")
	}
}

// A repository the database knows (a removed project) gets its old name and prefix
// back; a known name of another repository is not reused.
func TestProjectNameFromTheDatabase(t *testing.T) {
	b := Base{Known: map[string]string{"shop": "sh", "web": "web"},
		KnownPaths: map[string]string{"/dev/shop": "shop", "/old/web": "web"}}
	p := b.NewProject("/dev/shop", Choices{})
	if p.Name != "shop" || p.Prefix != "sh" || !p.AutoName || !p.AutoPrefix {
		t.Fatalf("re-added: %+v", p)
	}
	if p := b.NewProject("/new/web", Choices{}); p.Name != "web-2" {
		t.Fatalf("another repository named web: %+v", p)
	}
	stale := Project{Path: "/dev/shop", Name: "x", Prefix: "x", AutoName: true, AutoPrefix: true}
	b.Refresh(&stale)
	if stale.Name != "shop" || stale.Prefix != "sh" {
		t.Fatalf("refreshed %+v", stale)
	}
	mine := Project{Path: "/dev/shop", Name: "mine", Prefix: "mn"}
	b.Refresh(&mine)
	if mine.Name != "mine" || mine.Prefix != "mn" {
		t.Fatalf("the user's choice changed: %+v", mine)
	}
}

// The no-replace rename is a rename, not a hard link: it moves directories (which
// cannot be hard-linked) and never replaces a target.
func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(a, b); err != nil {
		t.Fatalf("a directory: %v (hard links only?)", err)
	}
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(a, b); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("over an existing target: %v", err)
	}
	if mustRead(t, a) != "a" {
		t.Fatal("the source moved")
	}
	if err := linkRename(a, filepath.Join(dir, "c")); err != nil || mustRead(t, filepath.Join(dir, "c")) != "a" {
		t.Fatalf("the fallback: %v", err)
	}
	if err := linkRename(filepath.Join(dir, "c"), b); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("the fallback over a target: %v", err)
	}
}
