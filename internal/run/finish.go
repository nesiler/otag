package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/workspace"
)

// DoneResult reports `otag done`.
type DoneResult struct {
	Run          string   `json:"run"`
	Task         string   `json:"task"`
	TaskStatus   string   `json:"task_status"`
	Summary      string   `json:"summary"`
	Commit       string   `json:"commit,omitempty"`
	CommitsAhead int      `json:"commits_ahead"`
	Dirty        bool     `json:"dirty"`                 // uncommitted changes were left in the workspace
	ReviewTask   string   `json:"review_task,omitempty"` // created when the project has a review profile
	Closed       []string `json:"closed_panes"`          // windows killed after the run ended
	// CleanupError: something of the run could not be killed or verified gone; the
	// daemon retries (runs.cleanup_pending).
	CleanupError string `json:"cleanup_error,omitempty"`
}

type head struct {
	sha   string
	dirty bool
	ahead int
}

func readHead(ctx context.Context, r *Run) head {
	var h head
	if r.Workspace == nil || r.Workspace.Path == "" {
		return h
	}
	sha, dirty, err := workspace.Head(ctx, r.Workspace.Path)
	if err != nil {
		return h
	}
	h.sha, h.dirty = sha, dirty
	h.ahead, _ = workspace.CommitsSince(ctx, r.Workspace.Path, r.Workspace.BaseSHA)
	return h
}

// Done finishes a live run: records the summary and the workspace HEAD, ends the run
// (done), moves the task to review (creating a linked review task) when the project has
// a review profile, else to done, and closes the run's windows. Transcripts stay with
// the harness, so the session can be resumed later. A review run's task goes to done:
// a review is never reviewed again.
func (m *Manager) Done(ctx context.Context, runID, summary string) (*DoneResult, error) {
	r, h, err := m.prepareDone(ctx, runID, summary)
	if err != nil {
		return nil, err
	}
	var res *DoneResult
	err = m.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		res, err = m.complete(ctx, tx, r, h, summary)
		return err
	})
	if err != nil {
		return nil, err
	}
	var cerr error
	if res.Closed, cerr = m.Cleanup(ctx, runID); cerr != nil {
		res.CleanupError = oneLine(cerr.Error())
	}
	return res, nil
}

// prepareDone checks that runID can be done and reads its workspace HEAD (git runs
// outside any transaction).
func (m *Manager) prepareDone(ctx context.Context, runID, summary string) (*Run, head, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, head{}, apperr.New(apperr.Invalid, "summary is required: otag done \"what you did\"")
	}
	r, err := Get(ctx, m.DB, runID)
	if err != nil {
		return nil, head{}, err
	}
	if r.Status == Stopping {
		// e.g. `otag task cancel` while the agent was finishing: the stop wins.
		return nil, head{}, apperr.New(apperr.InvalidTransition, "run %s is being stopped (task %s is %s); nothing to do",
			r.ID, r.TaskID, r.TaskStatus)
	}
	if !live(r.Status) {
		return nil, head{}, apperr.New(apperr.InvalidTransition, "run %s is %s %s; only a started run can be done",
			r.ID, r.Status, r.EndedReason)
	}
	return r, readHead(ctx, r), nil
}

// complete is the transaction part of Done, for callers that already hold a write
// transaction (`otag review` finishes the review run in its own): it never opens one.
// It records the summary and HEAD h of run r (read by prepareDone), ends the run as
// done, closes its task's open ask, and moves the task to review (with a new review
// task) or done (telling the coordinator: NotifyDone). The caller closes the run's windows after the commit (Cleanup).
func (m *Manager) complete(ctx context.Context, tx *sql.Tx, r *Run, h head, summary string) (*DoneResult, error) {
	summary = strings.TrimSpace(summary)
	res := &DoneResult{Run: r.ID, Task: r.TaskID, Summary: summary, Commit: h.sha, CommitsAhead: h.ahead,
		Dirty: h.dirty, Closed: []string{}}
	row, err := load(ctx, tx, r.ID)
	if err != nil {
		return nil, err
	}
	if !live(row.Status) {
		return nil, apperr.New(apperr.InvalidTransition, "run %s is %s", r.ID, row.Status)
	}
	t, err := ownedTask(ctx, tx, row)
	if err != nil {
		return nil, err
	}
	if err := insertOutput(ctx, tx, r.ID, "summary", summary, nil); err != nil {
		return nil, err
	}
	if h.sha != "" {
		data := map[string]any{"dirty": h.dirty, "commits_ahead": h.ahead, "base_sha": r.Workspace.BaseSHA,
			"branch": r.Workspace.Branch}
		if err := insertOutput(ctx, tx, r.ID, "commit", h.sha, data); err != nil {
			return nil, err
		}
	}
	if err := closeWorkspace(ctx, tx, r.ID, h.sha); err != nil {
		return nil, err
	}
	if err := row.endSessions(ctx, tx, "done"); err != nil {
		return nil, err
	}
	if err := row.end(ctx, tx, m.Actor, ReasonDone, ""); err != nil {
		return nil, err
	}
	// A question still open is moot now.
	if err := msg.CloseAsks(ctx, tx, t.ID, msg.ClosedAskerDone, msgActor(m.Actor)); err != nil {
		return nil, err
	}
	var prefix, reviewProfile string
	if err := tx.QueryRowContext(ctx, `SELECT p.prefix, coalesce(rp.name, '') FROM projects p
		LEFT JOIN profiles rp ON rp.id = p.review_profile_id AND rp.retired_at IS NULL WHERE p.id = ?`,
		row.ProjectID).Scan(&prefix, &reviewProfile); err != nil {
		return nil, err
	}
	if reviewProfile, err = taskReviewProfile(ctx, tx, t, reviewProfile); err != nil {
		return nil, err
	}
	if isReview(r, t) {
		reviewProfile = "" // a review is never reviewed again
	}
	to := task.Done
	if reviewProfile != "" {
		to = task.Review
	}
	if _, err := task.Transition(ctx, tx, t.ID, to, "", 0, m.Actor.task(row.ID)); err != nil {
		return nil, err
	}
	res.TaskStatus = to
	if reviewProfile == "" {
		// Done: tell the coordinator it is ready to deliver (not for a review task).
		return res, NotifyDone(ctx, tx, t.ID, m.Actor)
	}
	rt, err := task.Add(ctx, tx, task.AddInput{
		ProjectID: row.ProjectID, Prefix: prefix, Title: "Review: " + t.Title, Profile: reviewProfile,
		Body: reviewBody(t, r, h, summary), Deps: []task.DepRef{{Kind: "review_of", On: t.ID}},
	}, m.Actor.task(row.ID))
	if err != nil {
		return nil, fmt.Errorf("create review task: %w", err)
	}
	res.ReviewTask = rt.ID
	return res, nil
}

// taskReviewProfile is the review profile of task t: its override (`--review`), else
// projectProfile (the project's review_profile). --review none disables review; an
// override whose profile was retired falls back to the project's, like a retired
// project review profile disables review.
func taskReviewProfile(ctx context.Context, tx *sql.Tx, t *task.Task, projectProfile string) (string, error) {
	switch t.Review {
	case "":
		return projectProfile, nil
	case task.ReviewNone:
		return "", nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM profiles WHERE name = ? AND retired_at IS NULL`, t.Review).
		Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return projectProfile, nil
	}
	return t.Review, nil
}

// isReview reports whether run r reviews another task: its role, or its task's
// review_of edge (a run started before roles were set).
func isReview(r *Run, t *task.Task) bool {
	if r.Role == "review" {
		return true
	}
	for _, d := range t.Deps {
		if d.Kind == "review_of" {
			return true
		}
	}
	return false
}

// ownedTask returns the run's task if the run still owns it and it is in progress.
func ownedTask(ctx context.Context, tx *sql.Tx, row *row) (*task.Task, error) {
	if row.TaskID == "" {
		return nil, apperr.New(apperr.InvalidTransition, "run %s has no task", row.ID)
	}
	t, err := task.Get(ctx, tx, row.TaskID)
	if err != nil {
		return nil, err
	}
	if t.CurrentRunID != row.ID || !slices.Contains([]string{task.Running, task.Waiting}, t.Status) {
		return nil, apperr.New(apperr.InvalidTransition, "task %s is %s (current run %q), not in progress by run %s",
			t.ID, t.Status, t.CurrentRunID, row.ID)
	}
	return t, nil
}

func reviewBody(t *task.Task, r *Run, h head, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the result of run %s for task %s (%s).\n\n", r.ID, t.ID, t.Title)
	if w := r.Workspace; w != nil {
		if w.Branch != "" {
			fmt.Fprintf(&b, "Branch: %s (%d commit(s))\nBase: %s\nHead: %s\n", w.Branch, h.ahead, w.BaseSHA, h.sha)
			if w.BaseSHA != "" && h.sha != "" {
				fmt.Fprintf(&b, "Diff: git diff %s...%s\n", w.BaseSHA, h.sha)
			}
		} else {
			fmt.Fprintf(&b, "Head: %s\n", h.sha)
		}
		fmt.Fprintf(&b, "Workspace: %s\n", w.Path)
		if w.Mode == workspace.Worktree && h.sha != "" {
			b.WriteString("Your review worktree is a detached checkout of the head commit.\n")
		}
	}
	fmt.Fprintf(&b, "\nSummary from the agent:\n%s\n", summary)
	if h.dirty {
		b.WriteString("\nNote: uncommitted changes were left in the workspace.\n")
	}
	b.WriteString("\nAccept or reject with a reason; do not change the reviewed branch.\n")
	return b.String()
}

// Fail ends a run as failed, puts its task back in the queue and closes its windows,
// so nothing keeps writing to the worktree the next attempt will use.
func (m *Manager) Fail(ctx context.Context, runID, reason string) (*Run, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, apperr.New(apperr.Invalid, "reason is required: otag fail \"why\"")
	}
	r, err := Get(ctx, m.DB, runID)
	if err != nil {
		return nil, err
	}
	if r.Status == Ended {
		return nil, apperr.New(apperr.InvalidTransition, "run %s already ended (%s)", r.ID, r.EndedReason)
	}
	h := readHead(ctx, r)
	err = m.DB.Tx(ctx, func(tx *sql.Tx) error {
		row, err := load(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := closeWorkspace(ctx, tx, runID, h.sha); err != nil {
			return err
		}
		if err := row.endSessions(ctx, tx, "failed"); err != nil {
			return err
		}
		if err := row.end(ctx, tx, m.Actor, ReasonFailed, reason); err != nil {
			return err
		}
		if err := closeOwnAsk(ctx, tx, row, msg.ClosedAskerFailed, m.Actor); err != nil {
			return err
		}
		return row.releaseTask(ctx, tx, m.Actor, "run "+runID+" failed: "+oneLine(reason))
	})
	if err != nil {
		return nil, err
	}
	_, _ = m.Cleanup(ctx, runID) // a failure is kept on the run (cleanup_error) and retried
	return Get(ctx, m.DB, runID)
}

// closeOwnAsk closes the open ask of the run's task (msg.CloseAsks) while the run still
// owns the task: its question is moot once it gave up.
func closeOwnAsk(ctx context.Context, tx *sql.Tx, row *row, reason string, a Actor) error {
	if row.TaskID == "" {
		return nil
	}
	var current sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT current_run_id FROM tasks WHERE id = ?`, row.TaskID).Scan(&current); err != nil {
		return err
	}
	if current.String != row.ID {
		return nil
	}
	return msg.CloseAsks(ctx, tx, row.TaskID, reason, msgActor(a))
}

// StopResult reports `otag run stop`.
type StopResult struct {
	Run    *Run          `json:"run"`
	Before tmux.Liveness `json:"before"` // pane state before stopping
	Killed []string      `json:"killed"` // panes whose windows were killed
	// CleanupError: a process of the run could not be killed or verified gone after
	// the run ended; the daemon retries (runs.cleanup_pending).
	CleanupError string `json:"cleanup_error,omitempty"`
}

// Stop kills the run's windows. A live run ends as stopped (crashed when its pane was
// already dead) and its task goes back to ready; on an ended run it only cleans up panes.
func (m *Manager) Stop(ctx context.Context, ref, reason string) (*StopResult, error) {
	return m.StopWith(ctx, ref, StopOptions{Reason: reason})
}

// StopOptions tune StopWith.
type StopOptions struct {
	Reason string // why; stored as runs.error and on the task
	// EndReason replaces the ended_reason of a live run (default: stopped, or crashed
	// when the pane was already dead), e.g. stalled for the daemon's watchdog.
	EndReason string
	Cause     string // data.cause on the ending run.status event (launch_failed causes)
	// Expect, when set, is a precondition checked inside the transaction that moves
	// the run to stopping: the run must still have this status and heartbeat. If a
	// hook changed it after the caller looked (a new event, waiting, ended), StopWith
	// changes nothing and returns ErrChanged. The daemon decides on a snapshot and
	// uses it so a run that just came back to life is never killed.
	Expect *Expect
	// Guard, when set, runs inside that same transaction after Expect: an error stops
	// nothing and is returned (Park checks the ask is still open there).
	Guard func(tx *sql.Tx) error
}

// Expect is the state a caller of StopWith saw.
type Expect struct {
	Status      string
	HeartbeatAt string // runs.heartbeat_at as read ("" = none)
	NoPane      bool   // no main pane was recorded (a launch that may still record one)
}

// ErrChanged means StopOptions.Expect no longer held: nothing was stopped.
var ErrChanged = apperr.Retryable(apperr.Conflict, "run changed since it was checked; not stopped")

// StopWith is Stop with options: the adapter's Stopper gets a chance to exit
// gracefully, then every window of the run is killed and a live run ends.
func (m *Manager) StopWith(ctx context.Context, ref string, o StopOptions) (*StopResult, error) {
	reason := o.Reason
	r, err := Resolve(ctx, m.DB, ref)
	if err != nil {
		return nil, err
	}
	res := &StopResult{Killed: []string{}}
	var pane string
	var pid int
	if r.Session != nil {
		pane, pid = r.Session.TmuxPane, r.Session.PID
	}
	if o.Expect != nil && r.Status == Ended {
		return nil, ErrChanged
	}
	res.Before = m.Tmux.Check(ctx, pane, pid)
	endReason := ReasonStopped
	if res.Before.State == tmux.Exited || res.Before.State == tmux.Gone {
		endReason = ReasonCrashed
	}
	dead := endReason == ReasonCrashed
	if o.EndReason != "" {
		endReason = o.EndReason
	}
	if r.Status != Ended {
		// From now on a launch in progress kills what it starts (see launcher.record).
		err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
			row, err := load(ctx, tx, r.ID)
			if err != nil {
				return err
			}
			if o.Expect != nil {
				var hb string
				if err := tx.QueryRowContext(ctx, `SELECT coalesce(heartbeat_at, '') FROM runs WHERE id = ?`, r.ID).
					Scan(&hb); err != nil {
					return err
				}
				if row.Status != o.Expect.Status || (o.Expect.HeartbeatAt != "*" && hb != o.Expect.HeartbeatAt) {
					return ErrChanged
				}
				if o.Expect.NoPane {
					var panes int
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE run_id = ? AND tmux_pane IS NOT NULL`,
						r.ID).Scan(&panes); err != nil {
						return err
					}
					if panes > 0 {
						return ErrChanged
					}
				}
			}
			if row.Status == Ended {
				return nil
			}
			if o.Guard != nil {
				if err := o.Guard(tx); err != nil {
					return err
				}
			}
			// The stop intent is persisted with the status: if this process dies or the
			// cleanup below fails, ResumeStop (the daemon) finishes it.
			intent := map[string]any{"reason": reason, "end_reason": endReason}
			if o.Cause != "" {
				intent["cause"] = o.Cause
			}
			return row.setStatus(ctx, tx, m.Actor, Stopping, intent)
		})
		if err != nil {
			return nil, err
		}
	}
	if ad, ok := m.Adapters.Get(r.Harness); ok && res.Before.State == tmux.Alive {
		if st, ok := ad.(adapter.Stopper); ok {
			sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			t := adapter.Target{RunID: r.ID, Pane: m.Tmux.Control(pane)}
			if r.Session != nil {
				t.HarnessSessionID, t.ServerURL = r.Session.HarnessSessionID, r.Session.ServerURL
			}
			if st.Stop(sctx, t) == nil {
				m.Tmux.WaitGone(sctx, pane, 3*time.Second)
			}
			cancel()
		}
	}
	// Windows that cannot be killed keep the run stopping (ResumeStop retries); a process
	// that survives its window does not hold up the end, it stays a cleanup obligation.
	killed, complete, werr, perr := m.reap(ctx, r.ID, func(string) bool { return true })
	res.Killed = killed
	if werr != nil {
		return nil, apperr.New(apperr.Unavailable, "%v", werr)
	}
	var extra map[string]any
	if o.Cause != "" {
		extra = map[string]any{"cause": o.Cause}
	}
	h := readHead(ctx, r)
	err = m.DB.Tx(ctx, func(tx *sql.Tx) error {
		row, err := load(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if err := row.endSessions(ctx, tx, "stopped"); err != nil {
			return err
		}
		if row.Status == Ended {
			return nil
		}
		if err := closeWorkspace(ctx, tx, r.ID, h.sha); err != nil {
			return err
		}
		text := reason
		if dead && text == "" {
			text = "pane " + res.Before.State + ": " + res.Before.Detail
		}
		if err := row.end(ctx, tx, m.Actor, endReason, text, extra); err != nil {
			return err
		}
		if err := requeueLaunchPrompt(ctx, tx, r.ID, "ended "+endReason+" before it started working", m.Actor); err != nil {
			return err
		}
		if res.Before.ExitStatus != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET exit_code = ? WHERE id = ?`, *res.Before.ExitStatus, r.ID); err != nil {
				return err
			}
		}
		if endReason == ReasonParked {
			return nil // the task keeps waiting for its answer; Resume continues it
		}
		why := "run " + r.ID + " " + endReason
		if reason != "" {
			why += ": " + oneLine(reason)
		}
		return row.releaseTask(ctx, tx, m.Actor, why)
	})
	if err != nil {
		return nil, err
	}
	if perr != nil {
		res.CleanupError = oneLine(perr.Error())
	}
	if complete || perr != nil {
		if err := m.settleCleanup(ctx, r.ID, perr); err != nil {
			return nil, err
		}
	}
	if res.Run, err = Get(ctx, m.DB, r.ID); err != nil {
		return nil, err
	}
	return res, nil
}

// StopTaskRun is the run side of cancelling or blocking a task (`otag task cancel`,
// `otag task block`), called inside the caller's transaction that changes the task: the
// task's active run, if any, moves to stopping with the intent to end as stopped, so
// the task change and the stop commit together. It returns that run's id ("" when the
// task has no active run). The caller then finishes the stop with StopWith (Expect
// Status stopping); if it dies first, the daemon does (ResumeStop). Meanwhile the
// run's `otag done` is refused, and when the run ends it leaves the task alone: the
// task is no longer in progress.
//
// A cancelled task's open ask is closed too (its parked run, if any, has no live run
// left to stop).
func StopTaskRun(ctx context.Context, tx *sql.Tx, taskID, reason string, a Actor) (string, error) {
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if status == task.Cancelled {
		if err := msg.CloseAsks(ctx, tx, taskID, msg.ClosedCancelled, msgActor(a)); err != nil {
			return "", err
		}
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE task_id = ? AND status <> 'ended'`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	r, err := load(ctx, tx, id)
	if err != nil {
		return "", err
	}
	return id, r.setStatus(ctx, tx, a, Stopping, map[string]any{"reason": reason, "end_reason": ReasonStopped})
}

// ResumeStop finishes a run left in stopping (the process that stopped it died, or
// killing its windows failed) with the intent recorded on its run.status → stopping
// event: the same reason, end reason and cause.
func (m *Manager) ResumeStop(ctx context.Context, runID string) (*StopResult, error) {
	var data string
	err := m.DB.QueryRowContext(ctx, `SELECT data FROM events WHERE run_id = ? AND kind = 'run.status'
		AND to_status = 'stopping' ORDER BY id DESC LIMIT 1`, runID).Scan(&data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var intent struct {
		Reason    string `json:"reason"`
		EndReason string `json:"end_reason"`
		Cause     string `json:"cause"`
	}
	_ = json.Unmarshal([]byte(data), &intent)
	return m.StopWith(ctx, runID, StopOptions{Reason: intent.Reason, EndReason: intent.EndReason, Cause: intent.Cause,
		Expect: &Expect{Status: Stopping, HeartbeatAt: "*"}})
}
