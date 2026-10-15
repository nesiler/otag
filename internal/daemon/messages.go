package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/run"
)

// The messaging step (docs/MESSAGES.md): every tick it sweeps proof timeouts and
// expiries, notifies the human of messages addressed to them and of deliveries whose
// outcome is unknown, and dispatches due injected deliveries into idle (or, where the
// harness can steer, running) runs. Busy Claude runs take theirs through their hooks.

// MessagesBudget bounds the dispatch part of one tick, so the other loops stay
// responsive: no attempt starts with less than minSendBudget of it left, and each
// adapter call gets at most what is left (a hung harness server cannot hold the tick;
// a call cut short has an unknown outcome, like any send that never answered).
const MessagesBudget = 5 * time.Second

// minSendBudget is the least time an adapter call is started with.
const minSendBudget = time.Second

// ProofTimeouts: a delivery dispatched longer ago than this on the channel without a
// proof becomes outcome_unknown (never re-sent automatically). Hook context is proven
// as it is printed, a mod submit within seconds; a steer waits for a step boundary and
// a Stop block for the end of the continued turn.
var ProofTimeouts = map[string]time.Duration{
	adapter.ChanToolContext:   2 * time.Minute,
	adapter.ChanPromptContext: 2 * time.Minute,
	adapter.ChanModSubmit:     3 * time.Minute,
	adapter.ChanAPIStart:      10 * time.Minute,
	adapter.ChanAPISteer:      10 * time.Minute,
	adapter.ChanLaunchPrompt:  10 * time.Minute,
	adapter.ChanStopBlock:     30 * time.Minute,
}

// msgActor is the daemon in message events.
var msgActor = msg.Actor{Name: Actor.Name, Source: Actor.Source}

// messages is the tick's messaging step.
func (d *Daemon) messages(ctx context.Context) error {
	now := d.now()
	var errs []error
	err := d.DB.Tx(ctx, func(tx *sql.Tx) error {
		for ch, after := range ProofTimeouts {
			if _, err := msg.ProofTimeout(ctx, tx, now.Add(-after), ch, 500, msgActor); err != nil {
				return err
			}
		}
		res, err := msg.Expire(ctx, tx, now, 500, msgActor)
		if err == nil && res.Expired+res.Dead > 0 {
			d.logf("messages: %d expired, %d dead", res.Expired, res.Dead)
		}
		return err
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("sweep: %w", err))
	}
	if err := d.messageNotices(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("notices: %w", err))
	}
	if err := d.dispatch(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("dispatch: %w", err))
	}
	if d.Runs != nil {
		if err := run.SweepInboxes(ctx, d.DB, d.Runs.Getenv, now, 30*time.Second); err != nil {
			errs = append(errs, fmt.Errorf("inboxes: %w", err))
		}
	}
	return errors.Join(errs...)
}

// dispatch makes one attempt per due run an injected channel can serve now, least
// recently served first, until the budget is spent.
func (d *Daemon) dispatch(ctx context.Context, now time.Time) error {
	if d.Runs == nil || d.Runs.Adapters == nil {
		return nil
	}
	disp := &run.Dispatcher{DB: d.DB, Adapters: d.Runs.Adapters, Getenv: d.Runs.Getenv, Actor: msgActor}
	ids, err := msg.DueRuns(ctx, d.DB, msg.DueInput{Now: now, Limit: 50,
		Eligible: func(id string) bool { return disp.Eligible(ctx, id) }})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(MessagesBudget)
	for _, id := range ids {
		left := time.Until(deadline)
		if ctx.Err() != nil || left < minSendBudget {
			break
		}
		disp.Timeout = min(left, run.DeliverTimeout)
		ra := disp.Dispatch(ctx, id)
		if ra.Result != "skipped" {
			d.logf("%s: delivery %v by %s: %s %s", id, ra.Deliveries, ra.Channel, ra.Result, ra.Reason)
		}
	}
	return nil
}

// noticeWindow: older messages and outcomes (from before the daemon ran) get no
// notification; otag needs and otag inbox still list them.
const noticeWindow = time.Hour

// messageNotices adds one notification per message to the human (or to the
// coordinator role while none is live) and per delivery that ended outcome_unknown.
func (d *Daemon) messageNotices(ctx context.Context, now time.Time) error {
	since := now.Add(-noticeWindow).UTC().Format(db.TimeFormat)
	rows, err := d.DB.QueryContext(ctx, `
SELECT DISTINCT d.message_id FROM message_deliveries d JOIN messages m ON m.id = d.message_id
WHERE d.state = 'queued' AND m.created_at >= ?
  AND (d.recipient_kind = 'human' OR (d.recipient_kind = 'role' AND NOT EXISTS (
       SELECT 1 FROM runs r WHERE r.id = d.run_id AND r.status NOT IN ('stopping', 'parked', 'ended'))))
  AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.kind = 'msg.human' AND n.message_id = d.message_id)
ORDER BY d.message_id LIMIT 20`, since)
	if err != nil {
		return err
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return err
	}
	for _, id := range ids {
		m, err := msg.Get(ctx, d.DB, id)
		if err != nil {
			return err
		}
		sev, action := notify.Info, "otag inbox"
		if m.Kind == msg.KindQuestion {
			sev, action = notify.Warn, fmt.Sprintf("otag reply %d \"<answer>\"", m.ID)
		}
		task, runID := "", ""
		if m.Task != nil {
			task = *m.Task
		}
		if m.From.Run != nil {
			runID = *m.From.Run
		}
		if err := d.msgNotify(ctx, m.ProjectID, task, runID, m.ID, "msg.human", sev,
			"otag: "+msg.Headline(*m), oneLine(m.Body)+" → "+action); err != nil {
			return err
		}
	}

	rows, err = d.DB.QueryContext(ctx, `
SELECT d.id FROM message_deliveries d
WHERE d.state = 'outcome_unknown' AND d.updated_at >= ?
  AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.kind = 'msg.outcome_unknown' AND n.message_id = d.message_id
                  AND coalesce(n.run_id, '') = coalesce(d.run_id, ''))
ORDER BY d.id LIMIT 20`, since)
	if err != nil {
		return err
	}
	if ids, err = scanIDs(rows); err != nil {
		return err
	}
	for _, id := range ids {
		dl, err := msg.GetDelivery(ctx, d.DB, id)
		if err != nil {
			return err
		}
		runID, task, why := "", "", ""
		if dl.To.Run != nil {
			runID = *dl.To.Run
		}
		if dl.To.Task != nil {
			task = *dl.To.Task
		}
		if dl.LastError != nil {
			why = *dl.LastError + ". "
		}
		body := fmt.Sprintf("Message #%d to %s may or may not have arrived. %sCheck the session; to send it again: "+
			"otag send --resend %d", dl.MessageID, dl.To.Recipient, why, dl.ID)
		if err := d.msgNotify(ctx, dl.ProjectID, task, runID, dl.MessageID, "msg.outcome_unknown", notify.Warn,
			fmt.Sprintf("delivery %d: outcome unknown", dl.ID), body); err != nil {
			return err
		}
	}
	return nil
}

func scanIDs(rows *sql.Rows) ([]int64, error) {
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

// msgNotify adds a notification tied to message messageID (notify.Add has no message).
func (d *Daemon) msgNotify(ctx context.Context, projectID int64, taskID, runID string, messageID int64,
	kind, severity, title, body string) error {
	nullStr := func(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
	_, err := d.DB.ExecContext(ctx, `INSERT INTO notifications (project_id, task_id, run_id, message_id, kind, severity, title, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, projectID, nullStr(taskID), nullStr(runID), messageID, kind, severity, title, body)
	return err
}
