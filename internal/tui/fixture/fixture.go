// Package fixture is a hand-built data.Snapshot for the TUI's unit and golden tests:
// every row kind, at a fixed time.
package fixture

import (
	"time"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/quota"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/data"
)

// Now is the snapshot's time.
var Now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) string { return Now.Add(-d).Format(db.TimeFormat) }

func str(s string) *string { return &s }

// Snapshot returns a new snapshot:
//
//	alpha (strict): coordinator alpha-c1 (idle); Ready to deliver: al-9 (done, run
//	al-9-r1 ended done); al-14 waiting (open ask of al-14-r1); al-16 blocked; al-12
//	running (al-12-r2 running, al-12-r1 crashed earlier) with review task al-17 nested;
//	al-13 ready p2; al-15 draft.
//	beta (light): be-1 ready; be-2 delivered (closed, with ShowClosed).
func Snapshot(showClosed bool) *data.Snapshot {
	tk := func(id, project, title, status string, prio int, changed time.Duration, current string) task.Task {
		return task.Task{ID: id, Project: project, Title: title, Status: status, Priority: prio,
			StatusChangedAt: ago(changed), CurrentRunID: current}
	}
	rn := func(id, project, taskID, role, status string, attempt int, started time.Duration, pane string) run.Run {
		r := run.Run{ID: id, Project: project, TaskID: taskID, Role: role, Status: status, Attempt: attempt,
			Profile: "sonnet", Account: "c2", Harness: "claude", QueuedAt: ago(started + time.Second),
			StartedAt: ago(started)}
		if pane != "" {
			r.Session = &run.Session{TmuxPane: pane, TmuxSession: "otag-" + project, PID: 100}
		}
		return r
	}
	s := &data.Snapshot{
		At: Now,
		Projects: []registry.ProjectRow{
			{ID: 1, Name: "alpha", Prefix: "al", Guard: "strict"},
			{ID: 2, Name: "beta", Prefix: "be", Guard: "light"},
		},
		Open: []task.Task{
			tk("al-12", "alpha", "Fix login", task.Running, 1, 12*time.Minute, "al-12-r2"),
			tk("al-13", "alpha", "Docs", task.Ready, 2, 3*time.Hour, ""),
			tk("al-14", "alpha", "Ask about colors", task.Waiting, 0, 5*time.Minute, "al-14-r1"),
			tk("al-15", "alpha", "Draft idea", task.Draft, 0, 26*time.Hour, ""),
			tk("al-16", "alpha", "Blocked thing", task.Blocked, 0, 2*time.Hour, ""),
			tk("al-17", "alpha", "Review of al-12", task.Ready, 0, 30*time.Second, ""),
			tk("be-1", "beta", "Beta thing", task.Ready, 0, 50*time.Minute, ""),
		},
		Deliverable: []task.Task{tk("al-9", "alpha", "Add CSV export", task.Done, 0, 3*time.Hour, "")},
		ReviewOf:    map[string]string{"al-17": "al-12"},
		Runs: []run.Run{
			rn("al-12-r2", "alpha", "al-12", "child", run.Running, 2, 12*time.Minute, "%7"),
			rn("al-14-r1", "alpha", "al-14", "child", run.Waiting, 1, 20*time.Minute, "%8"),
			rn("alpha-c1", "alpha", "", "coordinator", run.Idle, 1, 2*time.Hour, "%1"),
		},
		RunCount: map[string]int{"al-12": 2, "al-14": 1, "al-9": 1},
		History:  map[string][]run.Run{},
		Needs: &msg.Needs{
			Questions: []msg.NeedQuestion{{Message: &msg.Message{ID: 42, Project: "alpha", Kind: msg.KindQuestion, Body: "Which color?",
				From: msg.From{Kind: msg.SenderRun, Run: str("al-14-r1")}, Task: str("al-14")}, To: "human", AgeS: 300,
				Action: `otag reply 42 "<answer>"`}},
			Blocked: []msg.NeedBlocked{{Task: "al-16", Title: "Blocked thing", Reason: "waits for the API",
				Action: "otag task ready al-16"}},
		},
		Asks: []msg.OpenAsk{{Asker: "al-14-r1", Message: &msg.Message{ID: 42, Project: "alpha"}}},
		Inbox: []msg.Item{{Message: msg.Message{ID: 41, Project: "alpha", Kind: msg.KindInfo, Body: "al-9 is done",
			From: msg.From{Kind: msg.SenderRun, Run: str("al-9-r1")}, Task: str("al-9")},
			Delivery: msg.Delivery{State: msg.Queued}}},
		Notes: []notify.Notification{{ID: 7, Project: "alpha", RunID: "al-12-r1", Kind: "run.crashed",
			Severity: notify.Warn, Title: "al-12-r1 crashed"}},
		Unread: 1,
		Quota: []quota.Row{{Account: "c2", Harness: "claude", Window: "5h", Source: "statusline",
			UsedPercent: ptr(91), ObservedAt: Now.Add(-time.Minute)}},
		Profiles: []registry.ProfileRow{{Name: "opus", Account: "c2", Harness: "claude", Model: "opus"},
			{Name: "sonnet", Account: "c2", Harness: "claude", Model: "sonnet", Tags: []string{"code"}}},
		Unavailable: map[string]string{"opus": "quota 91% of 5h, threshold 90%"},
	}
	ended := func(r run.Run, reason string, ran time.Duration) run.Run {
		r.Status, r.EndedReason = run.Ended, reason
		start, _ := time.Parse(time.RFC3339, r.StartedAt)
		r.EndedAt = start.Add(ran).Format(db.TimeFormat)
		return r
	}
	s.Runs = append(s.Runs, ended(rn("al-9-r1", "alpha", "al-9", "child", "", 1, 4*time.Hour, "%3"), run.ReasonDone, 40*time.Minute))
	s.History["al-12"] = []run.Run{s.Runs[0], ended(rn("al-12-r1", "alpha", "al-12", "child", "", 1, time.Hour, "%5"),
		run.ReasonCrashed, 10*time.Minute)}
	if showClosed {
		s.Closed = []task.Task{tk("be-2", "beta", "Old", task.Delivered, 0, 48*time.Hour, ""),
			tk("al-9", "alpha", "Add CSV export", task.Done, 0, 3*time.Hour, "")}
	}
	return s
}

// Live is the liveness of the fixture's live runs: every pane alive.
func Live() map[string]tmux.Liveness {
	return map[string]tmux.Liveness{
		"al-12-r2": {State: tmux.Alive, Pane: "%7"},
		"al-14-r1": {State: tmux.Alive, Pane: "%8"},
		"alpha-c1": {State: tmux.Alive, Pane: "%1"},
	}
}

func ptr(f float64) *float64 { return &f }
