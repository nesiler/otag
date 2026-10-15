package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

const quotaThenFallback = `
[profiles.main]
account = "f"
args = ["start", "busy", "quota", "wait"]
fallback = ["other"]
[profiles.other]
account = "g"
args = ["start", "wait"]
`

// A fallback start that fails before creating a run (tmux missing) is not lost: the
// decision is taken back and retried after a back-off.
func TestFallbackStartRetriesTransientFailure(t *testing.T) {
	e := setup(t, fakeAccounts+quotaThenFallback)
	tk := e.addTask("work", "main")
	r1 := e.start(tk)
	e.waitFor("quota", false, func() bool { return e.get(r1.ID).Status == run.Ended })

	bin := e.m.Tmux.Bin
	e.m.Tmux.Bin = "/nonexistent/tmux"
	e.d.Tick(e.ctx)
	if n := count(t, e.db, `SELECT count(*) FROM events WHERE dedupe_key = ?`, "fallback:"+r1.ID); n != 0 {
		t.Fatal("the decision of a start that created nothing was kept")
	}
	if n := count(t, e.db, `SELECT count(*) FROM events WHERE kind = 'daemon.fallback_retry' AND run_id = ?`, r1.ID); n != 1 {
		t.Fatalf("%d retry events", n)
	}
	if s, _ := e.taskStatus(tk); s != task.Ready {
		t.Fatalf("task %s", s)
	}

	e.m.Tmux.Bin = bin
	e.d.Tick(e.ctx) // within the back-off
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatalf("retried before the back-off: %d runs", n)
	}
	e.clock = e.clock.Add(31 * time.Second)
	e.d.Tick(e.ctx)
	r2 := e.get(tk + "-r2")
	if r2.Profile != "other" || r2.RetryOf != r1.ID {
		t.Fatalf("r2 %+v", r2)
	}
	if n := count(t, e.db, `SELECT count(*) FROM events WHERE dedupe_key = ?`, "fallback:"+r1.ID); n != 1 {
		t.Fatal("decision not recorded after the retry")
	}
}

// Transient failures give up after maxStartRetries: the task is blocked with the reason.
func TestFallbackStartGivesUp(t *testing.T) {
	e := setup(t, fakeAccounts+quotaThenFallback)
	tk := e.addTask("work", "main")
	r1 := e.start(tk)
	e.waitFor("quota", false, func() bool { return e.get(r1.ID).Status == run.Ended })
	e.m.Tmux.Bin = "/nonexistent/tmux"
	for i := 0; i < maxStartRetries; i++ {
		e.d.Tick(e.ctx)
		e.clock = e.clock.Add(11 * time.Minute)
	}
	status, reason := e.taskStatus(tk)
	if status != task.Blocked || !strings.Contains(reason, "fallback to other could not start") ||
		!strings.Contains(reason, "tmux") || !strings.Contains(reason, "5 attempts") {
		t.Fatalf("task %s %q", status, reason)
	}
	if len(e.notifications("task.blocked")) != 1 {
		t.Fatal("no notification")
	}
}

// A permanent start failure (no adapter for the profile's harness) blocks the task at
// once instead of abandoning the chain silently.
func TestFallbackStartPermanentFailureBlocks(t *testing.T) {
	e := setup(t, fakeAccounts+`
[accounts.c]
harness = "claude"
[profiles.main]
account = "f"
args = ["start", "busy", "quota", "wait"]
fallback = ["cl"]
[profiles.cl]
account = "c"
`)
	tk := e.addTask("work", "main")
	r1 := e.start(tk)
	e.waitFor("quota", false, func() bool { return e.get(r1.ID).Status == run.Ended })
	e.d.Tick(e.ctx)
	status, reason := e.taskStatus(tk)
	if status != task.Blocked || !strings.Contains(reason, "fallback to cl could not start") ||
		!strings.Contains(reason, "no adapter for harness claude") {
		t.Fatalf("task %s %q", status, reason)
	}
	ns := e.notifications("task.blocked")
	if len(ns) != 1 || ns[0].RunID != r1.ID {
		t.Fatalf("notifications %+v", ns)
	}
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatalf("%d runs", n)
	}
}
