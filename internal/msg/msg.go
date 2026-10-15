// Package msg is otag's durable message outbox (phase 4, docs/MESSAGES.md): messages,
// their per-recipient deliveries and the delivery state machine.
//
// Mutating functions take a *sql.Tx opened with BEGIN IMMEDIATE (db.DB.Tx); every
// state change is a compare-and-swap on message_deliveries.version (or messages.closed_at)
// plus an events row in that tx. A lost CAS means another path won; it is a no-op, never
// an error. The package never talks to a harness: P3's dispatcher and the hook bridge
// claim attempts here, deliver outside the transaction and settle them here.
package msg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/event"
	"github.com/nesiler/otag/internal/task"
)

// Message kinds.
const (
	KindQuestion = "question"
	KindInfo     = "info"
	KindAnswer   = "answer"
	KindCommand  = "command"
)

// Sender kinds.
const (
	SenderRun    = "run"    // verified with the run's token
	SenderHuman  = "human"  // a person (or a plain coordinator session) at the CLI
	SenderSystem = "system" // the daemon
)

// Recipient kinds.
const (
	ToRun   = "run"   // a run; follows its task to the next run if it ends undelivered
	ToTask  = "task"  // whichever run of the task takes it first (its next run when none is live)
	ToRole  = "role"  // the project's live coordinator, else the human
	ToHuman = "human" // read with `otag inbox` outside a run
)

// RoleCoordinator is the only role recipient: the coordinator of the project.
const RoleCoordinator = "coordinator"

// Delivery states.
const (
	Queued         = "queued"          // waiting for a channel
	Dispatched     = "dispatched"      // an attempt (nonce) is in flight or sent without proof
	Delivered      = "delivered"       // proven: in the session
	Read           = "read"            // read with otag inbox, or the recipient replied
	OutcomeUnknown = "outcome_unknown" // may or may not have arrived; never re-sent automatically
	Expired        = "expired"         // its time passed, or the question was answered or closed
	Dead           = "dead"            // undeliverable: target gone, or too many definite failures
)

// Proof kinds (message_deliveries.proof).
const (
	ProofMarker  = "marker"  // the delivery's marker in a hook payload of its run
	ProofReceipt = "receipt" // a harness ref matching the attempt's receipt
	ProofChannel = "channel" // the channel itself proved it (hook output produced, Stop continued)
	ProofCLI     = "cli"     // the recipient read it with otag inbox (or replied)
)

// Close reasons of a question.
const (
	ClosedAnswered    = "answered"
	ClosedAskerDone   = "asker_done"
	ClosedAskerFailed = "asker_failed"
	ClosedSuperseded  = "superseded" // a later run of the task asked anew
	ClosedCancelled   = "cancelled"
)

// MaxBody is the largest message body in bytes.
const MaxBody = 32 * 1024

// Actor is who changes messages; recorded on events.
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

// Sender is who sends a message. The CLI verifies a run sender with its token before
// it gets here; msg trusts it.
type Sender struct {
	Kind  string // SenderRun | SenderHuman | SenderSystem
	RunID string // SenderRun
	Actor string // run:<id> | user:<name> | daemon
}

// Recipient is an address resolved in a project.
type Recipient struct {
	Kind string // ToRun | ToTask | ToRole | ToHuman
	Ref  string // run id | task id | RoleCoordinator | "human"
}

// Message is one message with its deliveries (JSON shape of docs/MESSAGES.md).
type Message struct {
	ID           int64      `json:"id"`
	ClientMsgID  string     `json:"client_msg_id"`
	Project      string     `json:"project"`
	ProjectID    int64      `json:"-"`
	Kind         string     `json:"kind"`
	From         From       `json:"from"`
	Task         *string    `json:"task"`
	ReplyTo      *int64     `json:"reply_to"`
	Body         string     `json:"body"`
	Ask          bool       `json:"ask"`
	Park         bool       `json:"park"`
	ClosedAt     *string    `json:"closed_at"`
	ClosedReason *string    `json:"closed_reason"`
	AnswerID     *int64     `json:"answer_id"`
	ExpiresAt    *string    `json:"expires_at"`
	CreatedAt    string     `json:"created_at"`
	Deliveries   []Delivery `json:"deliveries,omitempty"`
}

// From describes the sender.
type From struct {
	Kind  string  `json:"kind"`
	Run   *string `json:"run"`
	Task  *string `json:"task"` // the sender run's task
	Role  *string `json:"role"` // the sender run's role
	Actor string  `json:"actor"`
}

// Delivery is one recipient's copy of a message.
type Delivery struct {
	ID            int64   `json:"id"`
	MessageID     int64   `json:"message"`
	ProjectID     int64   `json:"-"`
	To            To      `json:"to"`
	State         string  `json:"state"`
	Channel       *string `json:"channel"`
	Injected      bool    `json:"injected"`
	Nonce         *string `json:"nonce"`
	Attempts      int     `json:"attempts"`
	Receipt       *string `json:"receipt"`
	Proof         *string `json:"proof"`
	ProofRef      *string `json:"proof_ref"`
	NextAttemptAt *string `json:"next_attempt_at"`
	LastError     *string `json:"last_error"`
	DispatchedAt  *string `json:"dispatched_at"`
	DeliveredAt   *string `json:"delivered_at"`
	ReadAt        *string `json:"read_at"`
	ClosedAt      *string `json:"closed_at"`
	Version       int64   `json:"version"`
	CreatedAt     string  `json:"created_at"`
}

// To is a delivery's recipient and the run it is bound to.
type To struct {
	Kind      string  `json:"kind"`
	Recipient string  `json:"recipient"`
	Run       *string `json:"run"`
	Task      *string `json:"task"`
}

// Item is a message as one recipient sees it: inbox entries and claimed attempts.
type Item struct {
	Message
	Delivery Delivery `json:"delivery"`
}

// Marker returns the text that marks delivery id inside injected or hook text.
func Marker(deliveryID int64) string { return fmt.Sprintf("[otag#d%d]", deliveryID) }

const selectMessage = `
SELECT m.id, m.client_msg_id, p.name, m.project_id, m.kind, m.sender_kind, m.sender_run_id, sr.task_id, sr.role,
       m.sender_actor, m.task_id, m.reply_to_id, m.body, m.ask, m.park, m.closed_at, m.closed_reason,
       (SELECT a.id FROM messages a WHERE a.reply_to_id = m.id AND a.kind = 'answer'), m.expires_at, m.created_at
FROM messages m
JOIN projects p ON p.id = m.project_id
LEFT JOIN runs sr ON sr.id = m.sender_run_id`

type scanner interface{ Scan(...any) error }

func scanMessage(s scanner) (*Message, error) {
	var m Message
	var run, runTask, role, task, closedAt, closedReason, expires sql.NullString
	var reply, answer sql.NullInt64
	err := s.Scan(&m.ID, &m.ClientMsgID, &m.Project, &m.ProjectID, &m.Kind, &m.From.Kind, &run, &runTask, &role,
		&m.From.Actor, &task, &reply, &m.Body, &m.Ask, &m.Park, &closedAt, &closedReason, &answer, &expires, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	m.From.Run, m.From.Task, m.From.Role = ptr(run), ptr(runTask), ptr(role)
	m.Task, m.ClosedAt, m.ClosedReason, m.ExpiresAt = ptr(task), ptr(closedAt), ptr(closedReason), ptr(expires)
	m.ReplyTo, m.AnswerID = iptr(reply), iptr(answer)
	return &m, nil
}

const selectDelivery = `
SELECT d.id, d.message_id, d.project_id, d.recipient_kind, d.recipient, d.run_id, d.task_id, d.state, d.channel,
       d.injected, d.attempt_nonce, d.attempts, d.receipt_ref, d.proof, d.proof_ref, d.next_attempt_at, d.last_error,
       d.dispatched_at, d.delivered_at, d.read_at, d.closed_at, d.version, d.created_at
FROM message_deliveries d`

func scanDelivery(s scanner) (*Delivery, error) {
	var d Delivery
	var run, task, channel, nonce, receipt, proof, proofRef, next, lastErr, disp, deliv, read, closed sql.NullString
	err := s.Scan(&d.ID, &d.MessageID, &d.ProjectID, &d.To.Kind, &d.To.Recipient, &run, &task, &d.State, &channel,
		&d.Injected, &nonce, &d.Attempts, &receipt, &proof, &proofRef, &next, &lastErr, &disp, &deliv, &read, &closed,
		&d.Version, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	d.To.Run, d.To.Task = ptr(run), ptr(task)
	d.Channel, d.Nonce, d.Receipt, d.Proof, d.ProofRef = ptr(channel), ptr(nonce), ptr(receipt), ptr(proof), ptr(proofRef)
	d.NextAttemptAt, d.LastError = ptr(next), ptr(lastErr)
	d.DispatchedAt, d.DeliveredAt, d.ReadAt, d.ClosedAt = ptr(disp), ptr(deliv), ptr(read), ptr(closed)
	return &d, nil
}

// Get returns message id with its deliveries.
func Get(ctx context.Context, q db.Querier, id int64) (*Message, error) {
	m, err := getMessage(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if m.Deliveries, err = deliveries(ctx, q, " WHERE d.message_id = ? ORDER BY d.id", id); err != nil {
		return nil, err
	}
	return m, nil
}

func getMessage(ctx context.Context, q db.Querier, id int64) (*Message, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, selectMessage+" WHERE m.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.NotFound, "message %d not found", id)
	}
	return m, err
}

// GetDelivery returns one delivery.
func GetDelivery(ctx context.Context, q db.Querier, id int64) (*Delivery, error) {
	ds, err := deliveries(ctx, q, " WHERE d.id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		return nil, apperr.New(apperr.NotFound, "delivery %d not found", id)
	}
	return &ds[0], nil
}

func deliveries(ctx context.Context, q db.Querier, where string, args ...any) ([]Delivery, error) {
	rows, err := q.QueryContext(ctx, selectDelivery+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// items pairs deliveries with their messages (without the messages' delivery lists).
func items(ctx context.Context, q db.Querier, ds []Delivery) ([]Item, error) {
	out := make([]Item, 0, len(ds))
	cache := map[int64]*Message{}
	for _, d := range ds {
		m, ok := cache[d.MessageID]
		if !ok {
			var err error
			if m, err = getMessage(ctx, q, d.MessageID); err != nil {
				return nil, err
			}
			cache[d.MessageID] = m
		}
		out = append(out, Item{Message: *m, Delivery: d})
	}
	return out, nil
}

// ---------------------------------------------------------------- state changes

// change is one CAS move of a delivery: state `to` plus extra column assignments.
type change struct {
	cols []string // "col = ?"
	args []any
	data map[string]any // event data
}

func (c *change) set(col string, v any) {
	c.cols = append(c.cols, col+" = ?")
	c.args = append(c.args, v)
}

// move moves d to state `to` (CAS on version and state) and records msg.<kind>. It
// returns false without error when the CAS is lost: another path changed d first.
func move(ctx context.Context, tx *sql.Tx, a Actor, d *Delivery, to, kind string, c change) (bool, error) {
	q := "UPDATE message_deliveries SET state = ?, version = version + 1"
	args := []any{to}
	for i, col := range c.cols {
		q += ", " + col
		args = append(args, c.args[i])
	}
	q += " WHERE id = ? AND version = ? AND state = ?"
	args = append(args, d.ID, d.Version, d.State)
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	from := d.State
	d.State, d.Version = to, d.Version+1
	data := map[string]any{"message": d.MessageID}
	for k, v := range c.data {
		data[k] = v
	}
	return true, recordDelivery(ctx, tx, a, d, "msg."+kind, from, to, data)
}

func recordDelivery(ctx context.Context, tx *sql.Tx, a Actor, d *Delivery, kind, from, to string, data any) error {
	_, err := event.Record(ctx, tx, event.Event{
		Source: a.source(), Kind: kind, EntityType: "delivery", EntityID: strconv.FormatInt(d.ID, 10),
		FromStatus: from, ToStatus: to, ProjectID: d.ProjectID, TaskID: str(d.To.Task), RunID: str(d.To.Run),
		Actor: a.Name, Data: data,
	})
	return err
}

func recordMessage(ctx context.Context, tx *sql.Tx, a Actor, m *Message, kind string, data any) error {
	_, err := event.Record(ctx, tx, event.Event{
		Source: a.source(), Kind: kind, EntityType: "message", EntityID: strconv.FormatInt(m.ID, 10),
		ProjectID: m.ProjectID, TaskID: str(m.Task), RunID: str(m.From.Run), Actor: a.Name, Data: data,
	})
	return err
}

// ---------------------------------------------------------------- helpers

// clock is the time source (tests replace it).
var clock = time.Now

func ts(t time.Time) string { return t.UTC().Format(db.TimeFormat) }

func nowOr(t time.Time) time.Time {
	if t.IsZero() {
		return clock()
	}
	return t
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// NewNonce returns a fresh attempt nonce.
func NewNonce() string { return randomHex(16) }

func ptr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func iptr(i sql.NullInt64) *int64 {
	if !i.Valid {
		return nil
	}
	return &i.Int64
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// runInfo is what msg needs to know about a run.
type runInfo struct {
	ID, TaskID, Role, Status string
	ProjectID                int64
}

func loadRun(ctx context.Context, q db.Querier, id string) (*runInfo, error) {
	var r runInfo
	err := q.QueryRowContext(ctx, `SELECT id, coalesce(task_id, ''), role, status, project_id FROM runs WHERE id = ?`, id).
		Scan(&r.ID, &r.TaskID, &r.Role, &r.Status, &r.ProjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.NotFound, "run %s not found", id)
	}
	return &r, err
}

// gone reports whether a run in this status will never take a message again.
func gone(status string) bool { return status == "stopping" || status == "parked" || status == "ended" }

// goneSQL is gone(status of run <col>) in SQL.
func goneSQL(col string) string {
	return "EXISTS (SELECT 1 FROM runs g WHERE g.id = " + col + " AND g.status IN ('stopping', 'parked', 'ended'))"
}

func terminalTask(status string) bool { return task.Terminal(status) }
