package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/proc"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

// A server window that disappears, or a sidecar whose process exits, while the main
// pane lives is a crash of the run: otag lost the harness's server or its events.
func TestAuxWindowDeathCrashesRun(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.served]
account = "f"
args = ["start", "wait"]
env = { FAKE_SERVER = "1" }
[profiles.listened]
account = "f"
args = ["start", "wait"]
env = { FAKE_SIDECAR = "1" }
`)
	for _, c := range []struct{ profile, role, detail string }{
		{"served", "server", "server window server gone"},
		{"listened", "sidecar", "sidecar window listen exited"},
	} {
		tk := e.addTask(c.profile, c.profile)
		r := e.start(tk)
		e.waitFor("started", false, func() bool { s := e.get(r.ID).Status; return s == run.Idle || s == run.Running })
		aux := r.Session.AuxPanes[0]
		if aux.Role != c.role || aux.Birth == "" {
			t.Fatalf("aux %+v", aux)
		}
		if c.role == "server" {
			_ = e.m.Tmux.KillWindow(e.ctx, aux.Pane)
		} else {
			_ = syscall.Kill(aux.PID, syscall.SIGKILL)
		}
		e.waitFor("crash", true, func() bool { return e.get(r.ID).Status == run.Ended })
		got := e.get(r.ID)
		if got.EndedReason != run.ReasonCrashed || !strings.Contains(got.Error, c.detail) || got.TaskStatus != task.Ready {
			t.Fatalf("%s: %s %q task %s", c.profile, got.EndedReason, got.Error, got.TaskStatus)
		}
		if l := e.m.Tmux.Check(e.ctx, r.Session.TmuxPane, r.Session.PID); l.State != tmux.Gone {
			t.Fatalf("%s: main pane %+v", c.profile, l)
		}
	}
	if ns := e.notifications("run.crashed"); len(ns) != 2 || !strings.HasPrefix(ns[0].Body, "Server window") &&
		!strings.HasPrefix(ns[1].Body, "Server window") {
		t.Fatalf("notifications %+v", ns)
	}
}

// A run whose pane was recorded but which never got its first hook event (here made so
// by clearing it) ends launch_failed after launch_timeout, cause harness, and the
// daemon falls back to the next profile.
func TestFirstEventDeadline(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.quiet]
account = "f"
args = ["start", "wait"]
fallback = ["ok"]
[profiles.ok]
account = "g"
args = ["start", "wait"]
`)
	tk := e.addTask("quiet", "quiet")
	r := e.start(tk)
	e.waitFor("idle", false, func() bool { return e.get(r.ID).Status == run.Idle })
	e.exec(`UPDATE runs SET status = 'starting', first_activity_at = NULL, heartbeat_at = NULL, version = version + 1
		WHERE id = ?`, r.ID)
	e.d.Tick(e.ctx)
	if got := e.get(r.ID); got.Status != run.Starting {
		t.Fatalf("ended before launch_timeout: %s %s", got.Status, got.EndedReason)
	}
	e.clock = time.Now().Add(6 * time.Minute)
	e.d.Tick(e.ctx)
	got := e.get(r.ID)
	if got.EndedReason != run.ReasonLaunchFailed || !strings.Contains(got.Error, "no hook event") {
		t.Fatalf("after launch_timeout: %s %s %q", got.Status, got.EndedReason, got.Error)
	}
	var cause string
	_ = e.db.QueryRow(`SELECT json_extract(data, '$.cause') FROM events WHERE run_id = ? AND to_status = 'ended'`, r.ID).Scan(&cause)
	if cause != run.CauseHarness {
		t.Fatalf("cause %q", cause)
	}
	if r2 := e.get(tk + "-r2"); r2.Profile != "ok" || r2.RetryOf != r.ID {
		t.Fatalf("fallback %+v", r2)
	}
	if len(e.notifications("run.launch_failed")) != 1 {
		t.Fatal("no notification")
	}
}

// Windows tagged for a run of this database that ended or does not exist are killed
// after the grace period, with processes that ignore SIGHUP; windows of active runs,
// of another database and untagged ones are left alone. `otag run start` kills such a
// window of its own task at once, before it reuses the worktree.
func TestOrphanWindowsAreKilled(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "wait"]
`)
	e.d.Config.DeadGrace = time.Minute
	tk := e.addTask("slow", "slow")
	r := e.start(tk)
	window := func(name, runID, owner string) tmux.PaneRef {
		t.Helper()
		ref, err := e.m.Tmux.NewWindow(e.ctx, tmux.Window{Session: "otag-alpha", Name: name, Run: runID, Owner: owner,
			Argv: []string{"/bin/sh", "-c", `trap "" HUP; while :; do sleep 1; done`}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(-ref.PID, syscall.SIGKILL) })
		return ref
	}
	ghost := window("ghost", "al-ghost-r1", e.db.Path)
	foreign := window("foreign", "al-ghost-r1", "/elsewhere/otag.db")
	plain := window("plain", "", "")
	alive := func(ref tmux.PaneRef) bool { return e.m.Tmux.Check(e.ctx, ref.PaneID, ref.PID).State == tmux.Alive }

	e.d.Tick(e.ctx)
	if !alive(ghost) {
		t.Fatal("orphan killed within the grace period")
	}
	e.clock = e.clock.Add(2 * time.Minute)
	e.d.Tick(e.ctx)
	if alive(ghost) || tmux.ProcessAlive(ghost.PID) {
		t.Fatal("orphan window of an unknown run survived")
	}
	if !alive(foreign) || !alive(plain) || !alive(tmux.PaneRef{PaneID: r.Session.TmuxPane, PID: r.Session.PID}) {
		t.Fatal("killed a window that is not an orphan")
	}

	// The run ends; a window its launcher never recorded is still there. The next
	// start of the task kills it before it uses the worktree.
	if _, err := e.m.Stop(e.ctx, r.ID, "test"); err != nil {
		t.Fatal(err)
	}
	stray := window("stray", r.ID, e.db.Path)
	r2 := e.start(tk)
	if alive(stray) || tmux.ProcessAlive(stray.PID) {
		t.Fatalf("%s started next to a window of %s", r2.ID, r.ID)
	}
}

// A run that ended without its cleanup (the process that ended it died first) is
// cleaned up by the daemon: its window is killed and the obligation cleared.
func TestPendingCleanupIsFinished(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "wait"]
`)
	tk := e.addTask("slow", "slow")
	r := e.start(tk)
	e.waitFor("idle", false, func() bool { return e.get(r.ID).Status == run.Idle })
	e.exec(`UPDATE runs SET status = 'ended', ended_reason = 'done', ended_at = ?, cleanup_pending = 1,
		version = version + 1 WHERE id = ?`, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), r.ID)
	if got := e.get(r.ID); !got.CleanupPending {
		t.Fatal("not pending")
	}
	e.d.Tick(e.ctx)
	if l := e.m.Tmux.Check(e.ctx, r.Session.TmuxPane, r.Session.PID); l.State != tmux.Gone {
		t.Fatalf("pane %+v", l)
	}
	if got := e.get(r.ID); got.CleanupPending || got.CleanupError != "" {
		t.Fatalf("cleanup %v %q", got.CleanupPending, got.CleanupError)
	}
}

// A cleanup never touches a window or process that is not its run's, even when the
// recorded pane id and pid now match another run's (reuse): the window is tagged for
// the other run, and a recorded birth does not match.
func TestCleanupLeavesOtherRunsAlone(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "wait"]
`)
	live := e.start(e.addTask("live", "slow"))
	e.waitFor("idle", false, func() bool { return e.get(live.ID).Status == run.Idle })
	old := e.start(e.addTask("old", "slow"))
	if _, err := e.m.Stop(e.ctx, old.ID, "test"); err != nil {
		t.Fatal(err)
	}
	for _, birth := range []string{"", "2001-01-01T00:00:00.000Z"} {
		// The ended run's records now point at the live run's pane and pid.
		e.exec(`UPDATE sessions SET tmux_pane = ?, pid = ?, pid_started_at = nullif(?, ''), aux_panes = '[]' WHERE run_id = ?`,
			live.Session.TmuxPane, live.Session.PID, birth, old.ID)
		e.exec(`UPDATE runs SET cleanup_pending = 1 WHERE id = ?`, old.ID)
		if _, err := e.m.Cleanup(e.ctx, old.ID); err != nil {
			t.Fatalf("birth %q: %v", birth, err)
		}
		if l := e.m.Tmux.Check(e.ctx, live.Session.TmuxPane, live.Session.PID); l.State != tmux.Alive {
			t.Fatalf("birth %q: the live run's pane was killed: %+v", birth, l)
		}
		if got := e.get(old.ID); got.CleanupPending {
			t.Fatalf("birth %q: cleanup still pending: %q", birth, got.CleanupError)
		}
	}
}

// A child that outlives the recorded process (reparented to launchd) is still found
// through its session and killed; only then is the cleanup confirmed.
func TestCleanupKillsReparentedChildren(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "wait"]
`)
	r := e.start(e.addTask("t", "slow"))
	if _, err := e.m.Stop(e.ctx, r.ID, "test"); err != nil {
		t.Fatal(err)
	}
	childFile := filepath.Join(t.TempDir(), "child")
	ref, err := e.m.Tmux.NewWindow(e.ctx, tmux.Window{Session: "otag-alpha", Name: "parent", Run: r.ID, Owner: e.db.Path,
		Argv: []string{"/bin/sh", "-c", `(trap "" HUP; exec sleep 600) & echo $! > ` + childFile + `; while :; do sleep 1; done`}})
	if err != nil {
		t.Fatal(err)
	}
	birth := proc.Birth(ref.PID)
	var child int
	e.waitFor("child", false, func() bool {
		b, _ := os.ReadFile(childFile)
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return child > 0
	})
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	_ = syscall.Kill(ref.PID, syscall.SIGKILL) // the parent dies; the child is reparented
	e.waitFor("reparented", false, func() bool { p, ok, _ := proc.Get(child); return ok && p.PPID == 1 })
	e.exec(`UPDATE sessions SET aux_panes = ? WHERE run_id = ?`,
		fmt.Sprintf(`[{"role":"sidecar","name":"x","pane":%q,"pid":%d,"birth":%q}]`, ref.PaneID, ref.PID, birth), r.ID)
	e.exec(`UPDATE runs SET cleanup_pending = 1 WHERE id = ?`, r.ID)
	if _, err := e.m.Cleanup(e.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if tmux.ProcessAlive(child) {
		if p, ok, _ := proc.Get(child); ok && !p.Zombie {
			t.Fatal("the reparented child survived the cleanup")
		}
	}
	if got := e.get(r.ID); got.CleanupPending {
		t.Fatalf("cleanup pending: %q", got.CleanupError)
	}
}

// The back-off of a fallback whose start failed survives a daemon restart: the
// restarted daemon waits for it and keeps counting the attempts.
func TestFallbackRetryStateSurvivesRestart(t *testing.T) {
	e := setup(t, fakeAccounts+quotaThenFallback)
	tk := e.addTask("work", "main")
	r1 := e.start(tk)
	e.waitFor("quota", false, func() bool { return e.get(r1.ID).Status == run.Ended })
	e.m.Tmux.Bin = "/nonexistent/tmux"
	e.d.Tick(e.ctx)
	attempts := func() []int {
		rows, err := e.db.Query(`SELECT data FROM events WHERE kind = 'daemon.fallback_retry' AND run_id = ? ORDER BY id`, r1.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []int
		for rows.Next() {
			var data string
			_ = rows.Scan(&data)
			var d struct{ Attempt int }
			_ = json.Unmarshal([]byte(data), &d)
			out = append(out, d.Attempt)
		}
		return out
	}
	if a := attempts(); len(a) != 1 || a[0] != 1 {
		t.Fatalf("attempts %v", a)
	}

	clock := time.Now()
	restarted := &Daemon{DB: e.db, Runs: e.m, Config: e.d.Config, Now: func() time.Time { return clock }, Desktop: e.d.Desktop}
	restarted.Tick(e.ctx)
	if a := attempts(); len(a) != 1 {
		t.Fatalf("a restarted daemon ignored the back-off: %v", a)
	}
	clock = clock.Add(31 * time.Second)
	restarted.Tick(e.ctx)
	if a := attempts(); len(a) != 2 || a[1] != 2 {
		t.Fatalf("attempts after the restart %v, want [1 2]", a)
	}
}
