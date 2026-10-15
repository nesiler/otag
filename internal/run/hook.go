package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/guard"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/tmux"
)

// HookRequest is one `otag event <harness> <kind>` invocation.
type HookRequest struct {
	Harness string
	Kind    string
	RunID   string // from --run / OTAG_RUN_ID; "" = resolve through the payload's session id
	EventID string // from --event-id; overrides the adapter's EventID
	Payload []byte
	Getenv  func(string) string
	// Tmux, when set, lets a hook that ends a live run (session_end: the harness exited
	// without done/fail) close the run's other windows, as done/fail/stop do: a server
	// or sidecar must not keep working in a worktree whose task is back in the queue.
	Tmux *tmux.Client
	// RawPayloads stores the payload itself (redacted) in raw_hook_payloads, for
	// debugging ([daemon] raw_payloads); otherwise only a compact allowlisted extract.
	RawPayloads bool
}

// HookResponse is what the bridge did.
type HookResponse struct {
	Output  []byte // printed on stdout
	RunID   string // resolved run ("" = unknown)
	EventID int64  // first new events row (0: unknown run, duplicate or nothing to record)
	// Delivered, when set, settles the messages Output carries; the caller runs it
	// right after printing Output (hook context counts as delivered once printed).
	Delivered func(ctx context.Context) error
	// MsgErr is a messaging failure (proof, claim) for the log; the hook went on
	// without messaging.
	MsgErr error
}

// HandleHook stores the payload and applies the adapter's events in one transaction.
//
// Guarded kinds (adapter.Guarder: pre-tool and permission hooks) fail closed: the
// decision is made before, and independently of, the write, so a failed transaction
// never drops a deny; and when the run's policy cannot be applied (no database, a
// schema newer than this binary, an unknown or ended run, an unreadable payload, a
// token of another run) the hook answers the adapter's DenyOutput.
//
// Every other kind keeps the harness-safe no-op on failure: the adapter's output
// (additionalContext, ...) is returned only when the run is known and everything was
// recorded. err reports the failure for logging; the hook itself must still succeed.
func HandleHook(ctx context.Context, d *db.DB, reg *adapter.Registry, req HookRequest) (resp HookResponse, err error) {
	ad, ok := reg.Get(req.Harness)
	if !ok {
		err = fmt.Errorf("unknown harness %q", req.Harness)
		if d != nil {
			err = errors.Join(err, storeRaw(ctx, d, req, req.RunID, ""))
		}
		return resp, err
	}
	noop := func() []byte {
		defer func() { _ = recover() }()
		return ad.NoopOutput(req.Kind)
	}
	resp.Output = noop()
	guarder, _ := ad.(adapter.Guarder)
	guarded := guarder != nil && func() (ok bool) {
		defer func() { _ = recover() }()
		return guarder.GuardKind(req.Kind)
	}()
	// refuse is the fail-closed answer of a guarded hook: the harness must not run
	// the tool when otag cannot apply the run's policy.
	refuse := func(reason string) {
		if guarded {
			resp.Output = func() (out []byte) {
				defer func() { _ = recover() }()
				return guarder.DenyOutput(req.Kind, "otag guard: "+reason+"; refusing the tool (fail closed)")
			}()
		}
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			resp = HookResponse{Output: noop()}
			refuse("otag failed internally")
		}
	}()
	if d == nil {
		refuse("otag's database is unavailable")
		return resp, errors.New("no database")
	}
	if d.ReadOnly() {
		refuse("otag's database is newer than this otag binary")
		return resp, apperr.New(apperr.SchemaTooNew, "database schema v%d is newer than this binary (v%d); upgrade otag",
			d.Version, d.Latest)
	}

	hc := adapter.HookContext{Getenv: req.Getenv}
	if req.Getenv != nil {
		hc.DB, hc.TmuxSocket, hc.OtagBin = paths.DB(req.Getenv), paths.TmuxSocket(req.Getenv), paths.OtagBin(req.Getenv)
	}
	known := false
	if req.RunID != "" {
		if known, err = loadHookContext(ctx, d, req.RunID, &hc); err != nil {
			refuse("cannot load the run's policy")
			return resp, errors.Join(err, storeRaw(ctx, d, req, req.RunID, ""))
		}
	}
	res, perr := ad.ParseEvent(hc, req.Kind, req.Payload)
	if perr == nil && !known && req.RunID == "" && res.SessionRef != "" {
		var id string
		if d.QueryRowContext(ctx, `SELECT run_id FROM sessions WHERE harness = ? AND harness_session_id = ?
			ORDER BY id DESC LIMIT 1`, req.Harness, res.SessionRef).Scan(&id) == nil {
			req.RunID = id
			if known, err = loadHookContext(ctx, d, id, &hc); err == nil && known {
				res, perr = ad.ParseEvent(hc, req.Kind, req.Payload)
			}
		}
	}
	if perr != nil {
		refuse("the hook input cannot be read")
		return resp, errors.Join(fmt.Errorf("%s adapter: %w", req.Harness, perr), storeRaw(ctx, d, req, req.RunID, ""))
	}
	// The run token: a hook naming a run must come from that run.
	if known {
		token := ""
		if req.Getenv != nil {
			token = req.Getenv(TokenEnv)
		}
		if terr := CheckToken(ctx, d, req.RunID, token); terr != nil {
			refuse("this hook does not come from run " + req.RunID)
			return resp, errors.Join(terr, storeRaw(ctx, d, req, "", res.SessionRef))
		}
	}
	// The guard's decision does not depend on recording the event.
	if guarded {
		switch {
		case !known && req.RunID == "":
			refuse("no otag run is known for this session")
		case !known:
			refuse("run " + req.RunID + " is unknown")
		case !live(hc.Status):
			refuse("run " + req.RunID + " is " + hc.Status + ", no longer active")
		case !hasToken(ctx, d, req.RunID):
			// Created before run tokens: nothing proves the hook comes from it.
			refuse("run " + req.RunID + " has no run token (started by an older otag); restart it")
		case res.Output != nil:
			resp.Output = res.Output
		}
	}
	eventID := req.EventID
	if eventID == "" {
		eventID = res.EventID
	}
	// A session_end may end the run: stop its other windows (server, sidecars, the
	// pane if the caller is elsewhere) before the task is released; the caller's own
	// pane goes after the commit, or the caller would die before recording.
	var closer *Manager
	// A sandboxed agent can commit done/fail to the DB yet lack tmux IPC.
	// Its next host-side hook retries cleanup after committing this event.
	if known && hc.Status == Ended && req.Tmux != nil {
		closer = &Manager{DB: d, Tmux: req.Tmux, Getenv: req.Getenv}
	}
	if i := slices.IndexFunc(res.Events, func(e adapter.Event) bool { return e.Type == adapter.SessionEnd }); i >= 0 &&
		known && req.Tmux != nil && live(hc.Status) && endsLastSession(ctx, d, req.RunID, res.Events[i].HarnessSessionID) {
		closer = &Manager{DB: d, Tmux: req.Tmux, Getenv: req.Getenv}
		closer.killPanes(ctx, req.RunID, func(pane string) bool { return pane != closer.selfPane() })
	}
	// Messaging (docs/MESSAGES.md): proofs carried by this hook, and messages its
	// output can carry (HookResult.Slot). A failure here is logged, never fatal: it is
	// undone on its own (a savepoint) and the hook goes on as without messaging.
	hd, _ := ad.(adapter.HookDeliverer)
	slot := res.Slot
	if guarded || hd == nil {
		slot = nil
	}
	mactor := msg.Actor{Name: "run:" + req.RunID, Source: "hook:" + req.Harness}
	var first int64
	var at *msg.Attempt
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		at = nil // a retried transaction starts over
		rawID, err := insertRaw(ctx, tx, req, req.RunID, res.SessionRef)
		if err != nil || !known {
			return err
		}
		resp.MsgErr = savepoint(ctx, tx, func() error { return proveHook(ctx, tx, req, res, launchByTurn(ad), mactor) })
		events := res.Events
		// A savepoint that failed (an error or a recovered panic) undid its claim: then
		// there is no attempt, nothing is printed and the turn end stands.
		if slot != nil && slot.Channel == adapter.ChanStopBlock && live(hc.Status) && budgetLeft(ctx) {
			if serr := savepoint(ctx, tx, func() (err error) {
				at, err = stopBlock(ctx, tx, req.RunID, res.SessionRef, mactor)
				return err
			}); serr != nil {
				at, resp.MsgErr = nil, errors.Join(resp.MsgErr, serr)
			}
			if at != nil {
				// The turn goes on with the messages: the run stays busy.
				events = slices.DeleteFunc(slices.Clone(events), func(e adapter.Event) bool { return e.Type == adapter.TurnEnd })
			}
		}
		if res.Prime == adapter.PrimeMessage && live(hc.Status) && eventID != "" && budgetLeft(ctx) {
			// No hook output to carry it: queue it for the run's normal delivery. The
			// event id keys the message, so a repeated frame queues nothing new.
			resp.MsgErr = errors.Join(resp.MsgErr, savepoint(ctx, tx, func() error {
				return primeMessage(ctx, tx, req.RunID, eventID, mactor)
			}))
		}
		if len(events) == 0 {
			return nil
		}
		if first, err = Ingest(ctx, tx, Hook{Harness: req.Harness, Kind: req.Kind, RunID: req.RunID,
			EventID: eventID, Events: events}); err != nil || first == 0 {
			return err
		}
		if slot != nil && slot.Channel != adapter.ChanStopBlock && budgetLeft(ctx) {
			// Only a new hook (not a repeated delivery of one) carries messages.
			if serr := savepoint(ctx, tx, func() (err error) {
				at, err = msg.Claim(ctx, tx, msg.ClaimInput{RunID: req.RunID, Channel: slot.Channel}, mactor)
				return err
			}); serr != nil {
				at, resp.MsgErr = nil, errors.Join(resp.MsgErr, serr)
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE raw_hook_payloads SET event_id = ? WHERE id = ?`, first, rawID)
		return err
	})
	if err != nil {
		// The rollback took the raw payload with it; keep it on its own.
		return resp, errors.Join(err, storeRaw(ctx, d, req, req.RunID, res.SessionRef))
	}
	if !known {
		if req.RunID != "" {
			return resp, fmt.Errorf("run %s not found; payload stored", req.RunID)
		}
		return resp, nil
	}
	resp.RunID, resp.EventID = req.RunID, first
	if closer == nil && at == nil && req.Tmux != nil && first != 0 && live(hc.Status) {
		// The events may have ended the run (a turn failed on a usage limit). Not when
		// messages were claimed: the claim saw the run live after the events, and the
		// output carrying them must go out without delay.
		closer = &Manager{DB: d, Tmux: req.Tmux, Getenv: req.Getenv}
	}
	if closer != nil {
		var status string
		if d.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, req.RunID).Scan(&status) == nil && status == Ended {
			closer.killPanes(ctx, req.RunID, func(string) bool { return true })
		}
	}
	if res.Output != nil && !guarded {
		resp.Output = res.Output
	}
	if res.Prime == adapter.PrimeOutput && hd != nil && live(hc.Status) && budgetLeft(ctx) {
		// Claude SessionStart after a compaction: the orientation goes into the context.
		p, perr := BuildPrime(ctx, d, PrimeInput{RunID: req.RunID})
		if perr == nil {
			if out, ok := renderDelivery(hd, req.Kind, resp.Output, primeNote+p.Text()); ok {
				resp.Output = out
			}
		} else {
			resp.MsgErr = errors.Join(resp.MsgErr, perr)
		}
	}
	if at != nil {
		if out, ok := renderDelivery(hd, req.Kind, res.Output, at.Text(msg.DefaultMaxChars)); ok {
			resp.Output = out
			if slot.Channel != adapter.ChanStopBlock {
				// Hook context is delivered once the output is out (in time).
				nonce := at.Nonce
				resp.Delivered = func(ctx context.Context) error {
					return d.Tx(ctx, func(tx *sql.Tx) error {
						_, err := msg.Settle(ctx, tx, msg.SettleInput{Nonce: nonce, Outcome: msg.ChannelProved}, mactor)
						return err
					})
				}
			}
		}
		// Not rendered: the attempt stays dispatched and its proof timeout makes it
		// outcome_unknown (the Stop block: the next turn end does).
	}
	return resp, nil
}

// endsLastSession reports whether a session_end for harness session sid (or any, "")
// would close the run's last active session (see sessionEnded).
func endsLastSession(ctx context.Context, q db.Querier, runID, sid string) bool {
	var others int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE run_id = ? AND status = 'active'
		AND NOT (? = '' OR harness_session_id IS NULL OR harness_session_id = ?)`, runID, sid, sid).Scan(&others)
	return err == nil && others == 0
}

func loadHookContext(ctx context.Context, q db.Querier, runID string, hc *adapter.HookContext) (bool, error) {
	var deny, level string
	// The run keeps the level it started with; runs from before levels are strict. A
	// worktree without a branch is a detached review checkout.
	err := q.QueryRowContext(ctx, `
SELECT r.id, coalesce(r.task_id, ''), r.role, r.status, coalesce(w.path, ''), coalesce(w.branch, ''), p.deny,
       coalesce(r.guard, 'strict'), coalesce(w.mode = 'worktree' AND w.branch IS NULL, 0)
FROM runs r JOIN projects p ON p.id = r.project_id
LEFT JOIN workspaces w ON w.id = (SELECT max(id) FROM workspaces WHERE run_id = r.id)
WHERE r.id = ?`, runID).Scan(&hc.RunID, &hc.TaskID, &hc.Role, &hc.Status, &hc.Workdir, &hc.Branch, &deny, &level,
		&hc.Detached)
	hc.Guard = guard.Level(level)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(deny), &hc.Deny); err != nil {
		return false, fmt.Errorf("project deny list of run %s: %w", runID, err)
	}
	return true, nil
}

func insertRaw(ctx context.Context, q db.Querier, req HookRequest, runID, sessionRef string) (int64, error) {
	payload := compactPayload(req.Payload)
	if req.RawPayloads {
		payload = redactPayload(req.Payload)
	}
	// The run's own token (echoed by `env`, say) is never stored, whatever its form.
	if req.Getenv != nil {
		if t := req.Getenv(TokenEnv); len(t) >= 16 {
			payload = strings.ReplaceAll(payload, t, "[run token]")
		}
	}
	var id int64
	err := q.QueryRowContext(ctx, `INSERT INTO raw_hook_payloads (harness, hook, run_id, session_ref, payload)
		VALUES (?, ?, ?, ?, ?) RETURNING id`, req.Harness, req.Kind, nullStr(runID), nullStr(sessionRef),
		payload).Scan(&id)
	return id, err
}

// storeRaw keeps the payload when nothing else could be done with it.
func storeRaw(ctx context.Context, d *db.DB, req HookRequest, runID, sessionRef string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := insertRaw(ctx, tx, req, runID, sessionRef)
		return err
	})
}
