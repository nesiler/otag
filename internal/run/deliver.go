package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
)

// Dispatcher delivers queued messages into live sessions over injected channels
// (docs/MESSAGES.md): a Claude session's mod ($.prompt.submit), Codex's control socket
// (turn/start, turn/steer) and OpenCode's prompt API. It implements msg.Dispatcher for
// the CLI's synchronous attempt; the daemon calls Dispatch.
//
// One attempt is: Claim (a transaction), the adapter call outside any transaction,
// Settle (another transaction). msg.Claim keeps one injected attempt in flight per
// run. Nothing is typed into a pane: a run whose harness has no injected channel in
// its current state keeps its messages queued for its hooks or `otag inbox`.
type Dispatcher struct {
	DB       *db.DB
	Adapters *adapter.Registry
	Getenv   func(string) string
	Actor    msg.Actor
	// Timeout bounds one adapter call (0 = DeliverTimeout). The call is never cut
	// short by a caller's budget: an interrupted send has an unknown outcome.
	Timeout time.Duration
}

// DeliverTimeout bounds one adapter delivery call.
const DeliverTimeout = 20 * time.Second

// injection is how a run takes an injected message now.
type injection struct {
	Channel string
	Steer   bool
}

// injectPlan picks the injected channel for run r, or says why there is none now.
// Claude takes injected text only when idle (its mod submits a user turn); while it
// works, its PostToolUse/Stop hooks carry messages. Codex (app-server mode only) and
// OpenCode start a turn when idle and steer the running one.
func injectPlan(r *Run, ad adapter.Adapter) (injection, string) {
	switch r.Status {
	case Idle, Running:
	case Waiting:
		return injection{}, "run is in a harness dialog"
	default:
		return injection{}, "run is " + r.Status
	}
	_, rd := ad.(adapter.ReceiptDeliverer)
	_, dd := ad.(adapter.Deliverer)
	if !rd && !dd {
		return injection{}, r.Harness + " has no injected delivery; hooks or otag inbox take it"
	}
	switch r.Harness {
	case "claude":
		if r.Status != Idle {
			return injection{}, "run is busy; its next hook carries the message"
		}
		return injection{Channel: adapter.ChanModSubmit}, ""
	case "codex":
		if r.Session == nil || !strings.HasSuffix(r.Session.ServerURL, "/as.sock") {
			return injection{}, "codex hooks mode has no control socket; hooks or otag inbox take it"
		}
	}
	if r.Status == Running {
		return injection{Channel: adapter.ChanAPISteer, Steer: true}, ""
	}
	return injection{Channel: adapter.ChanAPIStart}, ""
}

// RunAttempt is the outcome of one Dispatch.
type RunAttempt struct {
	RunID      string  `json:"run"`
	Channel    string  `json:"channel,omitempty"`
	Nonce      string  `json:"nonce,omitempty"`
	Deliveries []int64 `json:"deliveries,omitempty"` // the deliveries the attempt carried
	// Result: skipped (nothing claimed), dispatched (sent, proof to come), delivered
	// (proven already), queued (definitely not sent; retried later or dead) or failed
	// (outcome unknown, or otag could not record the attempt).
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
}

// Dispatch makes one injected delivery attempt into run runID: it claims the run's
// due queued deliveries (when the run can take them now), sends them in one text and
// settles the attempt.
func (d *Dispatcher) Dispatch(ctx context.Context, runID string) RunAttempt {
	out := RunAttempt{RunID: runID, Result: "skipped"}
	r, err := Get(ctx, d.DB, runID)
	if err != nil {
		out.Result, out.Reason = "failed", err.Error()
		return out
	}
	ad, ok := d.Adapters.Get(r.Harness)
	if !ok {
		out.Reason = "unknown harness " + r.Harness
		return out
	}
	inj, why := injectPlan(r, ad)
	if inj.Channel == "" {
		out.Reason = why
		return out
	}
	// The plan holds for the status it was made for (a Claude mod submit only while
	// idle): the claim re-checks it in its own transaction.
	var at *msg.Attempt
	err = d.DB.Tx(ctx, func(tx *sql.Tx) error {
		at, err = msg.Claim(ctx, tx, msg.ClaimInput{RunID: runID, Channel: inj.Channel, Status: r.Status}, d.Actor)
		return err
	})
	if err != nil {
		out.Result, out.Reason = "failed", "claim: "+err.Error()
		return out
	}
	if at == nil {
		out.Reason = "nothing due, or another delivery to this run is in flight"
		return out
	}
	out.Channel, out.Nonce = inj.Channel, at.Nonce
	for _, it := range at.Items {
		out.Deliveries = append(out.Deliveries, it.Delivery.ID)
	}
	t := adapter.Target{RunID: r.ID, RunDir: paths.RunDir(d.getenv(), r.ID)}
	if r.Session != nil {
		t.HarnessSessionID, t.ServerURL = r.Session.HarnessSessionID, r.Session.ServerURL
	}
	m := adapter.Message{ID: at.Nonce, Nonce: at.Nonce, Text: at.Text(msg.DefaultMaxChars), Steer: inj.Steer}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DeliverTimeout
	}
	// The send outlives the caller's cancellation: a cut call has an unknown outcome.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	rc, derr := send(cctx, ad, t, m)
	cancel()

	in := msg.SettleInput{Nonce: at.Nonce, Outcome: msg.Sent, Receipt: rc}
	switch {
	case derr == nil:
		out.Result = "dispatched"
	case errors.Is(derr, adapter.ErrNotReady):
		// Not now (the mod is not up, a turn cannot be steered): retried soon, no
		// attempt counted.
		in.Outcome, in.Error = msg.NotReady, derr.Error()
		out.Result, out.Reason = "queued", derr.Error()
	case errors.Is(derr, adapter.ErrNotSent):
		in.Outcome, in.Error = msg.NotSent, derr.Error()
		out.Result, out.Reason = "queued", derr.Error()
	default:
		in.Outcome, in.Error = msg.Unknown, derr.Error()
		out.Result, out.Reason = "failed", "outcome unknown: "+derr.Error()
	}
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer scancel()
	err = d.DB.Tx(sctx, func(tx *sql.Tx) error {
		_, err := msg.Settle(sctx, tx, in, d.Actor)
		return err
	})
	if err != nil {
		// The attempt stays dispatched; the proof timeout makes it outcome_unknown.
		out.Result, out.Reason = "failed", fmt.Sprintf("settle (%s): %v", in.Outcome, err)
		return out
	}
	if out.Result == "dispatched" {
		// The proof may have arrived before the receipt was stored.
		if dl, err := msg.GetDelivery(sctx, d.DB, out.Deliveries[0]); err == nil && (dl.State == msg.Delivered || dl.State == msg.Read) {
			out.Result = "delivered"
		}
	}
	return out
}

// Eligible reports whether run runID could take an injected delivery now (see
// injectPlan): the daemon leaves the others out of msg.DueRuns, so runs that only
// their hooks or `otag inbox` serve never crowd out those it can serve.
func (d *Dispatcher) Eligible(ctx context.Context, runID string) bool {
	r, err := Get(ctx, d.DB, runID)
	if err != nil {
		return false
	}
	ad, ok := d.Adapters.Get(r.Harness)
	if !ok {
		return false
	}
	inj, _ := injectPlan(r, ad)
	return inj.Channel != ""
}

// InboxDir is the run dir's delivery inbox (the Claude mod's, see the claude adapter):
// files an injected attempt left for an in-session reader.
const InboxDir = "inbox"

// dropInbox removes run runID's delivery inbox once the run has ended and nothing of it
// is left: a file there could only be taken by a session that should not exist, and
// without the mod's ready file nothing is written there again.
func dropInbox(getenv func(string) string, runID string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if dir := paths.RunDir(getenv, runID); dir != "" {
		_ = os.RemoveAll(filepath.Join(dir, InboxDir))
	}
}

// SweepInboxes drops the inbox of runs that ended (cleanup confirmed) between an hour
// ago and settle ago: a Claude mod still alive for a moment after its own `otag done`
// rewrites its ready file after the cleanup removed the inbox.
func SweepInboxes(ctx context.Context, q db.Querier, getenv func(string) string, now time.Time, settle time.Duration) error {
	rows, err := q.QueryContext(ctx, `SELECT id FROM runs WHERE status = 'ended' AND cleanup_pending = 0
		AND ended_at BETWEEN ? AND ?`, now.Add(-time.Hour).UTC().Format(db.TimeFormat),
		now.Add(-settle).UTC().Format(db.TimeFormat))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		dropInbox(getenv, id)
	}
	return nil
}

// send calls the adapter: DeliverReceipt when it has one, else Deliver.
func send(ctx context.Context, ad adapter.Adapter, t adapter.Target, m adapter.Message) (rc adapter.Receipt, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("adapter panic: %v", p)
		}
	}()
	if rd, ok := ad.(adapter.ReceiptDeliverer); ok {
		return rd.DeliverReceipt(ctx, t, m)
	}
	return adapter.Receipt{}, ad.(adapter.Deliverer).Deliver(ctx, t, m)
}

func (d *Dispatcher) getenv() func(string) string {
	if d.Getenv != nil {
		return d.Getenv
	}
	return os.Getenv
}

// Attempt implements msg.Dispatcher: one attempt for the run delivery deliveryID is
// bound to (or, when that run has gone, its task's current run). The attempt carries
// every due delivery of that run, deliveryID among them when it is due.
func (d *Dispatcher) Attempt(ctx context.Context, deliveryID int64) msg.AttemptResult {
	out := msg.AttemptResult{Delivery: deliveryID, Result: "skipped"}
	dl, err := msg.GetDelivery(ctx, d.DB, deliveryID)
	if err != nil {
		out.Result, out.Reason = "failed", err.Error()
		return out
	}
	if dl.State != msg.Queued {
		out.Result, out.Reason = dl.State, "taken by an earlier attempt"
		if dl.State == msg.Dispatched {
			out.Reason = "dispatched in an earlier attempt"
		}
		return out
	}
	runID := ""
	if dl.To.Run != nil {
		runID = *dl.To.Run
	}
	if r, err := Get(ctx, d.DB, runID); (err != nil || !live(r.Status)) && dl.To.Task != nil {
		var cur sql.NullString
		if d.DB.QueryRowContext(ctx, `SELECT current_run_id FROM tasks WHERE id = ?`, *dl.To.Task).Scan(&cur) == nil && cur.Valid {
			runID = cur.String
		}
	}
	if runID == "" {
		out.Reason = "no live run: read with otag inbox, or taken by the next run"
		return out
	}
	ra := d.Dispatch(ctx, runID)
	if !slices.Contains(ra.Deliveries, deliveryID) {
		out.Reason = ra.Reason
		if ra.Result != "skipped" {
			out.Reason = "not due yet"
		}
		if out.Reason == "" {
			out.Reason = "queued for delivery"
		}
		return out
	}
	out.Result, out.Reason = ra.Result, ra.Reason
	if ra.Result == "dispatched" {
		out.Reason = "sent by " + ra.Channel + "; proof of delivery to come"
	}
	if ra.Result == "delivered" {
		if dl, err := msg.GetDelivery(ctx, d.DB, deliveryID); err == nil && dl.State != msg.Delivered && dl.State != msg.Read {
			out.Result = dl.State
		}
	}
	return out
}

// AttemptRun implements msg.RunDispatcher: one Dispatch into run runID, reported per
// delivery: those the attempt carried, else the run's queued ones with the reason
// nothing was sent.
func (d *Dispatcher) AttemptRun(ctx context.Context, runID string) []msg.AttemptResult {
	ra := d.Dispatch(ctx, runID)
	var out []msg.AttemptResult
	if len(ra.Deliveries) > 0 {
		reason := ra.Reason
		if ra.Result == "dispatched" {
			reason = "sent by " + ra.Channel + "; proof of delivery to come"
		}
		for _, id := range ra.Deliveries {
			r := msg.AttemptResult{Delivery: id, Result: ra.Result, Reason: reason}
			if ra.Result == "delivered" {
				if dl, err := msg.GetDelivery(ctx, d.DB, id); err == nil && dl.State != msg.Delivered && dl.State != msg.Read {
					r.Result = "dispatched"
				}
			}
			out = append(out, r)
		}
		return out
	}
	result, reason := "queued", ra.Reason
	if ra.Result == "failed" {
		result = "failed"
	}
	rows, err := d.DB.QueryContext(ctx, `SELECT id FROM message_deliveries WHERE run_id = ? AND state = 'queued' ORDER BY id`, runID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, msg.AttemptResult{Delivery: id, Result: result, Reason: reason})
		}
	}
	return out
}

var (
	_ msg.Dispatcher    = (*Dispatcher)(nil)
	_ msg.RunDispatcher = (*Dispatcher)(nil)
)
