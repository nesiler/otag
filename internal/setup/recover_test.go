package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crash runs fn and swallows the panic a write hook raises to stop a write midway,
// as a crash (or kill -9) would.
func crash(t *testing.T, step string, fn func()) {
	t.Helper()
	setHook(t, func(s, _ string) {
		if s == step {
			panic("crash")
		}
	})
	defer func() {
		if recover() == nil {
			t.Fatalf("no crash at %s", step)
		}
		writeHook = nil
	}()
	fn()
}

// A crash between moving registry.toml aside and putting the new one in place leaves
// only the aside copy: Leftovers finds it and Recover puts it back.
func TestRecoverAfterACrash(t *testing.T) {
	files, reg := appendFiles(t)
	before := mustRead(t, reg)
	crash(t, "place", func() { _ = Write(files) })
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Fatal("registry.toml is still there")
	}
	dir := filepath.Dir(reg)
	ls, err := Leftovers(dir)
	if err != nil || len(ls) != 1 || ls[0].Original != reg || ls[0].Aside != AsideName(reg) || ls[0].Both {
		t.Fatalf("leftovers %+v %v", ls, err)
	}
	stray := filepath.Join(dir, ".registry.toml"+tempMark+"123")
	if err := os.WriteFile(stray, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes, err := Recover(dir)
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "restored "+reg) {
		t.Fatalf("recover: %q %v", notes, err)
	}
	if mustRead(t, reg) != before {
		t.Fatal("not the old registry")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("left behind: %v", l)
	}
	// Then the change goes through.
	if err := Write(files); err != nil {
		t.Fatal(err)
	}
}

// A crash after the new file is in place but before the aside copy is gone leaves
// both: Recover touches neither and refuses, naming both.
func TestRecoverRefusesTwoVersions(t *testing.T) {
	files, reg := appendFiles(t)
	crash(t, "placed", func() { _ = Write(files) })
	aside := AsideName(reg)
	newer, older := mustRead(t, reg), mustRead(t, aside)
	notes, err := Recover(filepath.Dir(reg))
	wantConflict(t, "recover", err)
	if len(notes) != 0 || !strings.Contains(err.Error(), aside) || !strings.Contains(err.Error(), reg) {
		t.Fatalf("notes %q, error %v", notes, err)
	}
	if mustRead(t, reg) != newer || mustRead(t, aside) != older {
		t.Fatal("a version was touched")
	}
	// Writing refuses too, as long as both are there.
	wantConflict(t, "write", Write(files))
	if mustRead(t, reg) != newer || mustRead(t, aside) != older {
		t.Fatal("a version was touched by the write")
	}
}

// A project file `otag project remove` left aside comes back.
func TestRecoverProjectFile(t *testing.T) {
	dir := t.TempDir()
	proj := ProjectFile(dir, "web")
	if err := os.MkdirAll(filepath.Dir(proj), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RemovingName(proj), []byte("prefix = \"web\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notes, err := Recover(dir)
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "otag project remove") {
		t.Fatalf("recover: %q %v", notes, err)
	}
	if mustRead(t, proj) != "prefix = \"web\"\n" {
		t.Fatal("not restored")
	}
}
