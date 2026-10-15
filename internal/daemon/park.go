package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// lifecycle runs the phase 4 lifecycle loops: park idle askers, resume parked tasks
// whose answer arrived, nudge idle children. runs is the tick's snapshot (nil when
// listing failed: only the resume loop runs then).
func (d *Daemon) lifecycle(ctx context.Context, runs []*run.Run) {
	if runs != nil {
		if err := d.watchAsks(ctx, runs); err != nil {
			d.logf("park: %v", err)
		}
	}
	if err := d.watchParked(ctx); err != nil {
		d.logf("resume: %v", err)
	}
	if runs != nil {
		if err := d.watchIdle(ctx, runs); err != nil {
			d.logf("idle: %v", err)
		}
	}
}

// watchAsks parks idle runs whose open ask asked for it (`otag ask --park`), or that
// waited for their answer longer than [messages] park_after: the session closes
// (run.Manager.Park, ended parked) and the task keeps waiting; watchParked resumes it
// once the answer is queued. The run must still be idle with the heartbeat the
// snapshot saw, and the ask still open, when the stop begins.
func (d *Daemon) watchAsks(ctx context.Context, runs []*run.Run) error {
	asks, err := msg.OpenAsks(ctx, d.DB, msg.AskFilter{})
	if err != nil {
		return err
	}
	byID := map[string]*run.Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	now := d.now()
	parks := 0
	for _, a := range asks {
		if parks >= parkLimit {
			break
		}
		r := byID[a.Asker]
		if r == nil || r.Status != run.Idle || r.Role == "coordinator" || r.Dead() {
			continue
		}
		why := fmt.Sprintf("parked: waiting for an answer to #%d", a.Message.ID)
		if !a.Message.Park {
			age := now.Sub(parseTime(a.Message.CreatedAt))
			if d.Config.ParkAfter <= 0 || age < d.Config.ParkAfter {
				continue
			}
			why = fmt.Sprintf("parked: question #%d unanswered for %s", a.Message.ID, age.Round(time.Minute))
		}
		parks++
		res, err := d.Runs.Park(ctx, r.ID, run.ParkOptions{Reason: why,
			Expect: &run.Expect{Status: r.Status, HeartbeatAt: r.HeartbeatAt}, SkipDispatched: true})
		if errors.Is(err, run.ErrChanged) {
			continue
		}
		if err != nil {
			if apperr.Code(err) == apperr.InvalidTransition {
				continue
			}
			d.logf("park %s: %v", r.ID, err)
			continue
		}
		if res.Run.EndedReason != run.ReasonParked {
			continue
		}
		d.logf("%s: %s", r.ID, why)
		d.notify(ctx, r, "run.parked", notify.Info, "run "+r.ID+" parked",
			fmt.Sprintf("Task %s waits for the answer to question #%d; it resumes when the answer arrives (otag reply %d \"...\").",
				r.TaskID, a.Message.ID, a.Message.ID))
	}
	return nil
}

// resumeLaunches bounds the resumes of one tick (each launches a harness).
const resumeLaunches = 1

// parkLimit bounds the parks of one tick.
const parkLimit = 5

// watchParked resumes parked tasks whose answer is queued (run.Manager.Resume: same
// session, same profile revision and workspace, an available account). A resume that
// is blocked leaves the task waiting, in `otag needs`, with one notification per parked
// run; it is tried again after a back-off (30s doubling up to 10 min), since a cooldown
// ends by itself. Nothing starts a fresh run instead.
func (d *Daemon) watchParked(ctx context.Context) error {
	if d.Runs == nil {
		return nil
	}
	now := d.now()
	var skip []string
	for id, b := range d.resumeBackoff {
		if now.Before(b.next) {
			skip = append(skip, id)
		}
	}
	cands, err := run.ParkedWithAnswer(ctx, d.DB, 20, skip...)
	if err != nil {
		return err
	}
	launched := 0
	seen := map[string]bool{}
	for _, id := range skip {
		seen[id] = true
	}
	for _, c := range cands {
		seen[c.TaskID] = true
		if b, ok := d.resumeBackoff[c.TaskID]; ok && now.Before(b.next) {
			continue
		}
		if launched >= resumeLaunches {
			continue
		}
		launched++
		res, err := d.Runs.Resume(ctx, c.TaskID)
		if err == nil {
			delete(d.resumeBackoff, c.TaskID)
			d.logf("%s: resumed %s as %s", c.TaskID, c.RunID, res.Run.ID)
			d.notify(ctx, res.Run, "run.resumed", notify.Info, "task "+c.TaskID+" resumed",
				fmt.Sprintf("The answer arrived; run %s continues the session of %s.", res.Run.ID, c.RunID))
			continue
		}
		if t, terr := task.Get(ctx, d.DB, c.TaskID); terr == nil && (t.Status != task.Waiting || t.CurrentRunID != c.RunID) {
			// Someone else resumed or moved it meanwhile (otag run resume, a task edit).
			delete(d.resumeBackoff, c.TaskID)
			d.logf("%s: resume skipped, the task is %s now", c.TaskID, t.Status)
			continue
		}
		b := d.resumeBackoff[c.TaskID]
		b.n++
		b.next = now.Add(min(30*time.Second<<(b.n-1), 10*time.Minute))
		if d.resumeBackoff == nil {
			d.resumeBackoff = map[string]startRetry{}
		}
		d.resumeBackoff[c.TaskID] = b
		d.logf("%s: resume of %s: %v", c.TaskID, c.RunID, err)
		var rb *run.ResumeBlocked
		body := fmt.Sprintf("%v. The answer stays queued for the task (otag needs). Fix it and run `otag run resume %s`, "+
			"or move the task to ready (otag task edit %s --status ready) for a fresh run that gets the answer.",
			err, c.TaskID, c.TaskID)
		if !errors.As(err, &rb) {
			// A run was created and its launch failed: the task went back to ready.
			body = fmt.Sprintf("Task %s cannot resume: %v.", c.TaskID, err)
		}
		if err := d.notifyOnce(ctx, c, "run.resume_blocked", "task "+c.TaskID+" could not resume", body); err != nil {
			d.logf("notification for %s: %v", c.TaskID, err)
		}
	}
	for id := range d.resumeBackoff {
		if !seen[id] {
			delete(d.resumeBackoff, id)
		}
	}
	return nil
}

// notifyOnce adds a warning about parked run c.RunID unless one of this kind with the same body exists.
func (d *Daemon) notifyOnce(ctx context.Context, c run.Resumable, kind, title, body string) error {
	var n int
	if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE run_id = ? AND kind = ? AND body = ?`,
		c.RunID, kind, body).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := notify.Add(ctx, d.DB, notify.New{ProjectID: c.ProjectID, TaskID: c.TaskID, RunID: c.RunID, Kind: kind,
		Severity: notify.Warn, Title: title, Body: body})
	return err
}

// maxIdleNudges is how many reminders an idle child gets before the human is told.
const maxIdleNudges = 2

// nudgeText is the reminder an idle child gets.
const reviewNudgeText = "You stopped without a verdict on review task %s. Review the work, then run " +
	"`otag review <task> --accept` or `otag review <task> --reject \"reason\"`. " +
	"If you are stuck: `otag ask \"question\"`. Otherwise continue."

const nudgeText = "You stopped without finishing task %s. If it is done: commit, then run `otag done \"summary\"`. " +
	"If you are stuck: `otag ask \"question\"`. Otherwise continue."

// watchIdle reminds child and review runs that sit idle for [messages] idle_after
// without an open ask (and with nothing queued for them): a system command message,
// at most maxIdleNudges per run (spaced idle_after apart), delivered like any message.
// After that the human gets one run.idle notification (`otag needs` lists such runs too).
func (d *Daemon) watchIdle(ctx context.Context, runs []*run.Run) error {
	after := d.Config.IdleAfter
	if after <= 0 {
		return nil
	}
	now := d.now()
	for _, r := range runs {
		if r.Status != run.Idle || (r.Role != "child" && r.Role != "review") || r.TaskID == "" || r.Dead() {
			continue
		}
		var since, lastNudge string
		var nudges, pending int
		var open bool
		err := d.DB.QueryRowContext(ctx, `
SELECT coalesce((SELECT max(created_at) FROM events WHERE run_id = ? AND kind = 'run.status' AND to_status = 'idle'), ''),
       (SELECT count(*) FROM messages WHERE client_msg_id LIKE 'nudge:' || ? || ':%'),
       coalesce((SELECT max(created_at) FROM messages WHERE client_msg_id LIKE 'nudge:' || ? || ':%'), ''),
       (SELECT count(*) FROM message_deliveries d WHERE (d.run_id = ? OR (d.task_id = ? AND d.run_id IS NULL))
          AND d.state IN ('queued', 'dispatched')
          AND NOT (d.state = 'queued' AND EXISTS (SELECT 1 FROM messages m WHERE m.id = d.message_id
                                                  AND m.client_msg_id LIKE 'nudge:' || ? || ':%'))),
       EXISTS (SELECT 1 FROM messages WHERE task_id = ? AND ask = 1 AND closed_at IS NULL)`,
			r.ID, r.ID, r.ID, r.ID, r.TaskID, r.ID, r.TaskID).Scan(&since, &nudges, &lastNudge, &pending, &open)
		if err != nil {
			return err
		}
		idle := latest(since, lastNudge)
		if open || pending > 0 || idle.IsZero() || now.Sub(idle) < after {
			continue
		}
		if nudges >= maxIdleNudges {
			var n int
			if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE run_id = ? AND kind = 'run.idle'`,
				r.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				d.notify(ctx, r, "run.idle", notify.Warn, "run "+r.ID+" is idle",
					fmt.Sprintf("Idle for %s after %d reminders, without otag done or a question. Task %s; attach: otag run attach %s.",
						now.Sub(idle).Round(time.Minute), nudges, r.TaskID, r.ID))
			}
			continue
		}
		text := nudgeText
		if r.Role == "review" {
			text = reviewNudgeText
		}
		err = d.DB.Tx(ctx, func(tx *sql.Tx) error {
			_, _, err := msg.Send(ctx, tx, msg.SendInput{ProjectID: r.ProjectID, ClientMsgID: fmt.Sprintf("nudge:%s:%d", r.ID, nudges+1),
				Kind: msg.KindCommand, From: msg.Sender{Kind: msg.SenderSystem, Actor: Actor.Name}, TaskID: r.TaskID,
				Body: fmt.Sprintf(text, r.TaskID), To: []msg.Recipient{{Kind: msg.ToRun, Ref: r.ID}}},
				msg.Actor{Name: Actor.Name, Source: Actor.Source})
			return err
		})
		if err != nil {
			d.logf("nudge %s: %v", r.ID, err)
			continue
		}
		d.logf("%s: idle for %s, reminder %d sent", r.ID, now.Sub(idle).Round(time.Second), nudges+1)
	}
	return nil
}
