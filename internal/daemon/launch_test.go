package daemon

import (
	"testing"
	"time"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// A launch that never records its pane (its launcher was killed; here it hangs on a
// sidecar) is ended as launch_failed (cause interrupted) after launch_timeout; the
// launch loses the race and kills what it started.
func TestLaunchTimeout(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.hang]
account = "f"
env = { FAKE_SIDECAR = "never-ready", FAKE_READY_TIMEOUT = "60s" }
`)
	tk := e.addTask("hang", "hang")
	errc := make(chan error, 1)
	go func() {
		_, err := e.m.Start(e.ctx, run.StartInput{TaskID: tk})
		errc <- err
	}()
	id := tk + "-r1"
	e.waitFor("starting with a sidecar", false, func() bool {
		var aux string
		_ = e.db.QueryRow(`SELECT coalesce((SELECT aux_panes FROM sessions WHERE run_id = ?), '[]')`, id).Scan(&aux)
		return aux != "[]"
	})
	e.d.watchLaunches(e.ctx, e.snapshot())
	if got := e.get(id); got.Status != run.Starting {
		t.Fatalf("ended within launch_timeout: %s", got.Status)
	}
	e.clock = time.Now().Add(6 * time.Minute)
	e.d.watchLaunches(e.ctx, e.snapshot())
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("the interrupted launch reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launch still running")
	}
	// The stop races the interrupted launch killing its own windows; under load a tmux
	// or database call of the stop can fail half way and leave the run stopping, which
	// the daemon finishes (watchStopping, past stopRetryAfter). Wait for the stop to
	// complete either way.
	e.waitFor("the stop to complete", false, func() bool {
		if e.get(id).Status == run.Stopping {
			e.d.watchStopping(e.ctx, e.snapshot())
		}
		return e.get(id).Status == run.Ended
	})
	got := e.get(id)
	if got.EndedReason != run.ReasonLaunchFailed || got.TaskStatus != task.Ready {
		t.Fatalf("after launch_timeout: %s %s task %s", got.Status, got.EndedReason, got.TaskStatus)
	}
	var cause string
	_ = e.db.QueryRow(`SELECT json_extract(data, '$.cause') FROM events WHERE run_id = ? AND kind = 'run.status'
		AND to_status = 'ended'`, id).Scan(&cause)
	if cause != run.CauseInterrupted {
		t.Fatalf("cause %q", cause)
	}
	if len(e.notifications("run.launch_failed")) != 1 {
		t.Fatal("no notification")
	}
	// interrupted is not a fallback reason.
	e.d.Tick(e.ctx)
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatalf("%d runs", n)
	}
}
