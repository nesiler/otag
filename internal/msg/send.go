package msg

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/task"
)

// Resolve turns an address into a recipient in project projectID: "coord" (or
// "coordinator"), "human", a run id or a task id.
func Resolve(ctx context.Context, q db.Querier, projectID int64, addr string) (Recipient, error) {
	addr = strings.TrimSpace(addr)
	switch addr {
	case "":
		return Recipient{}, apperr.New(apperr.Invalid, "empty recipient")
	case "coord", RoleCoordinator:
		return Recipient{Kind: ToRole, Ref: RoleCoordinator}, nil
	case "human":
		return Recipient{Kind: ToHuman, Ref: "human"}, nil
	}
	var pid int64
	err := q.QueryRowContext(ctx, `SELECT project_id FROM runs WHERE id = ?`, addr).Scan(&pid)
	if err == nil {
		if pid != projectID {
			return Recipient{}, apperr.New(apperr.NotFound, "run %s is not in this project", addr)
		}
		return Recipient{Kind: ToRun, Ref: addr}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Recipient{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, addr).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return Recipient{}, apperr.New(apperr.NotFound, "no run or task %s (want a run id, a task id, coord or human)", addr)
	}
	if err != nil {
		return Recipient{}, err
	}
	if pid != projectID {
		return Recipient{}, apperr.New(apperr.NotFound, "task %s is not in this project", addr)
	}
	return Recipient{Kind: ToTask, Ref: addr}, nil
}

// SendInput describes a new message.
type SendInput struct {
	ProjectID int64
	// ClientMsgID is the durable request identity ("req:<request-id>"); "" = random. A
	// second Send with the same id returns the first message (replayed) when it matches,
	// REQUEST_ID_REUSED otherwise.
	ClientMsgID string
	Kind        string // default KindInfo; KindAnswer only through Reply
	From        Sender
	TaskID      string // the task it is about; "" = the sender run's task
	Body        string
	To          []Recipient
	ExpiresAt   string // "" = never; queued deliveries expire after it

	replyTo   int64
	ask, park bool
}

// Send stores a message and one queued delivery per recipient. It returns the message
// and whether it already existed (same ClientMsgID). It never delivers anything.
func Send(ctx context.Context, tx *sql.Tx, in SendInput, a Actor) (*Message, bool, error) {
	if in.Kind == "" {
		in.Kind = KindInfo
	}
	if !slices.Contains([]string{KindQuestion, KindInfo, KindAnswer, KindCommand}, in.Kind) {
		return nil, false, apperr.New(apperr.Invalid, "unknown message kind %q (want question|info|command)", in.Kind)
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, false, apperr.New(apperr.Invalid, "message body is empty")
	}
	if len(in.Body) > MaxBody {
		return nil, false, apperr.New(apperr.Invalid, "message body is %d bytes; the limit is %d", len(in.Body), MaxBody)
	}
	if len(in.To) == 0 {
		return nil, false, apperr.New(apperr.Invalid, "no recipient")
	}
	if in.ClientMsgID == "" {
		in.ClientMsgID = "c:" + randomHex(12)
	}
	if m, err := byClientID(ctx, tx, in.ClientMsgID); err != nil || m != nil {
		if err != nil {
			return nil, false, err
		}
		if m.ProjectID != in.ProjectID || m.Kind != in.Kind || m.Body != in.Body || m.From.Kind != in.From.Kind ||
			str(m.From.Run) != in.From.RunID {
			return nil, false, apperr.New(apperr.RequestIDReused, "message id %s was used for a different message", in.ClientMsgID)
		}
		return m, true, nil
	}

	switch in.From.Kind {
	case SenderRun:
		r, err := loadRun(ctx, tx, in.From.RunID)
		if err != nil {
			return nil, false, err
		}
		if r.ProjectID != in.ProjectID {
			return nil, false, apperr.New(apperr.Forbidden, "run %s is not in this project", r.ID)
		}
		if in.TaskID == "" {
			in.TaskID = r.TaskID
		}
		if in.From.Actor == "" {
			in.From.Actor = "run:" + r.ID
		}
	case SenderHuman, SenderSystem:
		if in.From.RunID != "" {
			return nil, false, apperr.New(apperr.Invalid, "a %s sender has no run", in.From.Kind)
		}
	default:
		return nil, false, apperr.New(apperr.Invalid, "unknown sender kind %q", in.From.Kind)
	}
	if in.TaskID != "" {
		var pid int64
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, in.TaskID).Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) || err == nil && pid != in.ProjectID {
			return nil, false, apperr.New(apperr.NotFound, "task %s not found in this project", in.TaskID)
		}
		if err != nil {
			return nil, false, err
		}
	}

	type bound struct {
		Recipient
		run, task string
	}
	var targets []bound
	seen := map[Recipient]bool{}
	for _, r := range in.To {
		if seen[r] {
			continue
		}
		seen[r] = true
		run, tsk, err := bind(ctx, tx, in.ProjectID, r)
		if err != nil {
			return nil, false, err
		}
		if in.From.Kind == SenderRun && run == in.From.RunID && r.Kind != ToRole {
			return nil, false, apperr.New(apperr.Invalid, "run %s cannot message itself", run)
		}
		targets = append(targets, bound{r, run, tsk})
	}

	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, sender_run_id, sender_actor, task_id, reply_to_id,
                      body, ask, park, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		in.ProjectID, in.ClientMsgID, in.Kind, in.From.Kind, nullStr(in.From.RunID), in.From.Actor, nullStr(in.TaskID),
		sql.NullInt64{Int64: in.replyTo, Valid: in.replyTo != 0}, in.Body, in.ask, in.park, nullStr(in.ExpiresAt)).Scan(&id)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, false, apperr.Retryable(apperr.Conflict, "message conflicts with another one: %v", err)
		}
		return nil, false, err
	}
	m, err := getMessage(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	var to []string
	for _, t := range targets {
		to = append(to, t.Kind+":"+t.Ref)
	}
	data := map[string]any{"kind": in.Kind, "to": to, "client_msg_id": in.ClientMsgID}
	if in.replyTo != 0 {
		data["reply_to"] = in.replyTo
	}
	if in.ask {
		data["ask"] = true
		data["park"] = in.park
	}
	if err := recordMessage(ctx, tx, a, m, "msg.created", data); err != nil {
		return nil, false, err
	}
	for _, t := range targets {
		var did int64
		err := tx.QueryRowContext(ctx, `
INSERT INTO message_deliveries (message_id, project_id, recipient_kind, recipient, run_id, task_id)
VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, id, in.ProjectID, t.Kind, t.Ref, nullStr(t.run), nullStr(t.task)).Scan(&did)
		if err != nil {
			return nil, false, err
		}
		d := &Delivery{ID: did, MessageID: id, ProjectID: in.ProjectID, To: To{Run: optStr(t.run), Task: optStr(t.task)}}
		if err := recordDelivery(ctx, tx, a, d, "msg.queued", "", Queued, map[string]any{"message": id, "to": t.Kind + ":" + t.Ref}); err != nil {
			return nil, false, err
		}
	}
	m, err = Get(ctx, tx, id)
	return m, false, err
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func byClientID(ctx context.Context, q db.Querier, clientID string) (*Message, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM messages WHERE client_msg_id = ?`, clientID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Get(ctx, q, id)
}

// bind checks that r can receive a message and returns the run and task the delivery
// starts bound to: a run its run and task, a task its live run (if any), the
// coordinator role the project's live coordinator (if any), the human nothing.
func bind(ctx context.Context, q db.Querier, projectID int64, r Recipient) (run, tsk string, err error) {
	switch r.Kind {
	case ToRun:
		ri, err := loadRun(ctx, q, r.Ref)
		if err != nil {
			return "", "", err
		}
		if ri.ProjectID != projectID {
			return "", "", apperr.New(apperr.NotFound, "run %s is not in this project", r.Ref)
		}
		if gone(ri.Status) {
			// It follows its task to the next run, if the task is still open.
			open := false
			if ri.TaskID != "" {
				st, err := taskStatus(ctx, q, ri.TaskID)
				if err != nil {
					return "", "", err
				}
				open = !terminalTask(st)
			}
			if !open {
				return "", "", apperr.New(apperr.InvalidTransition, "run %s is %s and has no open task to follow", ri.ID, ri.Status)
			}
		}
		return ri.ID, ri.TaskID, nil
	case ToTask:
		st, err := taskStatus(ctx, q, r.Ref)
		if err != nil {
			return "", "", err
		}
		if terminalTask(st) {
			return "", "", apperr.New(apperr.InvalidTransition, "task %s is %s", r.Ref, st)
		}
		var live string
		err = q.QueryRowContext(ctx, `SELECT r.id FROM tasks t JOIN runs r ON r.id = t.current_run_id
			WHERE t.id = ? AND r.status NOT IN ('stopping', 'parked', 'ended')`, r.Ref).Scan(&live)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
		return live, r.Ref, nil
	case ToRole:
		if r.Ref != RoleCoordinator {
			return "", "", apperr.New(apperr.Invalid, "unknown role %q", r.Ref)
		}
		coord, err := liveCoordinator(ctx, q, projectID)
		return coord, "", err
	case ToHuman:
		return "", "", nil
	}
	return "", "", apperr.New(apperr.Invalid, "unknown recipient kind %q", r.Kind)
}

func taskStatus(ctx context.Context, q db.Querier, id string) (string, error) {
	var st string
	err := q.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", apperr.New(apperr.NotFound, "task %s not found", id)
	}
	return st, err
}

// liveCoordinator returns the project's coordinator run that can take messages, or "".
func liveCoordinator(ctx context.Context, q db.Querier, projectID int64) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM runs WHERE project_id = ? AND role = 'coordinator'
		AND status NOT IN ('stopping', 'parked', 'ended') ORDER BY queued_at DESC, id DESC LIMIT 1`, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// AskInput describes `otag ask`.
type AskInput struct {
	RunID       string // the asking run (verified by the caller)
	ClientMsgID string
	Body        string
	To          Recipient // zero = the coordinator (the human when none is live)
	Park        bool      // `otag ask --park`: P4 parks the run once it is idle
}

// Ask stores a question from a run about its task and moves the task running ->
// waiting in the same transaction. A task has at most one open ask: the same question
// again from the same run returns the open one (replayed); an open ask of an earlier
// run of the task that has gone is closed as superseded; anything else is CONFLICT.
func Ask(ctx context.Context, tx *sql.Tx, in AskInput, a Actor) (*Message, bool, error) {
	r, err := loadRun(ctx, tx, in.RunID)
	if err != nil {
		return nil, false, err
	}
	if r.TaskID == "" {
		return nil, false, apperr.New(apperr.Invalid, "run %s has no task to ask about", r.ID)
	}
	if gone(r.Status) {
		return nil, false, apperr.New(apperr.InvalidTransition, "run %s is %s", r.ID, r.Status)
	}
	if in.To == (Recipient{}) {
		in.To = Recipient{Kind: ToRole, Ref: RoleCoordinator}
	}
	if in.ClientMsgID != "" {
		if m, err := byClientID(ctx, tx, in.ClientMsgID); err != nil || m != nil {
			if err == nil && (!m.Ask || m.Body != in.Body || str(m.From.Run) != r.ID) {
				err = apperr.New(apperr.RequestIDReused, "message id %s was used for a different message", in.ClientMsgID)
			}
			return m, err == nil, err
		}
	}
	t, err := task.Get(ctx, tx, r.TaskID)
	if err != nil {
		return nil, false, err
	}
	if t.CurrentRunID != r.ID || (t.Status != task.Running && t.Status != task.Waiting) {
		return nil, false, apperr.New(apperr.InvalidTransition, "task %s is %s (run %s); a run asks only while it works on the task",
			t.ID, t.Status, t.CurrentRunID)
	}
	open, err := openAsk(ctx, tx, t.ID)
	if err != nil {
		return nil, false, err
	}
	if open != nil {
		asker := str(open.From.Run)
		switch {
		case asker == r.ID && open.Body == in.Body:
			return open, true, nil
		case asker == r.ID:
			return nil, false, apperr.New(apperr.Conflict, "question #%d of task %s is still open; end your turn and wait for its answer",
				open.ID, t.ID)
		}
		ar, err := loadRun(ctx, tx, asker)
		if err != nil {
			return nil, false, err
		}
		if !gone(ar.Status) {
			return nil, false, apperr.New(apperr.Conflict, "task %s has open question #%d from run %s", t.ID, open.ID, asker)
		}
		if err := closeQuestion(ctx, tx, open, ClosedSuperseded, a); err != nil {
			return nil, false, err
		}
	}
	m, replayed, err := Send(ctx, tx, SendInput{
		ProjectID: r.ProjectID, ClientMsgID: in.ClientMsgID, Kind: KindQuestion,
		From: Sender{Kind: SenderRun, RunID: r.ID}, TaskID: t.ID, Body: in.Body, To: []Recipient{in.To},
		ask: true, park: in.Park,
	}, a)
	if err != nil || replayed {
		return m, replayed, err
	}
	if t.Status == task.Running {
		reason := "question #" + itoa(m.ID) + ": " + oneLine(in.Body, 120)
		if _, err := task.Transition(ctx, tx, t.ID, task.Waiting, reason, 0, taskActor(a, r.ID)); err != nil {
			return nil, false, err
		}
	}
	return m, false, nil
}

// ReplyInput describes `otag reply`.
type ReplyInput struct {
	ClientMsgID string
	From        Sender
	To          int64 // the message replied to
	Body        string
	Kind        string // "" = answer to an open question, info otherwise; KindInfo for a follow-up
	ExpiresAt   string
}

// Reply answers (or follows up on) message in.To. The reply goes to its sender: a
// run that can still take messages, else that run's open task, else (a coordinator
// run) the coordinator role; a human's message to the human, the daemon's to the
// coordinator. Answering a question closes it in the same transaction and expires
// its other queued copies; the replier's own copy becomes read. The asking task goes
// back to running only when the answer reaches its run (see Prove, Settle, MarkRead).
func Reply(ctx context.Context, tx *sql.Tx, in ReplyInput, a Actor) (*Message, bool, error) {
	orig, err := Get(ctx, tx, in.To)
	if err != nil {
		return nil, false, err
	}
	if in.ClientMsgID != "" {
		if m, err := byClientID(ctx, tx, in.ClientMsgID); err != nil || m != nil {
			if err == nil && (m.ReplyTo == nil || *m.ReplyTo != orig.ID || m.Body != in.Body ||
				m.From.Kind != in.From.Kind || str(m.From.Run) != in.From.RunID) {
				err = apperr.New(apperr.RequestIDReused, "message id %s was used for a different message", in.ClientMsgID)
			}
			return m, err == nil, err
		}
	}
	kind := in.Kind
	switch {
	case kind != "" && kind != KindAnswer && kind != KindInfo:
		return nil, false, apperr.New(apperr.Invalid, "a reply is an answer or info, not %s", kind)
	case orig.Kind != KindQuestion && kind == KindAnswer:
		return nil, false, apperr.New(apperr.Invalid, "message #%d is %s, not a question", orig.ID, orig.Kind)
	case orig.Kind == KindQuestion && kind != KindInfo && orig.ClosedAt != nil:
		why := *orig.ClosedReason
		if orig.AnswerID != nil {
			why += " by #" + itoa(*orig.AnswerID)
		}
		return nil, false, apperr.New(apperr.Conflict, "question #%d is closed (%s); send a new message instead", orig.ID, why)
	case kind == "" && orig.Kind == KindQuestion:
		kind = KindAnswer
	case kind == "":
		kind = KindInfo
	}
	to, err := replyRecipient(ctx, tx, orig)
	if err != nil {
		return nil, false, err
	}
	m, replayed, err := Send(ctx, tx, SendInput{
		ProjectID: orig.ProjectID, ClientMsgID: in.ClientMsgID, Kind: kind, From: in.From, TaskID: str(orig.Task),
		Body: in.Body, To: []Recipient{to}, ExpiresAt: in.ExpiresAt, replyTo: orig.ID,
	}, a)
	if err != nil || replayed {
		return m, replayed, err
	}
	// The replier has read what it replied to.
	if in.From.Kind != SenderSystem {
		reader := Reader{ProjectID: orig.ProjectID}
		if in.From.Kind == SenderRun {
			if reader, err = ReaderFor(ctx, tx, in.From.RunID); err != nil {
				return nil, false, err
			}
		}
		if err := readOwnCopies(ctx, tx, orig, reader, a); err != nil {
			return nil, false, err
		}
	}
	if kind == KindAnswer {
		if err := closeQuestion(ctx, tx, orig, ClosedAnswered, a); err != nil {
			return nil, false, err
		}
	}
	m, err = Get(ctx, tx, m.ID)
	return m, false, err
}

func replyRecipient(ctx context.Context, q db.Querier, orig *Message) (Recipient, error) {
	switch orig.From.Kind {
	case SenderHuman:
		return Recipient{Kind: ToHuman, Ref: "human"}, nil
	case SenderSystem:
		return Recipient{Kind: ToRole, Ref: RoleCoordinator}, nil
	}
	r, err := loadRun(ctx, q, *orig.From.Run)
	if err != nil {
		return Recipient{}, err
	}
	if !gone(r.Status) {
		return Recipient{Kind: ToRun, Ref: r.ID}, nil
	}
	if r.TaskID != "" {
		st, err := taskStatus(ctx, q, r.TaskID)
		if err != nil {
			return Recipient{}, err
		}
		if !terminalTask(st) {
			return Recipient{Kind: ToTask, Ref: r.TaskID}, nil
		}
		return Recipient{}, apperr.New(apperr.InvalidTransition, "run %s has ended and task %s is %s", r.ID, r.TaskID, st)
	}
	if r.Role == RoleCoordinator {
		return Recipient{Kind: ToRole, Ref: RoleCoordinator}, nil
	}
	return Recipient{}, apperr.New(apperr.InvalidTransition, "run %s has ended", r.ID)
}

// readOwnCopies marks the deliveries of m that reader can read as read.
func readOwnCopies(ctx context.Context, tx *sql.Tx, m *Message, reader Reader, a Actor) error {
	where, args := readable(reader)
	ds, err := deliveries(ctx, tx, " WHERE d.message_id = ? AND "+where, append([]any{m.ID}, args...)...)
	if err != nil {
		return err
	}
	for i := range ds {
		if _, err := markRead(ctx, tx, &ds[i], reader, a); err != nil {
			return err
		}
	}
	return nil
}

// CloseAsks closes the open ask of task taskID (P4: the asker finished with done or
// fail: ClosedAskerDone, ClosedAskerFailed; a cancelled task: ClosedCancelled). Its
// queued deliveries expire. No open ask is a no-op.
func CloseAsks(ctx context.Context, tx *sql.Tx, taskID, reason string, a Actor) error {
	m, err := openAsk(ctx, tx, taskID)
	if err != nil || m == nil {
		return err
	}
	return closeQuestion(ctx, tx, m, reason, a)
}

// closeQuestion closes question m (CAS on closed_at) and expires its queued copies.
func closeQuestion(ctx context.Context, tx *sql.Tx, m *Message, reason string, a Actor) error {
	res, err := tx.ExecContext(ctx, `UPDATE messages SET closed_at = ?, closed_reason = ?
		WHERE id = ? AND kind = 'question' AND closed_at IS NULL`, db.Now(), reason, m.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return err // closed meanwhile
	}
	if err := recordMessage(ctx, tx, a, m, "msg.closed", map[string]any{"reason": reason}); err != nil {
		return err
	}
	ds, err := deliveries(ctx, tx, " WHERE d.message_id = ? AND d.state = 'queued'", m.ID)
	if err != nil {
		return err
	}
	for i := range ds {
		if err := expire(ctx, tx, &ds[i], Expired, "question "+reason, a); err != nil {
			return err
		}
	}
	return nil
}

// expire moves a delivery to expired or dead with a reason.
func expire(ctx context.Context, tx *sql.Tx, d *Delivery, to, why string, a Actor) error {
	var c change
	c.set("closed_at", db.Now())
	c.set("last_error", why)
	c.data = map[string]any{"reason": why}
	_, err := move(ctx, tx, a, d, to, to, c)
	return err
}

func openAsk(ctx context.Context, q db.Querier, taskID string) (*Message, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM messages WHERE task_id = ? AND ask = 1 AND closed_at IS NULL`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Get(ctx, q, id)
}

func taskActor(a Actor, runID string) task.Actor {
	return task.Actor{Name: a.Name, RunID: runID, Source: a.source()}
}
