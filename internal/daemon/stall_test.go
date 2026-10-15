package daemon

import (
	"testing"
	"time"

	"github.com/nesiler/otag/internal/run"
)

// A tool call that started and has not ended (a long Bash command or build emits no
// events) gets tool_stall_after instead of stall_after.
func TestStallWaitsLongerDuringATool(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.building]
account = "f"
args = ["start", "busy", "toolrun", "wait"]
[profiles.built]
account = "g"
args = ["start", "busy", "toolrun", "tooldone", "wait"]
`)
	building := e.start(e.addTask("build", "building"))
	built := e.start(e.addTask("built", "built"))
	for _, r := range []*run.Run{building, built} {
		e.waitFor("tool events", false, func() bool {
			return count(t, e.db, `SELECT count(*) FROM events WHERE run_id = ? AND kind = 'hook.activity'`, r.ID) ==
				map[string]int{building.ID: 1, built.ID: 2}[r.ID]
		})
	}
	e.clock = time.Now().Add(30 * time.Minute) // past stall_after (20m), within tool_stall_after (45m)
	e.d.Tick(e.ctx)
	if got := e.get(building.ID); got.Status != run.Running {
		t.Fatalf("run inside a tool call stalled after 30m: %s %s", got.Status, got.EndedReason)
	}
	if got := e.get(built.ID); got.EndedReason != run.ReasonStalled {
		t.Fatalf("run whose tool ended not stalled: %s %s", got.Status, got.EndedReason)
	}
	e.clock = time.Now().Add(46 * time.Minute)
	e.d.Tick(e.ctx)
	if got := e.get(building.ID); got.EndedReason != run.ReasonStalled {
		t.Fatalf("run inside a tool call not stalled after tool_stall_after: %s %s", got.Status, got.EndedReason)
	}
}

// With stall_pane_activity, output of the pane after the last event is a heartbeat.
func TestStallPaneActivity(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.chatty]
account = "f"
args = ["start", "busy", "sleep:2", "wait"]
`)
	r := e.start(e.addTask("chatty", "chatty"))
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	hb := e.get(r.ID).HeartbeatAt
	time.Sleep(2500 * time.Millisecond) // the pane prints "fake: wait" ~2s after the last event
	if got := e.get(r.ID).HeartbeatAt; got != hb {
		t.Fatalf("unexpected event: %s → %s", hb, got)
	}
	e.clock = parseTime(hb).Add(e.d.Config.StallAfter + 500*time.Millisecond)
	e.d.Config.StallPaneActivity = true
	e.d.Tick(e.ctx)
	if got := e.get(r.ID); got.Status != run.Running {
		t.Fatalf("stalled although the pane printed after the last event: %s %s", got.Status, got.EndedReason)
	}
	e.d.Config.StallPaneActivity = false
	e.d.Tick(e.ctx)
	if got := e.get(r.ID); got.EndedReason != run.ReasonStalled {
		t.Fatalf("pane output counted without stall_pane_activity: %s %s", got.Status, got.EndedReason)
	}
}
