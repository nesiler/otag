package daemon

import (
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// hook delivers one fake-harness hook to a run, like `otag event fake <kind>`.
func (e *testEnv) hook(runID, kind, payload string) {
	e.t.Helper()
	time.Sleep(5 * time.Millisecond) // heartbeat_at has millisecond resolution
	token := fake.Token(paths.RunDir(e.d.Runs.Getenv, runID))
	req := run.HookRequest{Harness: "fake", Kind: kind, RunID: runID, Payload: []byte(payload),
		Getenv: func(k string) string {
			if k == run.TokenEnv {
				return token
			}
			return e.d.Runs.Getenv(k)
		}}
	if _, err := run.HandleHook(e.ctx, e.db, adapter.NewRegistry(fake.New()), req); err != nil {
		e.t.Fatalf("hook %s: %v", kind, err)
	}
}

// snapshot is what a tick sees at its start.
func (e *testEnv) snapshot() []*run.Run {
	e.t.Helper()
	runs, err := e.d.liveRuns(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return runs
}

// The stall watchdog decides on the tick's snapshot; an event that arrives before the
// stop (a new heartbeat) cancels it.
func TestStallRacingHookIsNotKilled(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "busy", "wait"]
`)
	tk := e.addTask("slow", "slow")
	r := e.start(tk)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	e.clock = time.Now().Add(time.Hour)
	runs := e.snapshot()
	e.hook(r.ID, "activity", `{"tool":"Bash"}`)
	e.d.watchStalls(e.ctx, runs)
	if got := e.get(r.ID); got.Status != run.Running {
		t.Fatalf("a run with a fresh event was stopped: %s %s", got.Status, got.EndedReason)
	}
	if len(e.notifications("run.stalled")) != 0 {
		t.Fatal("stall notification for a live run")
	}
	// With a fresh snapshot the (still quiet) run is stalled as usual.
	e.d.watchStalls(e.ctx, e.snapshot())
	if got := e.get(r.ID); got.EndedReason != run.ReasonStalled {
		t.Fatalf("not stalled on the next look: %s %s", got.Status, got.EndedReason)
	}
}

// The pane watcher decides on the tick's snapshot: a run that changed meanwhile is
// left for the next tick, and one that ended another way gets no crash notification.
func TestDeadPaneRacingHook(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.crashy]
account = "f"
args = ["start", "busy", "exit:3"]
`)
	tk := e.addTask("crash", "crashy")
	r := e.start(tk)
	e.waitFor("pane exit", false, func() bool {
		return e.snapshot()[0].Dead() && e.get(r.ID).Status == run.Running
	})

	runs := e.snapshot()
	e.hook(r.ID, "question", `{"text":"Which color?"}`) // running → waiting
	e.d.watchPanes(e.ctx, runs)
	if got := e.get(r.ID); got.Status != run.Waiting {
		t.Fatalf("reaped a run that changed after the snapshot: %s %s", got.Status, got.EndedReason)
	}

	runs = e.snapshot()
	e.hook(r.ID, "session_end", `{"reason":"exit"}`) // the harness's own end arrives late
	e.d.watchPanes(e.ctx, runs)
	got := e.get(r.ID)
	if got.EndedReason != run.ReasonExited || got.TaskStatus != task.Ready {
		t.Fatalf("after session_end: %s %s task %s", got.Status, got.EndedReason, got.TaskStatus)
	}
	if n := len(e.notifications("run.crashed")); n != 0 {
		t.Fatalf("%d crash notification(s) for a run that exited", n)
	}
}
