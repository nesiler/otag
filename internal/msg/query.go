package msg

import (
	"context"
	"database/sql"
	"slices"

	"github.com/nesiler/otag/internal/db"
)

// AddressedTo reports whether message id has a delivery for reader in any state: one
// bound or addressed to its run, or addressed to its task (a child replies only to
// those). For the human: a human or coordinator-role delivery.
func AddressedTo(ctx context.Context, q db.Querier, r Reader, id int64) (bool, error) {
	cond := "d.recipient_kind IN ('human', 'role')"
	args := []any{id}
	if r.RunID != "" {
		cond = "(d.run_id = ? OR (d.recipient_kind = 'run' AND d.recipient = ?) OR (d.recipient_kind = 'task' AND d.recipient = ?))"
		args = append(args, r.RunID, r.RunID, r.TaskID)
	}
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM message_deliveries d WHERE d.message_id = ? AND `+cond, args...).Scan(&n)
	return n > 0, err
}

// Inbox returns what reader may read, oldest first: the unread deliveries (queued,
// dispatched, delivered, outcome_unknown), or with all the latest limit of any state.
// limit 0 = 50.
func Inbox(ctx context.Context, q db.Querier, r Reader, all bool, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 50
	}
	where, args := readable(r)
	query := " WHERE " + where
	if !all {
		query += " AND " + unreadSQL + " ORDER BY d.id LIMIT ?"
	} else {
		query += " ORDER BY d.id DESC LIMIT ?"
	}
	ds, err := deliveries(ctx, q, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	if all {
		slices.Reverse(ds)
	}
	return items(ctx, q, ds)
}

// HasOpenAsk reports whether task taskID has an open ask. P3: a closing harness
// dialog must leave such a task waiting.
func HasOpenAsk(ctx context.Context, q db.Querier, taskID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE task_id = ? AND ask = 1 AND closed_at IS NULL`,
		taskID).Scan(&n)
	return n > 0, err
}

// HasOpenAskBy reports whether task taskID has an open ask from run runID. A closing
// harness dialog of runID leaves the task waiting only for its own ask: an ask of an
// earlier run of the task that has gone does not hold the new run's task.
func HasOpenAskBy(ctx context.Context, q db.Querier, taskID, runID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM messages
		WHERE task_id = ? AND sender_run_id = ? AND ask = 1 AND closed_at IS NULL`, taskID, runID).Scan(&n)
	return n > 0, err
}

// AwaitsAnswer reports whether run runID of task taskID still waits for an answer: its
// ask is open, or its latest ask is answered but no copy of the answer has been
// delivered or read yet (queued, in flight, unknown, expired or dead: `otag needs`
// shows the last three). A closing harness dialog of runID leaves the task waiting
// then; the answer reaching the run moves it (received).
func AwaitsAnswer(ctx context.Context, q db.Querier, taskID, runID string) (bool, error) {
	if open, err := HasOpenAskBy(ctx, q, taskID, runID); err != nil || open {
		return open, err
	}
	var n int
	err := q.QueryRowContext(ctx, `
SELECT count(*) FROM messages a
WHERE a.kind = 'answer'
  AND a.reply_to_id = (SELECT max(id) FROM messages WHERE task_id = ? AND ask = 1 AND sender_run_id = ?)
  AND NOT EXISTS (SELECT 1 FROM message_deliveries d WHERE d.message_id = a.id AND d.state IN ('delivered', 'read'))`,
		taskID, runID).Scan(&n)
	return n > 0, err
}

// OpenAsk is an open `otag ask` question with its asker.
type OpenAsk struct {
	Message          *Message `json:"message"`
	Asker            string   `json:"asker"` // the run that asked
	AskerStatus      string   `json:"asker_status"`
	AskerEndedReason *string  `json:"asker_ended_reason"` // "parked" once P4 parked it
	TaskStatus       string   `json:"task_status"`
	AgeS             int64    `json:"age_s"`
}

// AskFilter selects open asks; zero fields match everything.
type AskFilter struct {
	ProjectID int64
	TaskID    string
	RunID     string // the asker
}

// OpenAsks returns the open asks (oldest first) of tasks that are not finished (done,
// delivered or cancelled). P4 parks and resumes from these (Message.Park, Asker*).
func OpenAsks(ctx context.Context, q db.Querier, f AskFilter) ([]OpenAsk, error) {
	query := `SELECT m.id, r.status, r.ended_reason, t.status,
		CAST((julianday('now') - julianday(m.created_at)) * 86400 AS INTEGER)
		FROM messages m JOIN runs r ON r.id = m.sender_run_id JOIN tasks t ON t.id = m.task_id
		WHERE m.ask = 1 AND m.closed_at IS NULL AND t.status NOT IN ('done', 'delivered', 'cancelled')`
	var args []any
	if f.ProjectID != 0 {
		query += " AND m.project_id = ?"
		args = append(args, f.ProjectID)
	}
	if f.TaskID != "" {
		query += " AND m.task_id = ?"
		args = append(args, f.TaskID)
	}
	if f.RunID != "" {
		query += " AND m.sender_run_id = ?"
		args = append(args, f.RunID)
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY m.id", args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		id    int64
		o     OpenAsk
		ended sql.NullString
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.o.AskerStatus, &r.ended, &r.o.TaskStatus, &r.o.AgeS); err != nil {
			_ = rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []OpenAsk{}
	for _, r := range rs {
		if r.o.Message, err = Get(ctx, q, r.id); err != nil {
			return nil, err
		}
		r.o.Asker, r.o.AskerEndedReason = str(r.o.Message.From.Run), ptr(r.ended)
		out = append(out, r.o)
	}
	return out, nil
}

// Needs is what waits for the human or the coordinator (`otag needs`).
type Needs struct {
	Questions  []NeedQuestion `json:"questions"`  // open questions to the human or the coordinator
	Deliveries []NeedDelivery `json:"deliveries"` // outcome_unknown or dead deliveries
	Blocked    []NeedBlocked  `json:"blocked"`    // blocked tasks
	Idle       []NeedIdle     `json:"idle"`       // idle child/review runs without an open ask
	Answers    []NeedAnswer   `json:"answers"`    // waiting tasks whose answer has not reached a run
}

// NeedAnswer is a waiting task whose ask is answered but whose answer is neither
// delivered nor read and will not get there by itself: no run of the task can take it
// (the asker ended or parked), or its delivery is expired, outcome_unknown or dead.
type NeedAnswer struct {
	Task     string   `json:"task"`
	Title    string   `json:"title"`
	Question int64    `json:"question"`
	Answer   int64    `json:"answer"`
	Delivery Delivery `json:"delivery"`
	Action   string   `json:"action"`
}

// NeedQuestion is an open question to the human or the coordinator.
type NeedQuestion struct {
	Message *Message `json:"message"`
	To      string   `json:"to"` // human | coordinator <run> | coordinator (none live: human)
	AgeS    int64    `json:"age_s"`
	Action  string   `json:"action"`
}

// NeedDelivery is a delivery whose outcome needs a person.
type NeedDelivery struct {
	Delivery Delivery `json:"delivery"`
	Message  int64    `json:"message"`
	Body     string   `json:"body"`
	Action   string   `json:"action"`
}

// NeedBlocked is a blocked task.
type NeedBlocked struct {
	Task   string `json:"task"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
	Action string `json:"action"`
}

// NeedIdle is a child or review run that is idle without an open ask.
type NeedIdle struct {
	Run       string `json:"run"`
	Task      string `json:"task"`
	Title     string `json:"title"`
	IdleSince string `json:"idle_since"`
	IdleS     int64  `json:"idle_s"`
	Action    string `json:"action"`
}

// NeedsOf collects Needs of project projectID (0 = every project).
func NeedsOf(ctx context.Context, q db.Querier, projectID int64) (*Needs, error) {
	out := &Needs{Questions: []NeedQuestion{}, Deliveries: []NeedDelivery{}, Blocked: []NeedBlocked{}, Idle: []NeedIdle{},
		Answers: []NeedAnswer{}}
	scope, args := "1 = 1", []any{}
	if projectID != 0 {
		scope, args = "m.project_id = ?", []any{projectID}
	}

	// Questions: open, with a live copy for the human or the coordinator role.
	rows, err := q.QueryContext(ctx, `
SELECT m.id, d.recipient_kind, d.run_id, CASE WHEN d.run_id IS NULL OR `+goneSQL("d.run_id")+` THEN 0 ELSE 1 END,
       CAST((julianday('now') - julianday(m.created_at)) * 86400 AS INTEGER)
FROM messages m JOIN message_deliveries d ON d.message_id = m.id
WHERE `+scope+` AND m.kind = 'question' AND m.closed_at IS NULL AND d.recipient_kind IN ('human', 'role')
  AND d.state NOT IN ('expired', 'dead')
  AND (m.task_id IS NULL OR EXISTS (SELECT 1 FROM tasks t WHERE t.id = m.task_id AND t.status NOT IN ('done', 'delivered', 'cancelled')))
ORDER BY m.id, d.id`, args...)
	if err != nil {
		return nil, err
	}
	type qrow struct {
		id        int64
		kind      string
		run       sql.NullString
		live      bool
		age       int64
		duplicate bool
	}
	var qs []qrow
	for rows.Next() {
		var r qrow
		if err := rows.Scan(&r.id, &r.kind, &r.run, &r.live, &r.age); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.duplicate = len(qs) > 0 && qs[len(qs)-1].id == r.id
		qs = append(qs, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range qs {
		if r.duplicate {
			continue
		}
		m, err := Get(ctx, q, r.id)
		if err != nil {
			return nil, err
		}
		to := "human"
		switch {
		case r.kind == ToRole && r.live:
			to = "coordinator " + r.run.String
		case r.kind == ToRole:
			to = "coordinator (none live: human)"
		}
		out.Questions = append(out.Questions, NeedQuestion{Message: m, To: to, AgeS: r.age,
			Action: "otag reply " + itoa(m.ID) + ` "<answer>"`})
	}

	// Deliveries that may or may not have arrived, or never will.
	dscope := "1 = 1"
	if projectID != 0 {
		dscope = "d.project_id = ?"
	}
	ds, err := deliveries(ctx, q, " WHERE "+dscope+" AND d.state IN ('outcome_unknown', 'dead') ORDER BY d.id DESC LIMIT 100", args...)
	if err != nil {
		return nil, err
	}
	slices.Reverse(ds)
	for _, d := range ds {
		m, err := getMessage(ctx, q, d.MessageID)
		if err != nil {
			return nil, err
		}
		out.Deliveries = append(out.Deliveries, NeedDelivery{Delivery: d, Message: m.ID, Body: oneLine(m.Body, 200),
			Action: "check the session; to send it again: otag send --resend " + itoa(d.ID)})
	}

	// Blocked tasks.
	tscope := "1 = 1"
	if projectID != 0 {
		tscope = "t.project_id = ?"
	}
	rows, err = q.QueryContext(ctx, `SELECT t.id, t.title, t.status_reason FROM tasks t
		WHERE `+tscope+` AND t.status = 'blocked' ORDER BY t.priority DESC, t.status_changed_at`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b NeedBlocked
		if err := rows.Scan(&b.Task, &b.Title, &b.Reason); err != nil {
			_ = rows.Close()
			return nil, err
		}
		b.Action = "otag task ready " + b.Task
		out.Blocked = append(out.Blocked, b)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Idle child and review runs without an open ask.
	rscope := "1 = 1"
	if projectID != 0 {
		rscope = "r.project_id = ?"
	}
	rows, err = q.QueryContext(ctx, `
SELECT r.id, coalesce(r.task_id, ''), coalesce(t.title, ''), coalesce(e.since, r.updated_at),
       CAST((julianday('now') - julianday(coalesce(e.since, r.updated_at))) * 86400 AS INTEGER)
FROM runs r
LEFT JOIN tasks t ON t.id = r.task_id
LEFT JOIN (SELECT run_id, max(created_at) AS since FROM events
           WHERE kind = 'run.status' AND to_status = 'idle' GROUP BY run_id) e ON e.run_id = r.id
WHERE `+rscope+` AND r.role IN ('child', 'review') AND r.status = 'idle'
  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.task_id = r.task_id AND m.ask = 1 AND m.closed_at IS NULL)
ORDER BY 4`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var i NeedIdle
		if err := rows.Scan(&i.Run, &i.Task, &i.Title, &i.IdleSince, &i.IdleS); err != nil {
			_ = rows.Close()
			return nil, err
		}
		i.Action = "otag send " + i.Run + ` "<instruction>"  (or otag run stop ` + i.Run + ")"
		out.Idle = append(out.Idle, i)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Waiting tasks whose answer is stuck: the latest ask is answered, the answer has
	// not reached a run, and either no run of the task can take it or it failed.
	rows, err = q.QueryContext(ctx, `
SELECT t.id, t.title, qm.id, am.id, d.id, CASE WHEN EXISTS (SELECT 1 FROM runs r WHERE r.task_id = t.id
       AND r.status NOT IN ('stopping', 'parked', 'ended')) THEN 1 ELSE 0 END
FROM tasks t
JOIN messages qm ON qm.task_id = t.id AND qm.ask = 1 AND qm.closed_reason = 'answered'
JOIN messages am ON am.reply_to_id = qm.id AND am.kind = 'answer'
JOIN message_deliveries d ON d.message_id = am.id
WHERE `+tscope+` AND t.status = 'waiting'
  AND qm.id = (SELECT max(x.id) FROM messages x WHERE x.task_id = t.id AND x.ask = 1)
  AND NOT EXISTS (SELECT 1 FROM message_deliveries y WHERE y.message_id = am.id AND y.state IN ('delivered', 'read'))
ORDER BY t.id, d.id`, args...)
	if err != nil {
		return nil, err
	}
	type arow struct {
		a    NeedAnswer
		did  int64
		live bool
	}
	var as []arow
	for rows.Next() {
		var r arow
		if err := rows.Scan(&r.a.Task, &r.a.Title, &r.a.Question, &r.a.Answer, &r.did, &r.live); err != nil {
			_ = rows.Close()
			return nil, err
		}
		as = append(as, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range as {
		d, err := GetDelivery(ctx, q, r.did)
		if err != nil {
			return nil, err
		}
		switch d.State {
		case OutcomeUnknown, Dead:
			r.a.Action = "check the session; to send it again: otag send --resend " + itoa(d.ID)
		case Expired:
			r.a.Action = "the answer expired; send it again: otag send " + r.a.Task + ` "<answer>"`
		default:
			if r.live {
				continue // a live run of the task will take it
			}
			r.a.Action = "no run of the task is live: otag run resume " + r.a.Task
		}
		r.a.Delivery = *d
		out.Answers = append(out.Answers, r.a)
	}
	return out, nil
}
