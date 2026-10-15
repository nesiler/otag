package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/setup"
)

// A crash left registry.toml moved aside: doctor says so (not "not set up"), and the
// next config command puts it back instead of taking the machine for a fresh one.
func TestInterruptedChangeIsRestored(t *testing.T) {
	f := newFresh(t, "claude")
	f.cwd = f.root
	f.ok("init", "--yes", "--no-daemon")
	reg := filepath.Join(f.cfg, "registry.toml")
	before := f.read("registry.toml") + "# custom\n"
	if err := os.WriteFile(reg, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	f.ok("sync")
	if err := os.Rename(reg, setup.AsideName(reg)); err != nil { // the crash
		t.Fatal(err)
	}
	_, e := f.json("doctor")
	cs := checksOf(t, e.Data)
	if c := cs["interrupted change"]; c.Status != "fail" || !strings.HasPrefix(c.Fix, "otag sync") {
		t.Fatalf("doctor %+v", c)
	}
	if c, ok := cs["registry"]; ok && strings.Contains(c.Detail, "not set up") {
		t.Fatalf("doctor calls it a fresh machine: %+v", c)
	}
	code, out, errs := f.run("init", "--yes", "--no-daemon")
	if code != 0 || !strings.Contains(errs, "restored "+reg) || !strings.Contains(out, "nothing to add") {
		t.Fatalf("init: %d %s %s", code, out, errs)
	}
	if f.read("registry.toml") != before {
		t.Fatal("the custom registry is gone")
	}

	// sync restores it too.
	if err := os.Rename(reg, setup.AsideName(reg)); err != nil {
		t.Fatal(err)
	}
	if _, _, errs := f.run("sync"); !strings.Contains(errs, "restored") || f.read("registry.toml") != before {
		t.Fatalf("sync: %s", errs)
	}
}

// Both versions there: every config-writing command refuses and touches neither;
// doctor fails with the fix.
func TestTwoVersionsBlockChanges(t *testing.T) {
	f := newFresh(t, "claude")
	f.ok("init", "--yes", "--no-daemon")
	reg := filepath.Join(f.cfg, "registry.toml")
	cur := f.read("registry.toml")
	if err := os.WriteFile(setup.AsideName(reg), []byte(cur+"# older\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.stub("codex")
	for _, args := range [][]string{{"sync"}, {"init", "--yes", "--no-daemon"}, {"project", "remove", "my-app"},
		{"project", "add", f.repo, "--yes", "--name", "x"}} {
		code, e := f.json(args...)
		wantCode(t, strings.Join(args, " "), code, e, "CONFLICT")
		if !strings.Contains(e.Error.Message, setup.AsideName(reg)) {
			t.Fatalf("%v: %s", args, e.Error.Message)
		}
	}
	if f.read("registry.toml") != cur {
		t.Fatal("registry.toml was touched")
	}
	if b, _ := os.ReadFile(setup.AsideName(reg)); string(b) != cur+"# older\n" {
		t.Fatal("the aside copy was touched")
	}
	_, e := f.json("doctor")
	if c := checksOf(t, e.Data)["interrupted change"]; c.Status != "fail" || !strings.Contains(c.Fix, "delete the other") {
		t.Fatalf("doctor %+v", c)
	}
}

// sync takes the lock before it looks at the config: during the instant init has no
// registry.toml it waits instead of failing or syncing half a change.
func TestSyncLocksBeforeLooking(t *testing.T) {
	f := newFresh(t, "claude")
	f.ok("init", "--yes", "--no-daemon")
	reg := filepath.Join(f.cfg, "registry.toml")
	unlock, err := setup.LockConfig(f.cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gap := filepath.Join(f.root, "in-flight")
	if err := os.Rename(reg, gap); err != nil { // a change in progress
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { code, _, _ := f.run("sync"); done <- code }()
	time.Sleep(300 * time.Millisecond)
	if err := os.Rename(gap, reg); err != nil {
		t.Fatal(err)
	}
	unlock()
	if code := <-done; code != 0 {
		t.Fatalf("sync during a change: exit %d", code)
	}
}

// A crash during project remove (file aside, project not yet retired) is undone by
// the next config command: the file comes back, the project stays.
func TestInterruptedRemoveIsRestored(t *testing.T) {
	f := newFresh(t, "claude")
	f.ok("init", "--yes", "--no-daemon")
	beforeRetire = func() { panic("crash") }
	t.Cleanup(func() { beforeRetire = nil })
	func() {
		defer func() { _ = recover() }()
		f.run("project", "remove", "my-app")
	}()
	beforeRetire = nil
	file := filepath.Join(f.cfg, "projects", "my-app.toml")
	if _, err := os.Stat(setup.RemovingName(file)); err != nil {
		t.Fatalf("no leftover: %v", err)
	}
	if _, _, errs := f.run("sync"); !strings.Contains(errs, "otag project remove") {
		t.Fatalf("sync did not say what it restored: %s", errs)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("not restored: %v", err)
	}
	if out := f.ok("status"); !strings.Contains(out, "my-app") {
		t.Fatalf("the project was retired:\n%s", out)
	}
	// And a removal still works afterwards.
	f.ok("project", "remove", "my-app")
	if kept, _ := filepath.Glob(file + ".removed-*"); len(kept) != 1 {
		t.Fatalf("kept %v", kept)
	}
}
