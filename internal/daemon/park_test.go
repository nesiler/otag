package daemon

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

const parkProfiles = fakeAccounts + `
[profiles.main]
account = "f"
args = ["start", "busy", "wait"]
`

// ask stores `otag ask` of run runID.
func (e *testEnv) ask(runID, body string, park bool) *msg.Message {
	e.t.Helper()
	var m *msg.Message
	err := e.db.Tx(e.ctx, func(tx *sql.Tx) error {
		var err error
		m, _, err = msg.Ask(e.ctx, tx, msg.AskInput{RunID: runID, Body: body, Park: park}, msg.Actor{Name: "run:" + runID})
		return err
	})
	if err != nil {
		e.t.Fatalf("ask: %v", err)
	}
	return m
}

// reply answers message id as the human.
func (e *testEnv) reply(id int64, body string) *msg.Message {
	e.t.Helper()
	var m *msg.Message
	err := e.db.Tx(e.ctx, func(tx *sql.Tx) error {
		var err error
		m, _, err = msg.Reply(e.ctx, tx, msg.ReplyInput{From: msg.Sender{Kind: msg.SenderHuman, Actor: "user:test"}, To: id,
			Body: body}, msg.Actor{Name: "user:test"})
		return err
	})
	if err != nil {
		e.t.Fatalf("reply: %v", err)
	}
	return m
}

// parked starts a run of a new task, asks with --park and lets the turn end, then
// runs the park loop: the run ends parked and the task keeps waiting.
func (e *testEnv) parked() (taskID string, r *run.Run, ask *msg.Message) {
	e.t.Helper()
	taskID = e.addTask("which color", "main")
	r = e.start(taskID)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	ask = e.ask(r.ID, "which color?", true)
	if s, _ := e.taskStatus(taskID); s != task.Waiting {
		e.t.Fatalf("task after ask: %s", s)
	}
	_ = e.d.watchAsks(e.ctx, e.snapshot()) // still in its turn: not parked
	if got := e.get(r.ID); got.Status != run.Running {
		e.t.Fatalf("parked mid-turn: %s %s", got.Status, got.EndedReason)
	}
	e.hook(r.ID, "turn_end", `{"text":"asked","reason":"answer"}`)
	_ = e.d.watchAsks(e.ctx, e.snapshot())
	got := e.get(r.ID)
	if got.Status != run.Ended || got.EndedReason != run.ReasonParked || got.TaskStatus != task.Waiting {
		e.t.Fatalf("after park: %s %s task %s", got.Status, got.EndedReason, got.TaskStatus)
	}
	if l := e.m.Tmux.Check(e.ctx, r.Session.TmuxPane, 0); l.State != tmux.Gone {
		e.t.Fatalf("parked run's window: %+v", l)
	}
	if len(e.notifications("run.parked")) != 1 {
		e.t.Fatal("no run.parked notification")
	}
	return taskID, r, ask
}

// `otag ask --park`: the idle asker is parked (ended parked, task waiting, ask open);
// nothing resumes it until the answer is queued, then the daemon resumes the same
// harness session in a new run whose first prompt is the answer, and the task runs.
func TestAskParkResume(t *testing.T) {
	e := setup(t, parkProfiles)
	tk, r, ask := e.parked()
	if open, _ := msg.HasOpenAsk(e.ctx, e.db, tk); !open {
		t.Fatal("parking closed the ask")
	}
	_ = e.d.watchParked(e.ctx)
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatalf("resumed without an answer: %d runs", n)
	}
	// The parked task cannot start a fresh run by accident.
	if _, err := e.m.Start(e.ctx, run.StartInput{TaskID: tk}); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("start of a parked task: %v", err)
	}

	ans := e.reply(ask.ID, "blue")
	_ = e.d.watchParked(e.ctx)
	r2 := e.get(tk + "-r2")
	if r2.RetryOf != r.ID || r2.Role != "child" || r2.Session == nil || r2.Session.HarnessSessionID != r.Session.HarnessSessionID {
		t.Fatalf("resumed run %+v (session %+v)", r2, r2.Session)
	}
	if r2.TaskStatus != task.Running {
		t.Fatalf("task after resume: %s", r2.TaskStatus)
	}
	d := ans.Deliveries[0]
	got, err := msg.GetDelivery(e.ctx, e.db, d.ID)
	if err != nil || got.State != msg.Dispatched || str(got.Channel) != "launch_prompt" || str(got.To.Run) != r2.ID {
		t.Fatalf("answer delivery %+v %v", got, err)
	}
	prompt, _ := os.ReadFile(filepath.Join(paths.RunDir(e.m.Getenv, r2.ID), "prompt.md"))
	if !strings.Contains(string(prompt), "blue") || !strings.Contains(string(prompt), msg.Marker(d.ID)) {
		t.Fatalf("resume prompt:\n%s", prompt)
	}
	if ns := e.notifications("run.resumed"); len(ns) != 1 {
		t.Fatalf("resumed notifications %d", len(ns))
	}
	_, _ = e.m.Stop(e.ctx, r2.ID, "test over")
}

// A resume that cannot keep the session going leaves the task waiting with its answer
// queued and tells the human once: another profile revision, or an account in
// cooldown (tried again after a back-off, and resumed once it clears).
func TestResumeBlocked(t *testing.T) {
	e := setup(t, parkProfiles)
	tk, r, ask := e.parked()
	e.reply(ask.ID, "blue")

	// The profile changed since the run started.
	var hash string
	_ = e.db.QueryRow(`SELECT config_hash FROM profiles WHERE id = ?`, r.ProfileID).Scan(&hash)
	e.exec(`INSERT INTO profile_revisions (profile_id, account_id, harness, config_hash, config)
		SELECT profile_id, account_id, harness, 'changed', config FROM profile_revisions WHERE id = ?`, r.RevisionID)
	e.exec(`UPDATE profiles SET config_hash = 'changed' WHERE id = ?`, r.ProfileID)
	_ = e.d.watchParked(e.ctx)
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatalf("resumed on another revision: %d runs", n)
	}
	if s, _ := e.taskStatus(tk); s != task.Waiting {
		t.Fatalf("task %s", s)
	}
	ns := e.notifications("run.resume_blocked")
	if len(ns) != 1 || !strings.Contains(ns[0].Body, "changed since run") {
		t.Fatalf("notifications %+v", ns)
	}
	if _, err := e.m.Resume(e.ctx, tk); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("manual resume: %v", err)
	}

	// Same revision, but the account cools down: blocked (retryable), no new notification.
	e.exec(`UPDATE profiles SET config_hash = ? WHERE id = ?`, hash, r.ProfileID)
	var account int64
	_ = e.db.QueryRow(`SELECT account_id FROM runs WHERE id = ?`, r.ID).Scan(&account)
	if err := run.SetCooldown(e.ctx, e.db, account, 0, time.Now().Add(time.Hour), "test"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(11 * time.Minute) // past the back-off
	_ = e.d.watchParked(e.ctx)
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatalf("resumed during a cooldown: %d runs", n)
	}
	_, err := e.m.Resume(e.ctx, tk)
	if e, ok := apperr.As(err); !ok || e.Code != apperr.Unavailable || !e.Retryable {
		t.Fatalf("resume during cooldown: %v", err)
	}
	// A different error is a new warning; the same one is not repeated.
	if n := len(e.notifications("run.resume_blocked")); n != 2 {
		t.Fatalf("%d notifications", n)
	}
	// Within the back-off nothing is tried; after it, the cleared cooldown resumes.
	e.exec(`UPDATE account_state SET cooldown_until = NULL`)
	_ = e.d.watchParked(e.ctx)
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 1 {
		t.Fatal("tried within the back-off")
	}
	e.clock = e.clock.Add(11 * time.Minute)
	_ = e.d.watchParked(e.ctx)
	r2 := e.get(tk + "-r2")
	if r2.TaskStatus != task.Running || r2.Session.HarnessSessionID != r.Session.HarnessSessionID {
		t.Fatalf("resume after the cooldown: %+v", r2)
	}
	_, _ = e.m.Stop(e.ctx, r2.ID, "test over")
}

// `otag run stop --park` (Park) needs a run whose task waits for its own open ask; a
// coordinator is never parked. An asker that is not parked gets parked after
// park_after once it is idle.
func TestParkRulesAndParkAfter(t *testing.T) {
	e := setup(t, parkProfiles)
	tk := e.addTask("work", "main")
	r := e.start(tk)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	if _, err := e.m.Park(e.ctx, r.ID, run.ParkOptions{}); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("park without a question: %v", err)
	}
	ask := e.ask(r.ID, "tabs or spaces?", false)
	e.hook(r.ID, "turn_end", `{"text":"asked","reason":"answer"}`)
	_ = e.d.watchAsks(e.ctx, e.snapshot())
	if got := e.get(r.ID); got.Status != run.Idle {
		t.Fatalf("parked before park_after: %s %s", got.Status, got.EndedReason)
	}
	e.clock = time.Now().Add(31 * time.Minute)
	_ = e.d.watchAsks(e.ctx, e.snapshot())
	got := e.get(r.ID)
	if got.EndedReason != run.ReasonParked || got.TaskStatus != task.Waiting || !strings.Contains(got.Error, "unanswered") {
		t.Fatalf("after park_after: %s %s %q task %s", got.Status, got.EndedReason, got.Error, got.TaskStatus)
	}
	if open, _ := msg.HasOpenAsk(e.ctx, e.db, tk); !open {
		t.Fatalf("ask #%d closed by the park", ask.ID)
	}
	// Cancelling the parked task closes its question.
	_ = e.db.Tx(e.ctx, func(tx *sql.Tx) error {
		if _, err := task.Transition(e.ctx, tx, tk, task.Cancelled, "", 0, task.Actor{Name: "user:test"}); err != nil {
			return err
		}
		_, err := run.StopTaskRun(e.ctx, tx, tk, "cancelled", e.m.Actor)
		return err
	})
	if m, _ := msg.Get(e.ctx, e.db, ask.ID); m.ClosedAt == nil || str(m.ClosedReason) != msg.ClosedCancelled {
		t.Fatalf("ask after cancel: %+v", m)
	}
}

// An idle child without a question gets at most two reminders (system command
// messages), each after idle_after; then the human gets one run.idle notification.
func TestIdleNudge(t *testing.T) {
	e := setup(t, parkProfiles)
	tk := e.addTask("work", "main")
	r := e.start(tk)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	e.hook(r.ID, "turn_end", `{"text":"stopped","reason":"answer"}`)
	nudges := func() int {
		return count(t, e.db, `SELECT count(*) FROM messages WHERE client_msg_id LIKE 'nudge:' || ? || ':%'`, r.ID)
	}
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	if nudges() != 0 {
		t.Fatal("nudged before idle_after")
	}
	e.clock = time.Now().Add(11 * time.Minute)
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	e.exec(`UPDATE messages SET created_at = ? WHERE client_msg_id LIKE 'nudge:%'`, e.clock.UTC().Format(db.TimeFormat))
	_ = e.d.watchIdle(e.ctx, e.snapshot()) // too soon after its reminder: nothing more
	if nudges() != 1 {
		t.Fatalf("%d nudges", nudges())
	}
	// The reminder is handled (read); idle again for idle_after: the second one.
	e.exec(`UPDATE message_deliveries SET state = 'read', read_at = created_at, closed_at = created_at WHERE run_id = ? OR task_id = ?`, r.ID, tk)
	e.clock = time.Now().Add(22 * time.Minute)
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	e.exec(`UPDATE message_deliveries SET state = 'read', read_at = created_at, closed_at = created_at WHERE run_id = ? OR task_id = ?`, r.ID, tk)
	e.clock = time.Now().Add(33 * time.Minute)
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	if nudges() != 2 || len(e.notifications("run.idle")) != 1 {
		t.Fatalf("%d nudges, %d notifications", nudges(), len(e.notifications("run.idle")))
	}
	_, _ = e.m.Stop(e.ctx, r.ID, "test over")
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
