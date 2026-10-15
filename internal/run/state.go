package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/event"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/quota"
	"github.com/nesiler/otag/internal/task"
)

// Actor is who changes a run; recorded on events.
type Actor struct {
	Name   string // user:<name> | run:<id> | daemon
	Source string // events.source: cli | hook:<harness> | daemon
}

func (a Actor) source() string {
	if a.Source == "" {
		return "cli"
	}
	return a.Source
}

func (a Actor) task(runID string) task.Actor {
	return task.Actor{Name: a.Name, RunID: runID, Source: a.source()}
}

// row is the mutable core of a run, read inside a write transaction.
type row struct {
	ID, TaskID, Harness, Status string
	ProjectID, Version          int64
}

func load(ctx context.Context, q db.Querier, id string) (*row, error) {
	var r row
	err := q.QueryRowContext(ctx, `SELECT id, coalesce(task_id, ''), harness, status, project_id, version
		FROM runs WHERE id = ?`, id).Scan(&r.ID, &r.TaskID, &r.Harness, &r.Status, &r.ProjectID, &r.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.NotFound, "run %s not found", id)
	}
	return &r, err
}

func (r *row) record(ctx context.Context, tx *sql.Tx, a Actor, kind, from, to string, data any) error {
	_, _, err := event.Insert(ctx, tx, event.Event{
		Source: a.source(), Kind: kind, EntityType: "run", EntityID: r.ID, FromStatus: from, ToStatus: to,
		ProjectID: r.ProjectID, TaskID: r.TaskID, RunID: r.ID, Actor: a.Name, Data: data,
	})
	return err
}

// recordID records a run event without a status change and returns its id.
func (r *row) recordID(ctx context.Context, tx *sql.Tx, a Actor, kind string, data any) (int64, error) {
	id, _, err := event.Insert(ctx, tx, event.Event{
		Source: a.source(), Kind: kind, EntityType: "run", EntityID: r.ID,
		ProjectID: r.ProjectID, TaskID: r.TaskID, RunID: r.ID, Actor: a.Name, Data: data,
	})
	return id, err
}

// setStatus moves r to status `to` (CAS on version) and records run.status.
func (r *row) setStatus(ctx context.Context, tx *sql.Tx, a Actor, to string, extra map[string]any) error {
	if r.Status == to {
		return nil
	}
	now := db.Now()
	q := `UPDATE runs SET status = ?, version = version + 1`
	args := []any{to}
	if to == Starting {
		q += ", started_at = coalesce(started_at, ?)"
		args = append(args, now)
	}
	q += " WHERE id = ? AND version = ?"
	args = append(args, r.ID, r.Version)
	if err := cas(tx.ExecContext(ctx, q, args...)); err != nil {
		return fmt.Errorf("run %s: %w", r.ID, err)
	}
	from := r.Status
	r.Status, r.Version = to, r.Version+1
	return r.record(ctx, tx, a, "run.status", from, to, extra)
}

// end finishes r with reason; errText (optional) goes to runs.error, extra (optional)
// into the event data. The run's windows and processes become a cleanup obligation
// (runs.cleanup_pending, see cleanup.go) that the caller settles after the commit.
func (r *row) end(ctx context.Context, tx *sql.Tx, a Actor, reason, errText string, extra ...map[string]any) error {
	if r.Status == Ended {
		return apperr.New(apperr.InvalidTransition, "run %s already ended", r.ID)
	}
	err := cas(tx.ExecContext(ctx, `
UPDATE runs SET status = 'ended', ended_reason = ?, ended_at = ?, error = coalesce(?, error), version = version + 1,
                cleanup_pending = 1
WHERE id = ? AND version = ?`, reason, db.Now(), nullStr(errText), r.ID, r.Version))
	if err != nil {
		return fmt.Errorf("run %s: %w", r.ID, err)
	}
	from := r.Status
	r.Status, r.Version = Ended, r.Version+1
	data := map[string]any{"reason": reason}
	if errText != "" {
		data["error"] = errText
	}
	for _, m := range extra {
		for k, v := range m {
			data[k] = v
		}
	}
	return r.record(ctx, tx, a, "run.status", from, Ended, data)
}

// moveTask moves the run's task from one of `from` to `to` when the task still belongs
// to this run; anything else is left alone.
func (r *row) moveTask(ctx context.Context, tx *sql.Tx, a Actor, from []string, to, reason string) error {
	if r.TaskID == "" {
		return nil
	}
	t, err := task.Get(ctx, tx, r.TaskID)
	if err != nil {
		return err
	}
	if t.CurrentRunID != r.ID || !slices.Contains(from, t.Status) {
		return nil
	}
	_, err = task.Transition(ctx, tx, r.TaskID, to, reason, 0, a.task(r.ID))
	return err
}

// releaseTask puts the task back in the queue after the run ended without finishing it.
func (r *row) releaseTask(ctx context.Context, tx *sql.Tx, a Actor, reason string) error {
	return r.moveTask(ctx, tx, a, []string{task.Claimed, task.Running, task.Waiting}, task.Ready, reason)
}

func (r *row) endSessions(ctx context.Context, tx *sql.Tx, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET status = 'ended', ended_at = ?, end_reason = ?
		WHERE run_id = ? AND status = 'active'`, db.Now(), reason, r.ID)
	return err
}

func cas(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return apperr.Retryable(apperr.Conflict, "modified concurrently")
	}
	return nil
}

// Hook is one hook invocation resolved to a run, ready to apply.
type Hook struct {
	Harness string
	Kind    string
	RunID   string
	EventID string // dedupe base; "" = no dedupe
	Events  []adapter.Event
}

// Ingest applies normalized events to the run inside tx. Each event is recorded as
// hook.<type> with dedupe key EventID[#i]; a duplicate is skipped entirely. It returns
// the id of the first new events row (0 if none).
func Ingest(ctx context.Context, tx *sql.Tx, h Hook) (int64, error) {
	r, err := load(ctx, tx, h.RunID)
	if err != nil {
		return 0, err
	}
	if r.Harness != h.Harness {
		return 0, fmt.Errorf("run %s is a %s run, not %s", r.ID, r.Harness, h.Harness)
	}
	a := Actor{Name: "run:" + r.ID, Source: "hook:" + h.Harness}
	var first int64
	for i, ev := range h.Events {
		key := h.EventID
		if key != "" && len(h.Events) > 1 {
			key += "#" + strconv.Itoa(i)
		}
		id, ok, err := event.Insert(ctx, tx, event.Event{
			Source: a.Source, DedupeKey: key, Kind: "hook." + ev.Type, EntityType: "run", EntityID: r.ID,
			ProjectID: r.ProjectID, TaskID: r.TaskID, RunID: r.ID, Actor: a.Name, Data: eventData(h.Kind, ev),
		})
		if err != nil {
			return first, err
		}
		if !ok {
			continue // duplicate delivery
		}
		if first == 0 {
			first = id
		}
		if err := apply(ctx, tx, r, a, ev, id); err != nil {
			return first, err
		}
	}
	return first, nil
}

func eventData(kind string, ev adapter.Event) map[string]any {
	d := map[string]any{"hook": kind}
	for k, v := range ev.Data {
		d[k] = v
	}
	set := func(k, v string) {
		if v != "" {
			d[k] = v
		}
	}
	set("session", ev.HarnessSessionID)
	set("reason", ev.Reason)
	set("tool", ev.Tool)
	set("tool_phase", ev.ToolPhase)
	if n := 500; len(ev.Text) > n {
		for n > 0 && !utf8.RuneStart(ev.Text[n]) {
			n--
		}
		set("text", ev.Text[:n]+"…")
	} else {
		set("text", ev.Text)
	}
	if !ev.ResetsAt.IsZero() {
		d["resets_at"] = ev.ResetsAt.UTC().Format(db.TimeFormat)
	}
	return d
}

// apply is the run/task state machine for one new event.
func apply(ctx context.Context, tx *sql.Tx, r *row, a Actor, ev adapter.Event, evID int64) error {
	now := db.Now()
	progress := ev.Type != adapter.SessionStart && ev.Type != adapter.SessionEnd
	if _, err := tx.ExecContext(ctx, `
UPDATE runs SET heartbeat_at = ?, first_activity_at = coalesce(first_activity_at, ?),
                last_progress_at = CASE WHEN ? THEN ? ELSE last_progress_at END
WHERE id = ?`, now, now, progress, now, r.ID); err != nil {
		return err
	}

	if ev.Type == adapter.Usage {
		// Late usage (e.g. a transcript read after done) still counts.
		if err := insertUsage(ctx, tx, r, ev.Usage); err != nil {
			return err
		}
	}
	if ev.Type == adapter.RateLimit && ev.ResetsAt.After(time.Now()) {
		// A known reset is worth keeping even when the run is over: a quota snapshot
		// and a cooldown of the account (Claude, Codex) or the profile until then.
		hit, err := quota.RecordHit(ctx, tx, quota.Hit{RunID: r.ID, ResetsAt: ev.ResetsAt, Text: ev.Text})
		if err != nil {
			return err
		}
		if err := SetCooldown(ctx, tx, hit.AccountID, hit.ProfileID, ev.ResetsAt, "rate_limit: "+oneLine(ev.Text)); err != nil {
			return err
		}
	}
	if !live(r.Status) {
		// queued, parked, stopping, ended: record only. A late session_start must not
		// reopen a session, and a late error must not overwrite the final one.
		return nil
	}
	switch ev.Type {
	case adapter.SessionStart:
		if err := bindSession(ctx, tx, r, ev); err != nil {
			return err
		}
	case adapter.SessionEnd:
		return sessionEnded(ctx, tx, r, a, ev)
	case adapter.Error, adapter.RateLimit:
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET error = ? WHERE id = ?`, oneLine(ev.Type+": "+ev.Text), r.ID); err != nil {
			return err
		}
	case adapter.TurnEnd:
		// A turn that fails after a (non-retried) rate limit means the harness gave up:
		// the account is out of quota. Transient 429s the harness retries itself only
		// record the error.
		if ev.Reason == "error" {
			text, limited, err := turnRateLimited(ctx, tx, r.ID, evID)
			if err != nil {
				return err
			}
			if limited {
				return quotaEnded(ctx, tx, r, a, text)
			}
		}
	}

	to := r.Status
	switch ev.Type {
	case adapter.SessionStart:
		if r.Status == Starting {
			to = Idle
		}
	case adapter.TurnStart, adapter.Activity:
		to = Running
	case adapter.TurnEnd:
		to = Idle
	case adapter.Question, adapter.Permission:
		to = Waiting
	}
	if to == r.Status {
		return nil
	}
	from := r.Status
	if err := r.setStatus(ctx, tx, a, to, map[string]any{"event": ev.Type}); err != nil {
		return err
	}
	switch {
	case to == Waiting:
		return r.moveTask(ctx, tx, a, []string{task.Running}, task.Waiting, oneLine(ev.Text))
	case from == Waiting:
		// A closed harness dialog does not answer the run's own `otag ask`: while that
		// is open, or its answer has not reached the run yet, the task keeps waiting
		// for the answer. Only this run's ask counts.
		if r.TaskID != "" {
			if open, err := msg.AwaitsAnswer(ctx, tx, r.TaskID, r.ID); err != nil || open {
				return err
			}
		}
		return r.moveTask(ctx, tx, a, []string{task.Waiting}, task.Running, "")
	}
	return nil
}

// bindSession records the harness session id. A different id on the same run (Claude
// /clear, resume) ends the old sessions row and opens a new one on the same pane.
func bindSession(ctx context.Context, tx *sql.Tx, r *row, ev adapter.Event) error {
	if ev.HarnessSessionID == "" {
		return nil
	}
	var id int64
	var cur sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, harness_session_id FROM sessions
		WHERE run_id = ? AND status = 'active' ORDER BY id DESC LIMIT 1`, r.ID).Scan(&id, &cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions (run_id, harness, harness_session_id, transcript_path)
			VALUES (?, ?, ?, ?)`, r.ID, r.Harness, ev.HarnessSessionID, nullStr(ev.TranscriptPath))
		return err
	case err != nil:
		return err
	case !cur.Valid || cur.String == ev.HarnessSessionID:
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET harness_session_id = ?,
			transcript_path = coalesce(?, transcript_path) WHERE id = ?`, ev.HarnessSessionID, nullStr(ev.TranscriptPath), id)
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status = 'ended', ended_at = ?, end_reason = 'replaced'
		WHERE id = ?`, db.Now(), id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO sessions (run_id, harness, harness_session_id, transcript_path, tmux_session, tmux_window, tmux_pane,
                      aux_panes, herdr_pane, server_url, server_pid, pid, pid_started_at)
SELECT run_id, harness, ?, ?, tmux_session, tmux_window, tmux_pane, aux_panes, herdr_pane, server_url, server_pid,
       pid, pid_started_at
FROM sessions WHERE id = ?`, ev.HarnessSessionID, nullStr(ev.TranscriptPath), id)
	return err
}

// sessionEnded closes the session; when no session is left and the run is live, the
// harness exited without done/fail: the run ends as exited and the task is released.
func sessionEnded(ctx context.Context, tx *sql.Tx, r *row, a Actor, ev adapter.Event) error {
	reason := ev.Reason
	if reason == "" {
		reason = "exit"
	}
	q := `UPDATE sessions SET status = 'ended', ended_at = ?, end_reason = ? WHERE run_id = ? AND status = 'active'`
	args := []any{db.Now(), reason, r.ID}
	if ev.HarnessSessionID != "" {
		q += " AND (harness_session_id IS NULL OR harness_session_id = ?)"
		args = append(args, ev.HarnessSessionID)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE run_id = ? AND status = 'active'`, r.ID).
		Scan(&open); err != nil {
		return err
	}
	if open > 0 || !live(r.Status) {
		return nil
	}
	if err := r.end(ctx, tx, a, ReasonExited, "harness session ended: "+reason); err != nil {
		return err
	}
	return r.releaseTask(ctx, tx, a, "run "+r.ID+" exited")
}

func insertUsage(ctx context.Context, tx *sql.Tx, r *row, u *adapter.UsageData) error {
	if u == nil {
		return nil
	}
	var key sql.NullString
	if u.DedupeKey != "" {
		key = sql.NullString{String: r.Harness + ":" + u.DedupeKey, Valid: true}
	}
	source := u.Source
	if source == "" {
		source = "hook"
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO run_usage (run_id, session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
                       reasoning_tokens, cost_usd, source, estimated, dedupe_key)
VALUES (?, (SELECT max(id) FROM sessions WHERE run_id = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (dedupe_key) DO UPDATE SET
  -- A repeated report of the same message keeps the largest counts: a transcript read
  -- while the message was still being written may have seen partial ones.
  input_tokens = max(input_tokens, excluded.input_tokens),
  output_tokens = max(output_tokens, excluded.output_tokens),
  cache_read_tokens = max(cache_read_tokens, excluded.cache_read_tokens),
  cache_write_tokens = max(cache_write_tokens, excluded.cache_write_tokens),
  reasoning_tokens = max(reasoning_tokens, excluded.reasoning_tokens),
  cost_usd = coalesce(max(cost_usd, excluded.cost_usd), cost_usd, excluded.cost_usd),
  updated_at = excluded.updated_at`,
		r.ID, r.ID, u.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens,
		u.ReasoningTokens, u.CostUSD, source, u.Estimated, key)
	return err
}
