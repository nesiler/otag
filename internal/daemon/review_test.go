package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/proc"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

func (e *testEnv) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

// rootProcess returns a running process of another user (root), which this test
// process can see but not signal.
func rootProcess(t *testing.T) proc.Process {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("needs a process this user cannot signal")
	}
	all, err := proc.All()
	if err != nil {
		t.Skip(err)
	}
	var out proc.Process
	for _, p := range all {
		if p.UID == 0 && p.PID > 1 && !p.Zombie && p.Birth != "" && (out.PID == 0 || p.PID < out.PID) {
			out = p
		}
	}
	if out.PID == 0 {
		t.Skip("no root process found")
	}
	return out
}

// A fallback does not start while a process of the ended run may still write to the
// worktree. A process recorded for the run that outlived its window and cannot be
// killed (here a root process) holds it back, also when its pane is long gone, and
// the cleanup failure is reported. One that ignores SIGHUP and SIGTERM is killed with
// SIGKILL, and then the fallback starts.
func TestFallbackWaitsForOldProcesses(t *testing.T) {
	old := quiesceTimeout
	quiesceTimeout = 500 * time.Millisecond
	t.Cleanup(func() { quiesceTimeout = old })
	e := setup(t, fakeAccounts+quotaThenFallback)
	root := rootProcess(t)
	tk := e.addTask("work", "main")
	r1 := e.start(tk)
	e.waitFor("quota", false, func() bool { return e.get(r1.ID).Status == run.Ended })
	e.exec(`UPDATE sessions SET aux_panes = ? WHERE run_id = ?`,
		fmt.Sprintf(`[{"role":"sidecar","name":"x","pane":"%%99999","pid":%d,"birth":%q}]`, root.PID, root.Birth), r1.ID)

	e.d.Tick(e.ctx)
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatalf("fallback started while a process of %s still runs (%d runs)", r1.ID, n)
	}
	var data string
	_ = e.db.QueryRow(`SELECT data FROM events WHERE kind = 'daemon.fallback_retry' AND run_id = ?`, r1.ID).Scan(&data)
	if !strings.Contains(data, "not fully stopped") {
		t.Fatalf("retry event %q", data)
	}
	if n := count(t, e.db, `SELECT count(*) FROM events WHERE dedupe_key = ?`, "fallback:"+r1.ID); n != 0 {
		t.Fatal("decision recorded without a run")
	}
	if got := e.get(r1.ID); !got.CleanupPending || !strings.Contains(got.CleanupError, fmt.Sprint(root.PID)) {
		t.Fatalf("cleanup state: pending %v error %q", got.CleanupPending, got.CleanupError)
	}
	if len(e.notifications("run.cleanup_failed")) != 1 {
		t.Fatal("no cleanup_failed notification")
	}

	ref, err := e.m.Tmux.NewWindow(e.ctx, tmux.Window{Session: "otag-alpha", Name: "stubborn",
		Argv: []string{"/bin/sh", "-c", `trap "" HUP TERM; while :; do sleep 1; done`}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-ref.PID, syscall.SIGKILL); _ = syscall.Kill(ref.PID, syscall.SIGKILL) })
	e.exec(`UPDATE sessions SET aux_panes = ? WHERE run_id = ?`,
		fmt.Sprintf(`[{"role":"sidecar","name":"x","pane":%q,"pid":%d}]`, ref.PaneID, ref.PID), r1.ID)
	e.clock = e.clock.Add(time.Minute) // past both back-offs
	e.d.Tick(e.ctx)
	if tmux.ProcessAlive(ref.PID) {
		t.Fatal("the stubborn process survived")
	}
	if r2 := e.get(tk + "-r2"); r2.Profile != "other" {
		t.Fatalf("r2 %+v", r2)
	}
	if got := e.get(r1.ID); got.CleanupPending || got.CleanupError != "" {
		t.Fatalf("cleanup not settled: %v %q", got.CleanupPending, got.CleanupError)
	}
}

// A decision made on a task that changed before it was applied (claimed by someone,
// edited, another run) is dropped: neither a start nor a block, nothing recorded.
func TestStaleFallbackDecisionIsDropped(t *testing.T) {
	e := setup(t, fakeAccounts+quotaThenFallback+`
[profiles.lonely]
account = "h"
args = ["start", "busy", "quota", "wait"]
`)
	for _, profile := range []string{"main", "lonely"} { // start and block decisions
		tk := e.addTask("work", profile)
		r1 := e.start(tk)
		e.waitFor("quota", false, func() bool { return e.get(r1.ID).Status == run.Ended })
		var version int64
		_ = e.db.QueryRow(`SELECT version FROM tasks WHERE id = ?`, tk).Scan(&version)
		e.exec(`UPDATE tasks SET title = 'edited', version = version + 1 WHERE id = ?`, tk)
		if err := e.d.fallback(e.ctx, candidate{task: tk, run: r1.ID, version: version}); err != nil {
			t.Fatal(err)
		}
		if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
			t.Fatalf("%s: stale decision started a run", profile)
		}
		if s, _ := e.taskStatus(tk); s != task.Ready {
			t.Fatalf("%s: stale decision changed the task: %s", profile, s)
		}
		if n := count(t, e.db, `SELECT count(*) FROM events WHERE dedupe_key = ?`, "fallback:"+r1.ID); n != 0 {
			t.Fatalf("%s: stale decision recorded", profile)
		}
		e.d.Tick(e.ctx) // a fresh look decides on the current task
		if n := count(t, e.db, `SELECT count(*) FROM events WHERE dedupe_key = ?`, "fallback:"+r1.ID); n != 1 {
			t.Fatalf("%s: not handled on the next tick", profile)
		}
	}
}

// The daemon died after the fallback run was created and before its pane was
// recorded: after launch_timeout a restarted daemon ends that run (cause interrupted)
// and starts the same fallback profile again, continuing the chain.
func TestFallbackResumesAfterCrashMidLaunch(t *testing.T) {
	e := setup(t, fakeAccounts+quotaThenFallback)
	tk := e.addTask("work", "main")
	r1 := e.start(tk)
	e.waitFor("quota", false, func() bool { return e.get(r1.ID).Status == run.Ended })
	// What the dead daemon left: the decision and the run (one transaction), no pane.
	res, err := e.m.Start(e.ctx, run.StartInput{TaskID: tk, Profile: "other", FallbackOrigin: profileID(t, e.db, "main")})
	if err != nil {
		t.Fatal(err)
	}
	r2 := res.Run
	_ = e.m.Tmux.KillWindow(e.ctx, r2.Session.TmuxPane)
	e.exec(`UPDATE sessions SET tmux_pane = NULL, pid = NULL WHERE run_id = ?`, r2.ID)
	e.exec(`UPDATE runs SET status = 'starting', version = version + 1 WHERE id = ?`, r2.ID)

	restarted := &Daemon{DB: e.db, Runs: e.m, Config: e.d.Config, Now: func() time.Time { return time.Now().Add(6 * time.Minute) },
		Desktop: e.d.Desktop}
	restarted.Tick(e.ctx)
	if got := e.get(r2.ID); got.EndedReason != run.ReasonLaunchFailed {
		t.Fatalf("r2 %s %s", got.Status, got.EndedReason)
	}
	r3 := e.get(tk + "-r3")
	if r3.Profile != "other" || r3.RetryOf != r2.ID || r3.FallbackOrigin != "main" {
		t.Fatalf("r3 %+v", r3)
	}
}

// A stop whose cleanup failed after the run went to stopping (here tmux could not kill
// the window) is finished by the daemon with the recorded intent.
func TestInterruptedStopIsFinished(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "busy", "wait"]
`)
	tk := e.addTask("slow", "slow")
	r := e.start(tk)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux")
	}
	broken := filepath.Join(t.TempDir(), "tmux")
	_ = os.WriteFile(broken, []byte("#!/bin/sh\ncase \"$*\" in *kill-window*) echo nope >&2; exit 1;; esac\nexec '"+real+"' \"$@\"\n"), 0o700)
	e.m.Tmux.Bin = broken
	if _, err := e.m.StopWith(e.ctx, r.ID, run.StopOptions{Reason: "by test", EndReason: run.ReasonStalled}); err == nil {
		t.Fatal("stop with a broken tmux succeeded")
	}
	if got := e.get(r.ID); got.Status != run.Stopping {
		t.Fatalf("status %s", got.Status)
	}
	e.m.Tmux.Bin = real
	e.d.Tick(e.ctx) // too early: someone may still be stopping it
	if got := e.get(r.ID); got.Status != run.Stopping {
		t.Fatalf("finished too early: %s", got.Status)
	}
	e.clock = time.Now().Add(2 * time.Minute)
	e.d.Tick(e.ctx)
	got := e.get(r.ID)
	if got.EndedReason != run.ReasonStalled || got.Error != "by test" || got.TaskStatus != task.Ready {
		t.Fatalf("after resume: %s %s %q task %s", got.Status, got.EndedReason, got.Error, got.TaskStatus)
	}
	if l := e.m.Tmux.Check(e.ctx, r.Session.TmuxPane, 0); l.State != tmux.Gone {
		t.Fatalf("pane %+v", l)
	}
}

// A launch that records its pane after the daemon's snapshot is not killed by the
// launch timeout.
func TestLaunchTimeoutRacingPaneRecord(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.hang]
account = "f"
env = { FAKE_SIDECAR = "never-ready", FAKE_READY_TIMEOUT = "60s" }
`)
	tk := e.addTask("hang", "hang")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.m.Start(e.ctx, run.StartInput{TaskID: tk})
	}()
	id := tk + "-r1"
	e.waitFor("starting with a sidecar", false, func() bool {
		var aux string
		_ = e.db.QueryRow(`SELECT coalesce((SELECT aux_panes FROM sessions WHERE run_id = ?), '[]')`, id).Scan(&aux)
		return aux != "[]"
	})
	e.clock = time.Now().Add(6 * time.Minute)
	runs := e.snapshot()
	e.exec(`UPDATE sessions SET tmux_pane = '%99999' WHERE run_id = ?`, id) // the launcher records its pane
	e.d.watchLaunches(e.ctx, runs)
	if got := e.get(id); got.Status != run.Starting {
		t.Fatalf("a launch that recorded its pane was ended: %s %s", got.Status, got.EndedReason)
	}
	_, _ = e.m.Stop(e.ctx, id, "test over")
	<-done
}
