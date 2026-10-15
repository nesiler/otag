package msg

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/task"
)

// Defaults of Claim and Settle.
const (
	DefaultMaxItems    = 10   // deliveries one attempt carries
	DefaultMaxChars    = 9000 // rendered text of one attempt (Claude caps hook context near 10k)
	DefaultMaxAttempts = 5    // definite not-sent failures before dead
	retryBase          = 10 * time.Second
	retryMax           = 5 * time.Minute
	notReadyRetry      = 5 * time.Second // NotReady: the next try (no attempt counted)
	proofKeep          = time.Hour       // unmatched early proofs are dropped after this
)

// Reader is whoever reads deliveries: a run (RunID, its TaskID and Role, see
// ReaderFor) or, with RunID "", the human.
type Reader struct {
	ProjectID int64 // the human: 0 = every project
	RunID     string
	TaskID    string
	Role      string
}

// ReaderFor returns the Reader of run runID.
func ReaderFor(ctx context.Context, q db.Querier, runID string) (Reader, error) {
	r, err := loadRun(ctx, q, runID)
	if err != nil {
		return Reader{}, err
	}
	return Reader{ProjectID: r.ProjectID, RunID: r.ID, TaskID: r.TaskID, Role: r.Role}, nil
}

// readable is the SQL condition (on alias d) for the deliveries reader may take or
// read: those bound to its run, the queued ones of its task whose run is unbound or
// gone (a run's undelivered messages follow its task), and for a coordinator the
// queued coordinator-role ones without a live coordinator. The human reads human
// deliveries and the coordinator-role ones without a live coordinator.
func readable(r Reader) (string, []any) {
	if r.RunID == "" {
		cond := "(d.recipient_kind = 'human' OR (d.recipient_kind = 'role' AND (d.run_id IS NULL OR (d.state = 'queued' AND " +
			goneSQL("d.run_id") + "))))"
		if r.ProjectID != 0 {
			return "d.project_id = ? AND " + cond, []any{r.ProjectID}
		}
		return cond, nil
	}
	cond := "d.project_id = ? AND (d.run_id = ?"
	args := []any{r.ProjectID, r.RunID}
	if r.TaskID != "" {
		cond += " OR (d.recipient_kind IN ('run', 'task') AND d.task_id = ? AND d.state = 'queued' AND (d.run_id IS NULL OR " +
			goneSQL("d.run_id") + "))"
		args = append(args, r.TaskID)
	}
	if r.Role == RoleCoordinator {
		cond += " OR (d.recipient_kind = 'role' AND d.state = 'queued' AND (d.run_id IS NULL OR " + goneSQL("d.run_id") + "))"
	}
	return cond + ")", args
}

// unread states: not read and not terminal. An injected attempt in flight is not
// listed (it is arriving; it shows again if its outcome turns unknown), so reading
// never frees the run's injected slot.
const unreadSQL = "(d.state IN ('queued', 'delivered', 'outcome_unknown') OR (d.state = 'dispatched' AND d.injected = 0))"

// ClaimInput describes one delivery attempt into a run.
type ClaimInput struct {
	RunID    string
	Channel  string    // adapter.Chan*; adapter.Injected decides serialization
	Max      int       // deliveries in this attempt; 0 = DefaultMaxItems
	MaxChars int       // rendered text budget; 0 = DefaultMaxChars (the first item always fits; Render truncates it)
	Now      time.Time // zero = now
	IDs      []int64   // only these deliveries (still subject to every other rule); nil = any
	// Status, when set, is the run status the caller planned the attempt for (say idle
	// for a Claude mod submit): the claim takes nothing unless the run is still in it,
	// checked in the claim's own transaction.
	Status string
}

// claimable is the SQL condition (aliases d, m) for the queued deliveries run r may
// take now, with its arguments.
func claimable(r *runInfo, now string) (string, []any) {
	where, args := readable(Reader{ProjectID: r.ProjectID, RunID: r.ID, TaskID: r.TaskID, Role: r.Role})
	return "d.state = 'queued' AND " + where +
			" AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= ?) AND (m.expires_at IS NULL OR m.expires_at > ?)",
		append(args, now, now)
}

// Attempt is a claimed delivery attempt: its deliveries are dispatched with Nonce.
type Attempt struct {
	Nonce   string
	RunID   string
	Channel string
	Items   []Item
}

// Text renders the attempt's messages (with their markers) within maxChars.
func (at *Attempt) Text(maxChars int) string { return Render(at.Items, maxChars) }

// Claim takes the run's due queued deliveries (oldest first) into one attempt:
// queued -> dispatched with a fresh nonce, the channel and attempts+1, binding them to
// the run. It returns nil when there is nothing to claim or the run cannot take this
// channel now: a gone run; an injected channel (other than the launch prompt) unless
// the run is idle or running; the launch prompt unless it is queued or starting; and an
// injected channel while another injected attempt of the run is still dispatched (one
// in flight per run). Hook channels take any run that is not gone.
func Claim(ctx context.Context, tx *sql.Tx, in ClaimInput, a Actor) (*Attempt, error) {
	if in.Channel == "" || in.Channel == adapter.ChanCLI {
		return nil, apperr.New(apperr.Invalid, "claim needs a delivery channel")
	}
	if in.Max <= 0 {
		in.Max = DefaultMaxItems
	}
	if in.MaxChars <= 0 {
		in.MaxChars = DefaultMaxChars
	}
	now := ts(nowOr(in.Now))
	r, err := loadRun(ctx, tx, in.RunID)
	if err != nil {
		return nil, err
	}
	injected := adapter.Injected(in.Channel)
	switch {
	case gone(r.Status):
		return nil, nil
	case in.Channel == adapter.ChanLaunchPrompt:
		if r.Status != "queued" && r.Status != "starting" {
			return nil, nil
		}
	case injected && r.Status != "idle" && r.Status != "running":
		return nil, nil
	case in.Status != "" && r.Status != in.Status:
		return nil, nil
	}
	if injected {
		var busy int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM message_deliveries
			WHERE run_id = ? AND state = 'dispatched' AND injected = 1`, r.ID).Scan(&busy); err != nil {
			return nil, err
		}
		if busy > 0 {
			return nil, nil
		}
	}
	where, args := claimable(r, now)
	if len(in.IDs) > 0 {
		where += " AND d.id IN (?" + strings.Repeat(", ?", len(in.IDs)-1) + ")"
		for _, id := range in.IDs {
			args = append(args, id)
		}
	}
	ds, err := deliveries(ctx, tx, " JOIN messages m ON m.id = d.message_id WHERE "+where+
		" ORDER BY d.id LIMIT ?", append(args, in.Max)...)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	all, err := items(ctx, tx, ds)
	if err != nil {
		return nil, err
	}
	at := &Attempt{Nonce: NewNonce(), RunID: r.ID, Channel: in.Channel}
	size := 0
	for i := range all {
		it := &all[i]
		n := len(renderOne(*it)) + len(separator)
		if len(at.Items) > 0 && size+n > in.MaxChars {
			break
		}
		var c change
		c.set("run_id", r.ID)
		c.set("attempt_nonce", at.Nonce)
		c.set("channel", in.Channel)
		c.set("injected", injected)
		c.set("attempts", it.Delivery.Attempts+1)
		c.set("dispatched_at", now)
		c.set("receipt_ref", nil)
		c.set("next_attempt_at", nil)
		c.data = map[string]any{"nonce": at.Nonce, "channel": in.Channel, "attempt": it.Delivery.Attempts + 1}
		d := it.Delivery
		d.To.Run = &r.ID
		ok, err := move(ctx, tx, a, &d, Dispatched, "dispatched", c)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		fresh, err := GetDelivery(ctx, tx, d.ID)
		if err != nil {
			return nil, err
		}
		it.Delivery = *fresh
		at.Items = append(at.Items, *it)
		size += n
	}
	if len(at.Items) == 0 {
		return nil, nil
	}
	return at, nil
}

// Settle outcomes.
const (
	Sent          = "sent"      // the harness accepted the text; proof still to come
	NotSent       = "not_sent"  // definitely not delivered (adapter.ErrNotSent): queued again, or dead
	NotReady      = "not_ready" // definitely not delivered, only "not now" (adapter.ErrNotReady): queued again soon, the attempt given back, never dead
	Unknown       = "unknown"   // ambiguous: outcome_unknown, never re-sent automatically
	ChannelProved = "delivered" // the channel itself proves delivery (hook output produced in time)
)

// SettleInput is the outcome of the attempt with Nonce.
type SettleInput struct {
	Nonce       string
	Outcome     string          // Sent | NotSent | NotReady | Unknown | ChannelProved
	Receipt     adapter.Receipt // Sent: Ref is stored (and matched with early proofs); Channel replaces the claimed one
	Error       string
	RetryAt     time.Time // NotSent: zero = exponential backoff from 10s up to 5m; NotReady: zero = 5s
	MaxAttempts int       // NotSent: 0 = DefaultMaxAttempts; reaching it makes the delivery dead
}

// SettleResult reports what Settle changed.
type SettleResult struct {
	Changed []int64 `json:"changed"` // deliveries whose state changed
	// Stale: no delivery carries Nonce (any more). A late settle of a proven delivery
	// (proof before receipt) is not stale: its receipt is still stored.
	Stale bool `json:"stale"`
}

// Settle records the outcome of attempt Nonce for the deliveries that carry it.
// dispatched: Sent keeps it dispatched with the receipt (delivered at once when the
// receipt's proof arrived first); NotSent requeues it (dead after MaxAttempts);
// NotReady requeues it soon and gives the attempt back (never dead); Unknown ->
// outcome_unknown; ChannelProved -> delivered. outcome_unknown (a proof timeout passed
// during the call): Sent stores the receipt for a late proof, NotSent and NotReady
// requeue, ChannelProved delivers. delivered or read: only a missing receipt is
// stored. Other states are left alone.
func Settle(ctx context.Context, tx *sql.Tx, in SettleInput, a Actor) (SettleResult, error) {
	var out SettleResult
	if in.Nonce == "" {
		return out, apperr.New(apperr.Invalid, "settle needs the attempt nonce")
	}
	ds, err := deliveries(ctx, tx, " WHERE d.attempt_nonce = ? ORDER BY d.id", in.Nonce)
	if err != nil {
		return out, err
	}
	if len(ds) == 0 {
		out.Stale = true
		return out, nil
	}
	max := in.MaxAttempts
	if max <= 0 {
		max = DefaultMaxAttempts
	}
	// An early proof of the receipt proves every delivery the attempt carries: it is
	// consumed once they all have it.
	proofs := map[int64]bool{}
	for i := range ds {
		d := &ds[i]
		var changed bool
		switch {
		case in.Outcome == Sent && (d.State == Dispatched || d.State == OutcomeUnknown):
			if changed, err = storeReceipt(ctx, tx, d, in.Receipt, proofs, a); err != nil {
				return out, err
			}
		case in.Outcome == Sent && (d.State == Delivered || d.State == Read) && d.Receipt == nil && in.Receipt.Ref != "":
			if _, err := tx.ExecContext(ctx, `UPDATE message_deliveries SET receipt_ref = ? WHERE id = ?`,
				in.Receipt.Ref, d.ID); err != nil {
				return out, err
			}
		case in.Outcome == NotSent && (d.State == Dispatched || d.State == OutcomeUnknown):
			var c change
			c.set("last_error", nullStr(in.Error))
			c.data = map[string]any{"nonce": in.Nonce, "error": in.Error}
			if d.Attempts >= max {
				c.set("closed_at", db.Now())
				changed, err = move(ctx, tx, a, d, Dead, Dead, c)
				break
			}
			retry := in.RetryAt
			if retry.IsZero() {
				retry = clock().Add(backoff(d.Attempts))
			}
			c.set("next_attempt_at", ts(retry))
			changed, err = move(ctx, tx, a, d, Queued, "requeued", c)
		case in.Outcome == NotReady && (d.State == Dispatched || d.State == OutcomeUnknown):
			// Nothing reached the session: the claim's attempt is given back.
			var c change
			c.set("last_error", nullStr(in.Error))
			given := d.Attempts - 1
			if given < 0 {
				given = 0
			}
			c.set("attempts", given)
			retry := in.RetryAt
			if retry.IsZero() {
				retry = clock().Add(notReadyRetry)
			}
			c.set("next_attempt_at", ts(retry))
			c.data = map[string]any{"nonce": in.Nonce, "error": in.Error, "not_ready": true}
			changed, err = move(ctx, tx, a, d, Queued, "requeued", c)
		case in.Outcome == Unknown && d.State == Dispatched:
			var c change
			c.set("last_error", nullStr(in.Error))
			c.data = map[string]any{"nonce": in.Nonce, "error": in.Error}
			changed, err = move(ctx, tx, a, d, OutcomeUnknown, OutcomeUnknown, c)
		case in.Outcome == ChannelProved && (d.State == Dispatched || d.State == OutcomeUnknown):
			changed, err = deliver(ctx, tx, d, ProofChannel, in.Nonce, a)
		case in.Outcome != Sent && in.Outcome != NotSent && in.Outcome != NotReady && in.Outcome != Unknown &&
			in.Outcome != ChannelProved:
			return out, apperr.New(apperr.Invalid, "unknown settle outcome %q", in.Outcome)
		}
		if err != nil {
			return out, err
		}
		if changed {
			out.Changed = append(out.Changed, d.ID)
		}
	}
	for pid := range proofs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_proofs WHERE id = ?`, pid); err != nil {
			return out, err
		}
	}
	return out, nil
}

// storeReceipt stores a Sent attempt's receipt, then delivers d when a proof with that
// ref arrived before it; the proof's id goes into used (Settle consumes it after the
// attempt's last delivery).
func storeReceipt(ctx context.Context, tx *sql.Tx, d *Delivery, rc adapter.Receipt, used map[int64]bool, a Actor) (bool, error) {
	q := `UPDATE message_deliveries SET receipt_ref = coalesce(?, receipt_ref), channel = coalesce(?, channel),
		version = version + 1 WHERE id = ? AND version = ?`
	if err := cas(tx.ExecContext(ctx, q, nullStr(rc.Ref), nullStr(rc.Channel), d.ID, d.Version)); err != nil {
		return false, err
	}
	d.Version++
	if rc.Ref != "" {
		d.Receipt = &rc.Ref
	}
	if err := recordDelivery(ctx, tx, a, d, "msg.sent", "", "", map[string]any{
		"message": d.MessageID, "nonce": str(d.Nonce), "ref": rc.Ref, "channel": rc.Channel}); err != nil {
		return false, err
	}
	if rc.Ref == "" {
		return false, nil
	}
	// Only a proof seen since this attempt was dispatched belongs to it.
	var pid int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM message_proofs WHERE run_id = ? AND ref = ? AND created_at >= ?`,
		str(d.To.Run), rc.Ref, coalesce(d.DispatchedAt, "")).Scan(&pid)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	used[pid] = true
	return deliver(ctx, tx, d, ProofReceipt, rc.Ref, a)
}

func cas(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return apperr.Retryable(apperr.Conflict, "delivery modified concurrently")
	}
	return nil
}

func backoff(attempts int) time.Duration {
	d := retryBase
	for i := 1; i < attempts && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

// ProveInput is what a hook (or sidecar) event of a run shows about deliveries.
type ProveInput struct {
	RunID   string
	Markers []int64  // delivery ids whose marker the payload carries (ScanMarkers)
	Refs    []string // harness refs (HookResult.Refs), matched with receipt_ref or the nonce
	// Channel proves every dispatched delivery of the run on this channel (a Stop with
	// stop_hook_active proves the Stop block). The caller checks the session.
	Channel string
	EventID int64 // the event carrying the proof, 0 = none
	// Nonce, when set, limits every proof above to that attempt (e.g. the Stop block
	// whose continuation this Stop is).
	Nonce string
}

// Prove delivers the run's deliveries that the input proves; it returns their ids.
// Only the run's own deliveries are proven: a marker of another run's delivery is
// ignored. A marker proves a delivery that is dispatched or outcome_unknown (late
// proof), or queued after an attempt reported not sent. A ref that matches nothing yet
// is kept (message_proofs) so the attempt's Settle(Sent) with that receipt proves it.
func Prove(ctx context.Context, tx *sql.Tx, in ProveInput, a Actor) ([]int64, error) {
	r, err := loadRun(ctx, tx, in.RunID)
	if err != nil {
		return nil, err
	}
	var proven []int64
	ref := func(s string) string {
		if in.EventID != 0 {
			return "event:" + strconv.FormatInt(in.EventID, 10)
		}
		return s
	}
	try := func(ds []Delivery, proof, proofRef string) error {
		for i := range ds {
			if in.Nonce != "" && str(ds[i].Nonce) != in.Nonce {
				continue
			}
			ok, err := deliver(ctx, tx, &ds[i], proof, ref(proofRef), a)
			if err != nil {
				return err
			}
			if ok {
				proven = append(proven, ds[i].ID)
			}
		}
		return nil
	}
	for _, id := range in.Markers {
		ds, err := deliveries(ctx, tx, ` WHERE d.id = ? AND d.run_id = ?
			AND (d.state IN ('dispatched', 'outcome_unknown') OR (d.state = 'queued' AND d.attempts > 0))`, id, r.ID)
		if err != nil {
			return nil, err
		}
		if err := try(ds, ProofMarker, Marker(id)); err != nil {
			return nil, err
		}
	}
	for _, rf := range in.Refs {
		if rf == "" {
			continue
		}
		ds, err := deliveries(ctx, tx, ` WHERE d.run_id = ? AND (d.receipt_ref = ? OR d.attempt_nonce = ?)`, r.ID, rf, rf)
		if err != nil {
			return nil, err
		}
		if len(ds) == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO message_proofs (project_id, run_id, ref, event_id, created_at)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT (run_id, ref) DO UPDATE SET event_id = excluded.event_id,
				created_at = excluded.created_at`, r.ProjectID, r.ID, rf,
				sql.NullInt64{Int64: in.EventID, Valid: in.EventID != 0}, ts(clock())); err != nil {
				return nil, err
			}
			continue
		}
		var open []Delivery
		for _, d := range ds {
			if d.State == Dispatched || d.State == OutcomeUnknown {
				open = append(open, d)
			}
		}
		if err := try(open, ProofReceipt, rf); err != nil {
			return nil, err
		}
	}
	if in.Channel != "" {
		// One attempt (Nonce) is also proven late, after its proof timeout made it
		// unknown; never one a turn end made unknown.
		where, args := ` WHERE d.run_id = ? AND d.state = 'dispatched' AND d.channel = ?`, []any{r.ID, in.Channel}
		if in.Nonce != "" {
			where = ` WHERE d.run_id = ? AND d.channel = ? AND d.attempt_nonce = ?
				AND (d.state = 'dispatched' OR (d.state = 'outcome_unknown' AND d.last_error = ?))`
			args = []any{r.ID, in.Channel, in.Nonce, ReasonProofTimeout}
		}
		ds, err := deliveries(ctx, tx, where, args...)
		if err != nil {
			return nil, err
		}
		if err := try(ds, ProofChannel, in.Channel); err != nil {
			return nil, err
		}
	}
	return proven, nil
}

// deliver moves d to delivered with its proof and applies what receiving it means.
func deliver(ctx context.Context, tx *sql.Tx, d *Delivery, proof, proofRef string, a Actor) (bool, error) {
	var c change
	now := db.Now()
	c.set("proof", proof)
	c.set("proof_ref", nullStr(proofRef))
	c.set("delivered_at", now)
	c.set("next_attempt_at", nil)
	c.data = map[string]any{"proof": proof, "ref": proofRef, "nonce": str(d.Nonce)}
	if d.State == Queued {
		c.set("closed_at", nil) // a requeued attempt that did arrive
	}
	ok, err := move(ctx, tx, a, d, Delivered, Delivered, c)
	if err != nil || !ok {
		return ok, err
	}
	return true, received(ctx, tx, d, a)
}

// MarkRead marks deliveries ids as read by reader (`otag inbox`): queued ones are taken
// directly (channel cli); delivered, outcome_unknown and hook-channel dispatched ones
// too. A dispatched injected delivery is skipped: its attempt holds the run's injected
// slot until it is settled or proven. Deliveries the reader may not read, or that are
// read or closed already, are skipped too. It returns the ids it marked.
//
// A run reader must still be live, checked in tx: a stopping, parked or ended run
// takes nothing (CONFLICT), so its messages stay queued for the task's next run (a
// parked run's answer must stay queued for the resume).
func MarkRead(ctx context.Context, tx *sql.Tx, ids []int64, reader Reader, a Actor) ([]int64, error) {
	if reader.RunID != "" {
		r, err := loadRun(ctx, tx, reader.RunID)
		if err != nil {
			return nil, err
		}
		if gone(r.Status) {
			return nil, apperr.New(apperr.Conflict, "run %s is %s: its messages stay queued for the task's next run", r.ID, r.Status)
		}
	}
	where, args := readable(reader)
	var out []int64
	for _, id := range ids {
		ds, err := deliveries(ctx, tx, " WHERE d.id = ? AND "+unreadSQL+" AND "+where, append([]any{id}, args...)...)
		if err != nil {
			return nil, err
		}
		if len(ds) == 0 {
			continue
		}
		ok, err := markRead(ctx, tx, &ds[0], reader, a)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// Take selects reader's unread deliveries (as Inbox, oldest first, up to limit) and
// marks them read in the same transaction: it returns only the items it took, with
// their deliveries as they are now. A delivery a dispatcher claimed in the meantime is
// never returned, so an agent never sees a message twice (`otag inbox`).
func Take(ctx context.Context, tx *sql.Tx, reader Reader, limit int, a Actor) ([]Item, error) {
	its, err := Inbox(ctx, tx, reader, false, limit)
	if err != nil || len(its) == 0 {
		return nil, err
	}
	ids := make([]int64, len(its))
	for i, it := range its {
		ids[i] = it.Delivery.ID
	}
	taken, err := MarkRead(ctx, tx, ids, reader, a)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(taken))
	for _, it := range its {
		if !slices.Contains(taken, it.Delivery.ID) {
			continue
		}
		d, err := GetDelivery(ctx, tx, it.Delivery.ID)
		if err != nil {
			return nil, err
		}
		it.Delivery = *d
		out = append(out, it)
	}
	return out, nil
}

func markRead(ctx context.Context, tx *sql.Tx, d *Delivery, reader Reader, a Actor) (bool, error) {
	if d.State == Read || d.State == Expired || d.State == Dead || d.State == Dispatched && d.Injected {
		return false, nil
	}
	var c change
	now := db.Now()
	c.set("read_at", now)
	c.set("delivered_at", coalesce(d.DeliveredAt, now))
	c.set("next_attempt_at", nil)
	if d.Channel == nil || d.State == Queued {
		c.set("channel", adapter.ChanCLI)
	}
	if d.Proof == nil {
		c.set("proof", ProofCLI)
	}
	switch {
	case reader.RunID != "" && d.To.Kind != ToHuman:
		c.set("run_id", reader.RunID)
		d.To.Run = &reader.RunID
	case reader.RunID == "" && d.To.Kind == ToRole:
		c.set("run_id", nil) // the human took it over from a coordinator that has gone
		d.To.Run = nil
	}
	who := "human"
	if reader.RunID != "" {
		who = "run:" + reader.RunID
	}
	c.data = map[string]any{"by": who}
	ok, err := move(ctx, tx, a, d, Read, Read, c)
	if err != nil || !ok {
		return ok, err
	}
	return true, received(ctx, tx, d, a)
}

func coalesce(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

// received applies what a delivery reaching its run means: the answer to the open
// ask of the run's task moves the task waiting -> running, when the task is still the
// run's, the run is not in a harness dialog and no other ask is open.
func received(ctx context.Context, tx *sql.Tx, d *Delivery, a Actor) error {
	run := str(d.To.Run)
	if run == "" {
		return nil
	}
	var kind string
	var taskID sql.NullString
	var ask bool
	err := tx.QueryRowContext(ctx, `SELECT m.kind, q.task_id, coalesce(q.ask, 0) FROM messages m
		LEFT JOIN messages q ON q.id = m.reply_to_id WHERE m.id = ?`, d.MessageID).Scan(&kind, &taskID, &ask)
	if err != nil || kind != KindAnswer || !ask || !taskID.Valid {
		return err
	}
	r, err := loadRun(ctx, tx, run)
	if err != nil {
		return err
	}
	if r.Status != "idle" && r.Status != "running" {
		return nil // starting: the launch moves the task; waiting: a dialog is open
	}
	t, err := task.Get(ctx, tx, taskID.String)
	if err != nil {
		return err
	}
	if t.Status != task.Waiting || t.CurrentRunID != run {
		return nil
	}
	if open, err := openAsk(ctx, tx, t.ID); err != nil || open != nil {
		return err
	}
	_, err = task.Transition(ctx, tx, t.ID, task.Running, "", 0, taskActor(a, run))
	return err
}

// ReasonProofTimeout is the last_error of a delivery ProofTimeout made unknown. A late
// correlated proof of the same attempt still delivers it (Prove: Channel with Nonce).
const ReasonProofTimeout = "no proof of delivery after the timeout"

// ProofTimeout moves deliveries dispatched before `before` without proof to
// outcome_unknown (never re-sent automatically; `otag needs` shows them). channel ""
// = every channel. It returns the ids it moved.
func ProofTimeout(ctx context.Context, tx *sql.Tx, before time.Time, channel string, limit int, a Actor) ([]int64, error) {
	if limit <= 0 {
		limit = 500
	}
	where := " WHERE d.state = 'dispatched' AND d.dispatched_at < ?"
	args := []any{ts(before)}
	if channel != "" {
		where += " AND d.channel = ?"
		args = append(args, channel)
	}
	ds, err := deliveries(ctx, tx, where+" ORDER BY d.id LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	var out []int64
	for i := range ds {
		var c change
		c.set("last_error", ReasonProofTimeout)
		c.data = map[string]any{"nonce": str(ds[i].Nonce), "reason": "proof timeout"}
		ok, err := move(ctx, tx, a, &ds[i], OutcomeUnknown, OutcomeUnknown, c)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ds[i].ID)
		}
	}
	return out, nil
}

// MarkUnknown moves the run's dispatched deliveries on channel (only attempt nonce,
// when set) to outcome_unknown. P3: a Stop without stop_hook_active means the previous
// Stop block was not continued (Esc, kill): MarkUnknown(run, adapter.ChanStopBlock, "").
func MarkUnknown(ctx context.Context, tx *sql.Tx, runID, channel, nonce, reason string, a Actor) ([]int64, error) {
	where := " WHERE d.run_id = ? AND d.state = 'dispatched' AND d.channel = ?"
	args := []any{runID, channel}
	if nonce != "" {
		where += " AND d.attempt_nonce = ?"
		args = append(args, nonce)
	}
	ds, err := deliveries(ctx, tx, where+" ORDER BY d.id", args...)
	if err != nil {
		return nil, err
	}
	var out []int64
	for i := range ds {
		var c change
		c.set("last_error", nullStr(reason))
		c.data = map[string]any{"nonce": str(ds[i].Nonce), "reason": reason}
		ok, err := move(ctx, tx, a, &ds[i], OutcomeUnknown, OutcomeUnknown, c)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ds[i].ID)
		}
	}
	return out, nil
}

// Due returns the runs that have queued deliveries an injected channel could take now
// (run idle or running, no injected attempt in flight), fairly ordered: least recently
// served by an injected attempt first, then by the oldest due delivery. P3's daemon
// step walks them in this order within its time budget. limit 0 = 50.
func Due(ctx context.Context, q db.Querier, now time.Time, limit int) ([]string, error) {
	return DueRuns(ctx, q, DueInput{Now: now, Limit: limit})
}

// DueInput selects the runs of DueRuns.
type DueInput struct {
	Now   time.Time // zero = now
	Limit int       // 0 = 50
	// Eligible, when set, drops the runs no injected channel can serve now (a busy
	// Claude run, Codex in hooks mode, a harness without injected delivery) before
	// the order and the limit: such a run is never served by an injected attempt, so
	// it would sort first forever and, past the limit, starve the others. It is asked
	// only about runs that have due deliveries.
	Eligible func(runID string) bool
}

// DueRuns is Due with the options of in.
func DueRuns(ctx context.Context, q db.Querier, in DueInput) ([]string, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	t := ts(nowOr(in.Now))
	rows, err := q.QueryContext(ctx, `
SELECT r.id, coalesce(r.task_id, ''), r.role, r.status, r.project_id,
       coalesce((SELECT max(x.dispatched_at) FROM message_deliveries x WHERE x.run_id = r.id AND x.injected = 1), '')
FROM runs r
WHERE r.status IN ('idle', 'running')
  AND NOT EXISTS (SELECT 1 FROM message_deliveries x WHERE x.run_id = r.id AND x.state = 'dispatched' AND x.injected = 1)`)
	if err != nil {
		return nil, err
	}
	type cand struct {
		r      runInfo
		served string
		oldest int64
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.r.ID, &c.r.TaskID, &c.r.Role, &c.r.Status, &c.r.ProjectID, &c.served); err != nil {
			_ = rows.Close()
			return nil, err
		}
		cs = append(cs, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var due []cand
	for _, c := range cs {
		where, args := claimable(&c.r, t)
		var oldest sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT min(d.id) FROM message_deliveries d JOIN messages m ON m.id = d.message_id
			WHERE `+where, args...).Scan(&oldest); err != nil {
			return nil, err
		}
		if oldest.Valid && (in.Eligible == nil || in.Eligible(c.r.ID)) {
			c.oldest = oldest.Int64
			due = append(due, c)
		}
	}
	slices.SortFunc(due, func(a, b cand) int {
		if a.served != b.served {
			return strings.Compare(a.served, b.served)
		}
		return int(a.oldest - b.oldest)
	})
	out := []string{}
	for i := 0; i < len(due) && i < limit; i++ {
		out = append(out, due[i].r.ID)
	}
	return out, nil
}

// ExpireResult counts what Expire changed.
type ExpireResult struct {
	Expired int `json:"expired"`
	Dead    int `json:"dead"`
	Proofs  int `json:"proofs"` // unmatched early proofs dropped
}

// Expire sweeps queued deliveries: past their message's expires_at -> expired; whose
// target can never take them -> dead (a task that is done or cancelled, a task-less
// run that has gone). It also drops early proofs nobody claimed within an hour.
func Expire(ctx context.Context, tx *sql.Tx, now time.Time, limit int, a Actor) (ExpireResult, error) {
	var out ExpireResult
	if limit <= 0 {
		limit = 500
	}
	t := ts(nowOr(now))
	ds, err := deliveries(ctx, tx, ` JOIN messages m ON m.id = d.message_id
		WHERE d.state = 'queued' AND m.expires_at IS NOT NULL AND m.expires_at <= ? ORDER BY d.id LIMIT ?`, t, limit)
	if err != nil {
		return out, err
	}
	for i := range ds {
		if err := expire(ctx, tx, &ds[i], Expired, "expired", a); err != nil {
			return out, err
		}
		out.Expired++
	}
	ds, err = deliveries(ctx, tx, ` WHERE d.state = 'queued' AND (
		(d.task_id IS NOT NULL AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = d.task_id AND t.status IN ('done', 'delivered', 'cancelled')))
		OR (d.recipient_kind = 'run' AND d.task_id IS NULL AND `+goneSQL("d.run_id")+`))
		ORDER BY d.id LIMIT ?`, limit)
	if err != nil {
		return out, err
	}
	for i := range ds {
		why := "the target run has gone"
		if ds[i].To.Task != nil {
			why = "task " + *ds[i].To.Task + " is closed"
		}
		// otag's own re-orientation of a run (hook bridge) is only stale once its run has
		// gone: expired, not a dead delivery for otag needs.
		state := Dead
		var prime bool
		if tx.QueryRowContext(ctx, `SELECT client_msg_id LIKE 'prime:%' FROM messages WHERE id = ?`,
			ds[i].MessageID).Scan(&prime) == nil && prime {
			state = Expired
		}
		if err := expire(ctx, tx, &ds[i], state, why, a); err != nil {
			return out, err
		}
		if state == Expired {
			out.Expired++
		} else {
			out.Dead++
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM message_proofs WHERE created_at < ?`, ts(nowOr(now).Add(-proofKeep)))
	if err != nil {
		return out, err
	}
	n, _ := res.RowsAffected()
	out.Proofs = int(n)
	return out, nil
}

// Requeue puts an outcome_unknown or dead delivery back in the queue: a person decided
// to send it again (`otag send --resend`). Its attempt count restarts.
func Requeue(ctx context.Context, tx *sql.Tx, deliveryID int64, a Actor) (*Delivery, error) {
	d, err := GetDelivery(ctx, tx, deliveryID)
	if err != nil {
		return nil, err
	}
	if d.State != OutcomeUnknown && d.State != Dead {
		return nil, apperr.New(apperr.InvalidTransition, "delivery %d is %s; only outcome_unknown or dead deliveries are re-sent",
			d.ID, d.State)
	}
	var c change
	c.set("attempts", 0)
	c.set("next_attempt_at", nil)
	c.set("closed_at", nil)
	c.data = map[string]any{"reason": "resend"}
	ok, err := move(ctx, tx, a, d, Queued, "requeued", c)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.Retryable(apperr.Conflict, "delivery %d changed concurrently", d.ID)
	}
	return GetDelivery(ctx, tx, d.ID)
}

// Dispatcher makes one synchronous delivery attempt of a queued delivery: Claim, the
// adapter call outside any transaction, Settle. P3 implements it (run package); the
// CLI calls it after send, reply and ask commit, the daemon once per due delivery.
type Dispatcher interface {
	Attempt(ctx context.Context, deliveryID int64) AttemptResult
}

// RunDispatcher is optionally implemented by a Dispatcher that attempts per run (one
// attempt, ClaimInput{RunID}, may carry several of the run's queued deliveries). The CLI
// prefers it: one AttemptRun per run the new message's deliveries are bound to. It
// returns one result per delivery of the run it looked at.
type RunDispatcher interface {
	AttemptRun(ctx context.Context, runID string) []AttemptResult
}

// AttemptResult is the outcome of one Dispatcher.Attempt.
type AttemptResult struct {
	Delivery int64  `json:"delivery"`
	Result   string `json:"result"` // dispatched | delivered | queued | skipped | failed
	Reason   string `json:"reason,omitempty"`
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }
