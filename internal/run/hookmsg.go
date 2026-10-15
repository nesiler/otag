package run

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/msg"
)

// Messaging in the hook bridge (docs/MESSAGES.md): see HandleHook.

// StopBlockLimit caps consecutive Stop blocks in one turn (since its prompt).
const StopBlockLimit = 3

// hookMsgBudget is the time a hook must have left to claim messages for its output.
const hookMsgBudget = time.Second

func budgetLeft(ctx context.Context) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) >= hookMsgBudget
}

// savepoint runs fn inside a savepoint of tx and undoes only fn's writes when it fails.
func savepoint(ctx context.Context, tx *sql.Tx, fn func() error) (err error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT otag_msg"); err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
		if err != nil {
			_, rerr := tx.ExecContext(ctx, "ROLLBACK TO otag_msg")
			err = errors.Join(err, rerr)
		}
		if _, rerr := tx.ExecContext(ctx, "RELEASE otag_msg"); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	return fn()
}

// launchByTurn reports whether the harness's turn start proves its launch prompt
// (adapter.LaunchTurnProver).
func launchByTurn(ad adapter.Adapter) bool {
	p, ok := ad.(adapter.LaunchTurnProver)
	return ok && p.LaunchProvedByTurnStart()
}

// proveHook proves the deliveries this hook shows arrived: delivery markers in a
// submitted prompt (Claude, Codex UserPromptSubmit; also when the event itself is a
// duplicate), harness refs (OpenCode inbox.delivered), a turn start for the launch
// prompt of a harness without markers or receipts there (byTurn, OpenCode), and a Stop
// that continues a Stop block of the same session. A turn that ends otherwise leaves
// its Stop block unproven: outcome_unknown, never re-sent.
func proveHook(ctx context.Context, tx *sql.Tx, req HookRequest, res adapter.HookResult, byTurn bool, a msg.Actor) error {
	in := msg.ProveInput{RunID: req.RunID, Refs: res.Refs}
	if slices.ContainsFunc(res.Events, func(e adapter.Event) bool { return e.Type == adapter.TurnStart }) {
		in.Markers = msg.ScanMarkers(promptOf(req.Payload))
		if byTurn {
			in.Channel = adapter.ChanLaunchPrompt
		}
	}
	if len(in.Markers) > 0 || len(in.Refs) > 0 || in.Channel != "" {
		if _, err := msg.Prove(ctx, tx, in, a); err != nil {
			return err
		}
	}
	if s := res.Slot; s != nil && s.Channel == adapter.ChanStopBlock && s.Continued {
		return proveStop(ctx, tx, req.RunID, res.SessionRef, a)
	}
	if slices.ContainsFunc(res.Events, func(e adapter.Event) bool {
		return e.Type == adapter.TurnEnd || e.Type == adapter.SessionEnd
	}) {
		return unknownStops(ctx, tx, req.RunID, a)
	}
	return nil
}

// promptOf returns the submitted prompt of a hook payload ({"prompt": ...}), the
// only text scanned for delivery markers.
func promptOf(payload []byte) []byte {
	if !bytes.Contains(payload, []byte("[otag#d")) {
		return nil
	}
	var p struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	return []byte(p.Prompt)
}

// stopRef is the receipt of a Stop block: the session it was printed to.
func stopRef(session string) string { return "stop:" + session }

// stopAttempts returns the nonces and receipts of the run's dispatched Stop blocks.
func stopAttempts(ctx context.Context, tx *sql.Tx, runID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT attempt_nonce, coalesce(receipt_ref, '') FROM message_deliveries
		WHERE run_id = ? AND state = 'dispatched' AND channel = ?`, runID, adapter.ChanStopBlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var nonce, ref string
		if err := rows.Scan(&nonce, &ref); err != nil {
			return nil, err
		}
		out[nonce] = ref
	}
	return out, rows.Err()
}

// proveStop delivers the Stop blocks a continued Stop (stop_hook_active) of session
// proves: the attempts printed to that session (another session's stay unproven). The
// session's latest Stop block of the current turn (since its prompt), the one this Stop
// continues, is proven even after its proof timeout made it unknown (a continued turn
// that ran longer than the timeout); an older turn's block stays unknown.
func proveStop(ctx context.Context, tx *sql.Tx, runID, session string, a msg.Actor) error {
	atts, err := stopAttempts(ctx, tx, runID)
	if err != nil {
		return err
	}
	var latest string
	err = tx.QueryRowContext(ctx, `SELECT attempt_nonce FROM message_deliveries
		WHERE run_id = ? AND channel = ? AND receipt_ref = ? AND attempt_nonce IS NOT NULL
			AND dispatched_at >= coalesce((SELECT max(created_at) FROM events
				WHERE run_id = ? AND kind = 'hook.turn_start'), '')
		ORDER BY dispatched_at DESC, id DESC LIMIT 1`, runID, adapter.ChanStopBlock, stopRef(session), runID).Scan(&latest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, ok := atts[latest]; latest != "" && !ok {
		atts[latest] = stopRef(session)
	}
	for nonce, ref := range atts {
		if ref != stopRef(session) {
			continue
		}
		in := msg.ProveInput{RunID: runID, Channel: adapter.ChanStopBlock, Nonce: nonce}
		if _, err := msg.Prove(ctx, tx, in, a); err != nil {
			return err
		}
	}
	return nil
}

// unknownStops settles the run's Stop blocks still waiting for their continued Stop
// as unknown: the turn ended (Esc, an error, a new prompt) without it.
func unknownStops(ctx context.Context, tx *sql.Tx, runID string, a msg.Actor) error {
	_, err := msg.MarkUnknown(ctx, tx, runID, adapter.ChanStopBlock, "",
		"the turn ended without the Stop that proves the block", a)
	return err
}

// stopBlock claims the run's queued messages for a Stop block, up to StopBlockLimit
// blocks since the turn's prompt. The attempt counts as sent once claimed (its
// receipt is the session); the session's next Stop with stop_hook_active proves it.
// On any error it returns no attempt: the caller's savepoint undoes the claim, so
// nothing may be printed.
func stopBlock(ctx context.Context, tx *sql.Tx, runID, session string, a msg.Actor) (*msg.Attempt, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT attempt_nonce) FROM message_deliveries
		WHERE run_id = ? AND channel = ? AND dispatched_at >= coalesce((SELECT max(created_at) FROM events
			WHERE run_id = ? AND kind = 'hook.turn_start'), '')`, runID, adapter.ChanStopBlock, runID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= StopBlockLimit {
		return nil, nil
	}
	at, err := msg.Claim(ctx, tx, msg.ClaimInput{RunID: runID, Channel: adapter.ChanStopBlock}, a)
	if err != nil || at == nil {
		return nil, err
	}
	if _, err := msg.Settle(ctx, tx, msg.SettleInput{Nonce: at.Nonce, Outcome: msg.Sent,
		Receipt: adapter.Receipt{Ref: stopRef(session)}}, a); err != nil {
		return nil, err
	}
	return at, nil
}

// renderDelivery is the adapter's hook output carrying text; false when it fails.
func renderDelivery(hd adapter.HookDeliverer, kind string, base []byte, text string) (out []byte, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = nil, false
		}
	}()
	out = hd.RenderDelivery(kind, base, text)
	return out, len(out) > 0
}
