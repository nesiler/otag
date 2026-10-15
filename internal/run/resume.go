package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

// ReasonParked ends a run that was parked while its task waits for an answer (`otag ask
// --park`, `otag run stop --park`, or the daemon's park_after). The task stays waiting;
// the answer resumes the harness session in a new run (Resume).
const ReasonParked = "parked"

func msgActor(a Actor) msg.Actor { return msg.Actor{Name: a.Name, Source: a.source()} }

// ParkOptions tune Park.
type ParkOptions struct {
	Reason string  // default "parked: waiting for an answer to #<ask>"
	Expect *Expect // see StopOptions.Expect (the daemon parks an idle run it saw)
	// SkipDispatched refuses to park a run with a dispatched delivery (it is working on a
	// message, whatever its status says); checked in the same transaction as the stop.
	SkipDispatched bool
}

// Park ends a child or review run whose task waits for the answer to the run's open
// ask: the run's windows are closed through the normal stop (cleanup included), the
// run ends with ended_reason parked, and the task stays waiting with the ask open. A
// coordinator is never parked. The guard is checked again in the transaction that
// moves the run to stopping: an answered ask or a task that moved on stops nothing.
func (m *Manager) Park(ctx context.Context, ref string, o ParkOptions) (*StopResult, error) {
	r, err := Resolve(ctx, m.DB, ref)
	if err != nil {
		return nil, err
	}
	if r.Role == "coordinator" || r.TaskID == "" {
		return nil, apperr.New(apperr.Invalid, "run %s is a coordinator; only a run whose task waits for an answer can be parked", r.ID)
	}
	if !live(r.Status) {
		return nil, apperr.New(apperr.InvalidTransition, "run %s is %s %s; only a live run can be parked", r.ID, r.Status, r.EndedReason)
	}
	asks, err := msg.OpenAsks(ctx, m.DB, msg.AskFilter{TaskID: r.TaskID, RunID: r.ID})
	if err != nil {
		return nil, err
	}
	if len(asks) == 0 {
		return nil, apperr.New(apperr.InvalidTransition, "run %s has no open question (otag ask); only a run waiting for an answer can be parked", r.ID)
	}
	ask := asks[0].Message.ID
	if o.Reason == "" {
		o.Reason = fmt.Sprintf("parked: waiting for an answer to #%d", ask)
	}
	guard := func(tx *sql.Tx) error {
		t, err := task.Get(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		if t.CurrentRunID != r.ID || t.Status != task.Waiting {
			return apperr.New(apperr.InvalidTransition, "task %s is %s (run %q); only a task waiting for an answer is parked",
				t.ID, t.Status, t.CurrentRunID)
		}
		open, err := msg.OpenAsks(ctx, tx, msg.AskFilter{TaskID: r.TaskID, RunID: r.ID})
		if err != nil {
			return err
		}
		if len(open) == 0 || open[0].Message.ID != ask {
			return apperr.New(apperr.InvalidTransition, "question #%d of run %s is no longer open; not parked", ask, r.ID)
		}
		if o.SkipDispatched {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM message_deliveries
				WHERE (run_id = ? OR (task_id = ? AND run_id IS NULL)) AND state = 'dispatched'`, r.ID, r.TaskID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return ErrChanged
			}
		}
		return nil
	}
	return m.StopWith(ctx, r.ID, StopOptions{Reason: o.Reason, EndReason: ReasonParked, Expect: o.Expect, Guard: guard})
}

// ResumeBlocked means a parked task cannot resume (now). It leaves the task waiting:
// `otag needs` lists its answer, and nothing starts a fresh run in its place.
type ResumeBlocked struct {
	Task   string
	Reason string
	Retry  bool // it may clear by itself (the account is cooling down)
}

func (e *ResumeBlocked) Error() string { return "task " + e.Task + " cannot resume: " + e.Reason }

// Unwrap gives the CLI its stable code.
func (e *ResumeBlocked) Unwrap() error {
	if e.Retry {
		return apperr.Retryable(apperr.Unavailable, "%s", e.Error())
	}
	return apperr.New(apperr.InvalidTransition, "%s", e.Error())
}

func blocked(taskID string, retry bool, format string, args ...any) error {
	return &ResumeBlocked{Task: taskID, Reason: fmt.Sprintf(format, args...), Retry: retry}
}

// resumeSpec is what Resume continues: the parked run and the queued deliveries of the
// answer it waited for.
type resumeSpec struct {
	prev   *Run
	answer []int64
}

// Resume continues a parked task (a task id, or a run id of it) in a new run on the
// same harness session: same profile revision, same workspace path and an available
// account, else ResumeBlocked. The task goes waiting -> claimed (task.ClaimWaiting),
// the new run claims the answer as its first prompt (launch_prompt), and the launch
// moves the task to running; without the answer nothing is created.
func (m *Manager) Resume(ctx context.Context, ref string) (*StartResult, error) {
	r, err := Resolve(ctx, m.DB, ref)
	if err != nil {
		return nil, err
	}
	if r.TaskID == "" {
		return nil, apperr.New(apperr.Invalid, "run %s has no task to resume", r.ID)
	}
	return m.Start(ctx, StartInput{TaskID: r.TaskID, resume: &resumeSpec{}})
}

// load reads the parked run of task t and the answer queued for it.
func (rs *resumeSpec) load(ctx context.Context, tx *sql.Tx, t *task.Task) error {
	if t.Status != task.Waiting {
		return blocked(t.ID, false, "it is %s, not waiting", t.Status)
	}
	if t.CurrentRunID == "" {
		return blocked(t.ID, false, "it has no parked run")
	}
	prev, err := Get(ctx, tx, t.CurrentRunID)
	if err != nil {
		return err
	}
	if (prev.Status != Ended || prev.EndedReason != ReasonParked) && prev.Status != Parked {
		return blocked(t.ID, false, "its run %s is %s %s, not parked", prev.ID, prev.Status, prev.EndedReason)
	}
	rs.prev = prev
	if rs.answer, err = queuedAnswer(ctx, tx, t.ID, prev.ID); err != nil {
		return err
	}
	if len(rs.answer) == 0 {
		return blocked(t.ID, false, "no answer to the question of run %s is queued", prev.ID)
	}
	return nil
}

// queuedAnswer returns the queued deliveries of the answer to the latest ask of run
// asker (task taskID).
func queuedAnswer(ctx context.Context, q db.Querier, taskID, asker string) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `
SELECT d.id FROM messages a JOIN message_deliveries d ON d.message_id = a.id
WHERE a.kind = 'answer' AND d.state = 'queued' AND d.recipient_kind IN ('run', 'task') AND d.task_id = ?
  AND a.reply_to_id = (SELECT max(id) FROM messages WHERE task_id = ? AND ask = 1 AND sender_run_id = ?)
ORDER BY d.id`, taskID, taskID, asker)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// resumable checks, inside the creation transaction, that the parked run's session can
// go on as it was: the profile has the revision the run started with (its account and
// harness configuration are unchanged), the account is available, the run has a harness
// session, and the new run gets the same workspace.
func (m *Manager) resumable(ctx context.Context, tx *sql.Tx, rs *resumeSpec, p *plan, t *task.Task, profileID int64) error {
	prev := rs.prev
	if p.rev.id != prev.RevisionID {
		return blocked(t.ID, false, "profile %s changed since run %s (revision %d, now %d); a resume needs the same revision",
			prev.Profile, prev.ID, prev.RevisionID, p.rev.id)
	}
	if ok, why := ProfileAvailable(ctx, tx, profileID); !ok {
		return blocked(t.ID, true, "profile %s is not available: %s", prev.Profile, why)
	}
	if prev.Session == nil || prev.Session.HarnessSessionID == "" {
		return blocked(t.ID, false, "run %s has no harness session id", prev.ID)
	}
	want := p.projectPath
	if p.mode == workspace.Worktree {
		want = paths.Worktree(m.Getenv, p.project, t.ID)
	}
	if w := prev.Workspace; w == nil || w.Mode != p.mode || !sameDir(w.Path, want) {
		return blocked(t.ID, false, "the workspace of run %s is not the one a new run gets (%s)", prev.ID, want)
	}
	return nil
}

// carries reports whether attempt at holds every delivery of ids.
func carries(at *msg.Attempt, ids []int64) bool {
	if at == nil || len(ids) == 0 {
		return false
	}
	var got []int64
	for _, it := range at.Items {
		got = append(got, it.Delivery.ID)
	}
	for _, id := range ids {
		if !slices.Contains(got, id) {
			return false
		}
	}
	return true
}

// Resumable is a parked task whose answer is queued (the daemon resumes these).
type Resumable struct {
	TaskID    string
	RunID     string // the parked run
	ProjectID int64
}

// ParkedWithAnswer lists parked tasks (waiting, their current run ended parked) whose
// answer is queued, oldest first.
func ParkedWithAnswer(ctx context.Context, q db.Querier, limit int, skip ...string) ([]Resumable, error) {
	excl, args := "", []any{}
	if len(skip) > 0 {
		excl = " AND t.id NOT IN (?" + strings.Repeat(",?", len(skip)-1) + ")"
		for _, s := range skip {
			args = append(args, s)
		}
	}
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `
SELECT t.id, r.id, t.project_id FROM tasks t JOIN runs r ON r.id = t.current_run_id
WHERE t.status = 'waiting' AND ((r.status = 'ended' AND r.ended_reason = 'parked') OR r.status = 'parked')
  AND EXISTS (SELECT 1 FROM messages a JOIN message_deliveries d ON d.message_id = a.id
              WHERE a.kind = 'answer' AND d.state = 'queued' AND d.task_id = t.id
                AND a.reply_to_id = (SELECT max(id) FROM messages WHERE task_id = t.id AND ask = 1 AND sender_run_id = r.id))
`+excl+`
ORDER BY t.status_changed_at LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Resumable
	for rows.Next() {
		var x Resumable
		if err := rows.Scan(&x.TaskID, &x.RunID, &x.ProjectID); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// requeueLaunchPrompt puts the first-prompt messages of a run that ends before its
// harness reported anything back in the queue (not sent): the harness never worked on
// them, so the task's next run gets them. A run that did report keeps them dispatched
// (proof or outcome_unknown decides).
func requeueLaunchPrompt(ctx context.Context, tx *sql.Tx, runID, why string, a Actor) error {
	var first sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT first_activity_at FROM runs WHERE id = ?`, runID).Scan(&first); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if first.Valid {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT attempt_nonce FROM message_deliveries
		WHERE run_id = ? AND channel = ? AND state = 'dispatched' AND attempt_nonce IS NOT NULL`, runID, adapter.ChanLaunchPrompt)
	if err != nil {
		return err
	}
	var nonces []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return err
		}
		nonces = append(nonces, n)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range nonces {
		if _, err := msg.Settle(ctx, tx, msg.SettleInput{Nonce: n, Outcome: msg.NotSent, Error: "run " + runID + ": " + why,
			RetryAt: time.Now()}, msgActor(a)); err != nil {
			return err
		}
	}
	return nil
}
