package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

// `otag task cancel` and `otag task block` stop a run that still works on the task (it
// ends as stopped, its window is killed and nothing is left to clean up); the task
// keeps the status it was given, and the run's own `otag done` afterwards fails cleanly.
func TestTaskCancelAndBlockStopTheRun(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	for _, c := range []struct{ cmd, status string }{{"cancel", task.Cancelled}, {"block", task.Blocked}} {
		id := h.addTask("alpha", c.cmd)
		r := h.start(id, "--profile", "manual").Run
		h.waitRun(r.ID, "idle", func(r run.Run) bool { return r.Status == run.Idle })

		code, e := h.json("task", c.cmd, id, "--reason", "not now")
		var tk task.Task
		_ = json.Unmarshal(e.Data, &tk)
		if code != 0 || tk.Status != c.status {
			t.Fatalf("%s: %d %s %+v", c.cmd, code, e.Data, e.Error)
		}
		got := h.show(r.ID)
		if got.EndedReason != run.ReasonStopped || got.TaskStatus != c.status || got.CleanupPending ||
			!strings.Contains(got.Error, "not now") {
			t.Fatalf("%s: run %s %s task %s pending %v error %q", c.cmd, got.Status, got.EndedReason, got.TaskStatus,
				got.CleanupPending, got.Error)
		}
		if l := h.tmux.Check(ctx, r.Session.TmuxPane, r.Session.PID); l.State != tmux.Gone {
			t.Fatalf("%s: pane %+v", c.cmd, l)
		}
		h.env[run.TokenEnv] = h.token(r.ID)
		code, e = h.json("--run", r.ID, "done", "finished anyway")
		delete(h.env, run.TokenEnv)
		if code == 0 || e.Error.Code != "INVALID_TRANSITION" {
			t.Fatalf("%s: late done: %d %+v", c.cmd, code, e.Error)
		}
	}
	// Blocking by a dependency changes nothing about a running run.
	a, b := h.addTask("alpha", "a"), h.addTask("alpha", "b")
	r := h.start(a, "--profile", "manual").Run
	h.ok("task", "block", a, "--on", b)
	if got := h.show(r.ID); got.Status == run.Ended {
		t.Fatalf("block --on stopped the run: %s", got.EndedReason)
	}
}

// Projects in "same" mode share one directory: one active run at a time there.
func TestSameWorkspaceOneActiveRun(t *testing.T) {
	h := newRunHarness(t)
	one, two := h.addTask("beta", "one"), h.addTask("beta", "two")
	r1 := h.start(one).Run
	code, e := h.json("run", "start", two, "--no-attach")
	if code == 0 || e.Error.Code != "CONFLICT" || !strings.Contains(e.Error.Message, r1.ID) ||
		!strings.Contains(e.Error.Message, "one active run per directory") {
		t.Fatalf("second run in the same directory: %d %+v", code, e.Error)
	}
	if s := h.taskStatus(two); s != task.Ready {
		t.Fatalf("rejected start left task %s %s", two, s)
	}
	h.ok("run", "stop", r1.ID)
	if r2 := h.start(two).Run; r2.Workspace.Path != r1.Workspace.Path {
		t.Fatalf("workspace %s, want %s", r2.Workspace.Path, r1.Workspace.Path)
	}
}
