package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/wizard"
)

// The wizard's proposal comes from a look at the database that may be stale. Apply
// chooses the automatic name and prefix again from the database it then holds
// writable: a removed project gets its historical name and prefix back. A prefix the
// user chose stays, and a clash is still refused.
func TestApplyRefreshesAutomaticNameAndPrefix(t *testing.T) {
	f := newFresh(t, "claude")
	f.ok("init", "--yes", "--no-daemon") // my-app, prefix mya
	f.ok("project", "remove", "my-app")
	f.terminal = true
	var got *setup.Project
	f.ui = &uiHooks{wizard: func(ctx context.Context, o wizard.Options) (wizard.Result, error) {
		c := o.Choices
		p := *c.Project
		p.Name, p.Prefix = "stale", "stl" // what a stale look would propose; still automatic
		c.Project = &p
		_, err := o.Apply(ctx, c)
		return wizard.Result{Applied: err == nil, Err: errString(err)}, nil
	}}
	f.ok("init", "--no-daemon")
	if _, err := os.Stat(filepath.Join(f.cfg, "projects", "my-app.toml")); err != nil {
		t.Fatalf("the project did not get its old name back: %v", err)
	}
	if !strings.Contains(f.read("projects/my-app.toml"), `prefix = "mya"`) {
		t.Fatalf("project file:\n%s", f.read("projects/my-app.toml"))
	}

	// A prefix the user typed is kept, and one of another project is refused.
	g := newFresh(t, "claude")
	g.ok("init", "--yes", "--no-daemon")
	g.ok("project", "remove", "my-app")
	g.terminal = true
	g.ui = &uiHooks{wizard: func(ctx context.Context, o wizard.Options) (wizard.Result, error) {
		c := o.Choices
		p := *c.Project
		p.Name, p.AutoName = "other", false
		p.Prefix, p.AutoPrefix = "mya", false // my-app's (retired) prefix
		c.Project = &p
		got = &p
		_, err := o.Apply(ctx, c)
		return wizard.Result{Applied: err == nil, Err: errString(err)}, nil
	}}
	code, _, errs := g.run("init", "--no-daemon")
	if code == 0 || !strings.Contains(errs, `prefix "mya" is used by another project`) || got == nil {
		t.Fatalf("init: %d %s", code, errs)
	}
}

// otag sync waits for the config lock: it never reads the config in the middle of a
// change by init or project.
func TestSyncWaitsForTheConfigLock(t *testing.T) {
	f := newFresh(t, "claude")
	f.ok("init", "--yes", "--no-daemon")
	unlock, err := setup.LockConfig(f.cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int)
	go func() { code, _, _ := f.run("sync"); done <- code }()
	select {
	case <-done:
		t.Fatal("sync ran while the config was locked")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	if code := <-done; code != 0 {
		t.Fatalf("sync after the lock: exit %d", code)
	}
}

// A sync started while project remove has the file moved aside waits, and then sees
// the restored file: the project is not retired with its live run.
func TestSyncDuringProjectRemove(t *testing.T) {
	h := newRunHarness(t)
	id := h.addTask("beta", "keep going")
	done := make(chan int, 1)
	beforeRetire = func() {
		go func() { code, _, _ := h.run("sync"); done <- code }()
		time.Sleep(300 * time.Millisecond) // a sync without the lock would run now
		h.start(id, "--profile", "manual")
	}
	t.Cleanup(func() { beforeRetire = nil })
	code, e := h.json("project", "remove", "beta")
	wantCode(t, "remove with a run started meanwhile", code, e, "CONFLICT")
	if code := <-done; code != 0 {
		t.Fatalf("sync: exit %d", code)
	}
	if n := h.count(`SELECT count(*) FROM projects WHERE name = 'beta' AND retired_at IS NULL`); n != 1 {
		t.Fatal("a concurrent sync retired beta while it had a live run")
	}
}
