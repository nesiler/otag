package run

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/task"
)

// Review verdicts (reviews.verdict).
const (
	Accept = "accept"
	Reject = "reject"
)

// ReviewInput is `otag review`.
type ReviewInput struct {
	Task    string // the reviewed task, or a review task (then the task it reviews)
	Verdict string // Accept | Reject
	Reason  string // required for Reject
	RunID   string // the calling run; "" = the human. A review run finishes with its verdict.
}

// ReviewResult reports `otag review`.
type ReviewResult struct {
	Review      int64       `json:"review"` // reviews.id
	Task        string      `json:"task"`   // the reviewed task
	TaskStatus  string      `json:"task_status"`
	ReviewedRun string      `json:"reviewed_run,omitempty"`
	Profile     string      `json:"profile,omitempty"` // the reviewed run's profile
	Verdict     string      `json:"verdict"`
	Reason      string      `json:"reason"`
	Cancelled   []string    `json:"cancelled_review_tasks,omitempty"` // review tasks a human review made moot
	Done        *DoneResult `json:"done,omitempty"`                   // the review run, finished with its verdict
}

// Review records a verdict on a task in review, in one transaction: a reviews row
// linked to the reviewed task's latest done run (and so to its profile), and the task
// to done (accept; ready to deliver, so the coordinator is told: NotifyDone) or back to
// ready (reject, with the reason; its next run sees it in the prior-attempt summary).
// Called from a review run, that run also finishes (done, through the same completion
// as `otag done`, never creating another review). Called by
// the human or the coordinator, review tasks of the task that have not started are
// cancelled; one with a live run is a CONFLICT (it is reviewing).
func (m *Manager) Review(ctx context.Context, in ReviewInput) (*ReviewResult, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	switch in.Verdict {
	case Accept:
	case Reject:
		if in.Reason == "" {
			return nil, apperr.New(apperr.Invalid, "a rejection needs a reason: otag review <task> --reject \"why\"")
		}
	default:
		return nil, apperr.New(apperr.Invalid, "verdict %q: want accept or reject", in.Verdict)
	}
	summary := "review: " + in.Verdict
	if in.Reason != "" {
		summary += ": " + in.Reason
	}
	var caller *Run
	var h head
	if in.RunID != "" {
		r, err := Get(ctx, m.DB, in.RunID)
		if err != nil {
			return nil, err
		}
		switch r.Role {
		case "review":
			if caller, h, err = m.prepareDone(ctx, in.RunID, summary); err != nil {
				return nil, err
			}
		case "coordinator": // reviews like the human
		default:
			return nil, apperr.New(apperr.Forbidden, "run %s is a %s run; only a review run, the coordinator or the human reviews",
				r.ID, r.Role)
		}
	}
	res := &ReviewResult{Verdict: in.Verdict, Reason: in.Reason}
	err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		t, err := task.Get(ctx, tx, in.Task)
		if err != nil {
			return err
		}
		if t.Status != task.Review {
			for _, d := range t.Deps {
				if d.Kind == "review_of" {
					if t, err = task.Get(ctx, tx, d.TaskID); err != nil {
						return err
					}
					break
				}
			}
		}
		if t.Status != task.Review {
			return apperr.New(apperr.InvalidTransition, "task %s is %s, not in review", t.ID, t.Status)
		}
		res.Task = t.ID
		var reviewed sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT r.id, p.name FROM runs r JOIN profiles p ON p.id = r.profile_id
			WHERE r.task_id = ? AND r.status = 'ended' AND r.ended_reason = 'done' ORDER BY r.attempt DESC LIMIT 1`, t.ID).
			Scan(&reviewed, &res.Profile)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res.ReviewedRun = reviewed.String
		var reviewTask, reviewerRun sql.NullString
		actor := ""
		if caller != nil {
			if !reviewedBy(t.Dependents, caller.TaskID) {
				return apperr.New(apperr.Forbidden, "run %s reviews task %s's review task, not task %s", caller.ID,
					caller.TaskID, t.ID)
			}
			reviewTask, reviewerRun = nullStr(caller.TaskID), nullStr(caller.ID)
		} else {
			actor = m.Actor.Name
			if res.Cancelled, err = m.cancelReviewTasks(ctx, tx, t); err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO reviews (review_task_id, reviewed_task_id, reviewed_run_id,
			reviewer_run_id, reviewer_actor, verdict, reason) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			reviewTask, t.ID, reviewed, reviewerRun, actor, in.Verdict, in.Reason).Scan(&res.Review); err != nil {
			return err
		}
		to, why := task.Done, ""
		if in.Verdict == Reject {
			to, why = task.Ready, "review rejected: "+oneLine(in.Reason)
		}
		if _, err := task.Transition(ctx, tx, t.ID, to, why, 0, m.Actor.task(in.RunID)); err != nil {
			return err
		}
		res.TaskStatus = to
		if to == task.Done {
			if err := NotifyDone(ctx, tx, t.ID, m.Actor); err != nil {
				return err
			}
		}
		if caller != nil {
			res.Done, err = m.complete(ctx, tx, caller, h, summary)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if res.Done != nil {
		var cerr error
		if res.Done.Closed, cerr = m.Cleanup(ctx, caller.ID); cerr != nil {
			res.Done.CleanupError = oneLine(cerr.Error())
		}
	}
	return res, nil
}

// reviewedBy reports whether deps has a review_of edge from task id.
func reviewedBy(deps []task.Dep, id string) bool {
	for _, d := range deps {
		if d.Kind == "review_of" && d.TaskID == id {
			return true
		}
	}
	return false
}

// cancelReviewTasks cancels the open review tasks of t that no run works on (a human
// or the coordinator reviewed it instead); one with a live run is a CONFLICT.
func (m *Manager) cancelReviewTasks(ctx context.Context, tx *sql.Tx, t *task.Task) ([]string, error) {
	var out []string
	for _, d := range t.Dependents {
		if d.Kind != "review_of" || task.Terminal(d.Status) {
			continue
		}
		var live string
		err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE task_id = ? AND status <> 'ended' LIMIT 1`, d.TaskID).Scan(&live)
		if err == nil {
			return nil, apperr.New(apperr.Conflict, "review task %s of %s is being worked on by run %s", d.TaskID, t.ID, live)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if _, err := task.Transition(ctx, tx, d.TaskID, task.Cancelled, "reviewed by "+m.Actor.Name, 0, m.Actor.task("")); err != nil {
			return nil, err
		}
		if err := msg.CloseAsks(ctx, tx, d.TaskID, msg.ClosedCancelled, msgActor(m.Actor)); err != nil {
			return nil, err
		}
		out = append(out, d.TaskID)
	}
	return out, nil
}
