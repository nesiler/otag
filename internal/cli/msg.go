package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
)

// newDispatcher returns the synchronous delivery attempt that send, reply and ask make
// after their commit (docs/MESSAGES.md): the run dispatcher (cli/event.go). nil
// would leave every delivery queued for the hooks, the daemon and `otag inbox`.
var newDispatcher = func(a *app) msg.Dispatcher { return a.dispatcher() }

// sendResult is the data of send, reply and ask.
type sendResult struct {
	Message  *msg.Message        `json:"message"`
	Replayed bool                `json:"replayed"`
	Attempts []msg.AttemptResult `json:"attempts"`
	// Resume is filled by P4 when an answer resumes a parked run (reply only).
	Resume any `json:"resume"`
	// Ask only.
	Park        *bool  `json:"park,omitempty"`
	Instruction string `json:"instruction,omitempty"`
}

// attempt makes the synchronous delivery attempt of each delivery of m.
func (a *app) attempt(ctx context.Context, res *sendResult, noDeliver bool) {
	res.Attempts = []msg.AttemptResult{}
	if res.Replayed || res.Message == nil {
		return
	}
	disp := newDispatcher(a)
	perRun, _ := disp.(msg.RunDispatcher)
	byRun := map[string][]msg.AttemptResult{} // AttemptRun results, once per run
	for _, d := range res.Message.Deliveries {
		r := msg.AttemptResult{Delivery: d.ID, Result: "queued"}
		switch {
		case d.To.Run == nil:
			r.Result, r.Reason = "skipped", "no live run: read with otag inbox, or taken by the next run"
		case noDeliver:
			r.Reason = "--no-deliver"
		case disp == nil:
			r.Reason = "queued for delivery"
		case perRun != nil:
			rs, ok := byRun[*d.To.Run]
			if !ok {
				rs = perRun.AttemptRun(ctx, *d.To.Run)
				byRun[*d.To.Run] = rs
			}
			r.Reason = "not taken by this attempt"
			for _, x := range rs {
				if x.Delivery == d.ID {
					r = x
				}
			}
		default:
			r = disp.Attempt(ctx, d.ID)
		}
		res.Attempts = append(res.Attempts, r)
	}
	if len(res.Attempts) > 0 {
		if m, err := msg.Get(ctx, a.db, res.Message.ID); err == nil {
			res.Message = m
		}
	}
}

func cmdSend(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("send")
	var kind, taskID, ttl string
	var noDeliver bool
	var resend int64
	fs.StringVar(&kind, "kind", msg.KindInfo, "info|command|question")
	fs.StringVar(&taskID, "task", "", "the task the message is about (default: your run's task)")
	fs.StringVar(&ttl, "ttl", "", "undelivered after this long, it expires (e.g. 2h)")
	fs.BoolVar(&noDeliver, "no-deliver", false, "only queue it; no delivery attempt now")
	fs.Int64Var(&resend, "resend", 0, "queue an outcome_unknown or dead delivery again")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if _, err := a.openDB(ctx); err != nil {
		return result{}, err
	}
	c, err := a.msgCaller(ctx, "send")
	if err != nil {
		return result{}, err
	}
	if resend != 0 {
		return sendResend(ctx, a, c, pos, resend)
	}
	if len(pos) < 2 {
		return result{}, usageErr(`usage: otag send <to>[,<to>...] "text" [--kind info|command|question] [--task T] [--ttl 2h]` +
			"\n  <to>: a run id, a task id, coord or human")
	}
	pid, err := c.requireProject()
	if err != nil {
		return result{}, err
	}
	if kind == msg.KindAnswer {
		return result{}, usageErr("answers go through otag reply <msg-id>")
	}
	var expires string
	if ttl != "" {
		dur, err := time.ParseDuration(ttl)
		if err != nil || dur <= 0 {
			return result{}, usageErr("--ttl wants a positive duration such as 30m or 2h")
		}
		expires = time.Now().Add(dur).UTC().Format(db.TimeFormat)
	}
	addrs := strings.Split(pos[0], ",")
	if c.child() {
		for _, to := range addrs {
			if t := strings.TrimSpace(to); t != "coord" && t != msg.RoleCoordinator {
				return result{}, apperr.New(apperr.Forbidden, "inside run %s, otag send goes only to coord (got %s); "+
					"to answer a message use otag reply <msg-id>", c.run.ID, t)
			}
		}
		if taskID != "" && taskID != c.run.TaskID {
			return result{}, apperr.New(apperr.Forbidden, "inside run %s, messages are about your own task", c.run.ID)
		}
	}
	body := strings.Join(pos[1:], " ")
	res, err := mutate(ctx, a, requestKey("send", c.id, fs, pos), func(tx *sql.Tx) (sendResult, error) {
		var to []msg.Recipient
		for _, addr := range addrs {
			r, err := msg.Resolve(ctx, tx, pid, addr)
			if err != nil {
				return sendResult{}, err
			}
			to = append(to, r)
		}
		m, replayed, err := msg.Send(ctx, tx, msg.SendInput{
			ProjectID: pid, ClientMsgID: clientMsgID(a), Kind: kind, From: c.sender, TaskID: taskID,
			Body: body, To: to, ExpiresAt: expires,
		}, c.actor)
		return sendResult{Message: m, Replayed: replayed}, err
	})
	if err != nil {
		return result{}, err
	}
	res.Replayed = res.Replayed || a.replayed
	a.attempt(ctx, &res, noDeliver)
	return result{data: res, human: func(w io.Writer) { printSent(w, res) }}, nil
}

func sendResend(ctx context.Context, a *app, c *caller, pos []string, id int64) (result, error) {
	if len(pos) > 0 {
		return result{}, usageErr("usage: otag send --resend <delivery-id>")
	}
	if c.child() {
		return result{}, apperr.New(apperr.Forbidden, "inside run %s, deliveries are re-sent by the coordinator or a person", c.run.ID)
	}
	d, err := mutate(ctx, a, opKey{Command: "send --resend", Project: projectName(c.id), Args: []string{strconv.FormatInt(id, 10)}},
		func(tx *sql.Tx) (*msg.Delivery, error) {
			if pid := c.projectID(); pid != 0 {
				if d, err := msg.GetDelivery(ctx, tx, id); err == nil && d.ProjectID != pid {
					return nil, apperr.New(apperr.NotFound, "delivery %d is not in this project", id)
				}
			}
			return msg.Requeue(ctx, tx, id, c.actor)
		})
	if err != nil {
		return result{}, err
	}
	return result{data: map[string]any{"delivery": d}, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "delivery %d of message #%d is queued again\n", d.ID, d.MessageID)
	}}, nil
}

// clientMsgID makes --request-id the durable identity of the message, so a replay
// finds it even after request_log was pruned.
func clientMsgID(a *app) string {
	if a.requestID == "" {
		return ""
	}
	return "req:" + a.requestID
}

func printSent(w io.Writer, res sendResult) {
	m := res.Message
	for i, d := range m.Deliveries {
		to := d.To.Recipient
		if d.To.Run != nil && *d.To.Run != to {
			to += " (run " + *d.To.Run + ")"
		}
		outcome := d.State
		if i < len(res.Attempts) {
			outcome = res.Attempts[i].Result
			if r := res.Attempts[i].Reason; r != "" {
				outcome += ": " + r
			}
		}
		_, _ = fmt.Fprintf(w, "%s #%d → %s: %s\n", m.Kind, m.ID, to, outcome)
	}
	if res.Replayed {
		_, _ = fmt.Fprintln(w, "(already sent; nothing new)")
	}
}

func cmdReply(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("reply")
	var info, noDeliver bool
	fs.BoolVar(&info, "info", false, "a follow-up, not the answer (for a question that is already answered)")
	fs.BoolVar(&noDeliver, "no-deliver", false, "only queue it; no delivery attempt now")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) < 2 {
		return result{}, usageErr(`usage: otag reply <msg-id> "text" [--info]`)
	}
	to, err := strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64)
	if err != nil || to <= 0 {
		return result{}, usageErr("otag reply: %q is not a message id", pos[0])
	}
	if _, err := a.openDB(ctx); err != nil {
		return result{}, err
	}
	c, err := a.msgCaller(ctx, "reply")
	if err != nil {
		return result{}, err
	}
	orig, err := msg.Get(ctx, a.db, to)
	if err != nil {
		return result{}, err
	}
	switch {
	case c.run != nil && orig.ProjectID != c.run.ProjectID:
		return result{}, apperr.New(apperr.NotFound, "message %d not found in this project", to)
	case c.child():
		ok, err := msg.AddressedTo(ctx, a.db, c.reader, to)
		if err != nil {
			return result{}, err
		}
		if !ok {
			return result{}, apperr.New(apperr.Forbidden, "inside run %s, otag reply answers only messages sent to this run "+
				"or its task; message #%d was not", c.run.ID, to)
		}
	}
	kind := ""
	if info {
		kind = msg.KindInfo
	}
	body := strings.Join(pos[1:], " ")
	res, err := mutate(ctx, a, requestKey("reply", c.id, fs, pos), func(tx *sql.Tx) (sendResult, error) {
		m, replayed, err := msg.Reply(ctx, tx, msg.ReplyInput{
			ClientMsgID: clientMsgID(a), From: c.sender, To: to, Body: body, Kind: kind,
		}, c.actor)
		return sendResult{Message: m, Replayed: replayed}, err
	})
	if err != nil {
		return result{}, err
	}
	res.Replayed = res.Replayed || a.replayed
	a.attempt(ctx, &res, noDeliver)
	return result{data: res, human: func(w io.Writer) { printSent(w, res) }}, nil
}

func cmdAsk(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("ask")
	var park bool
	var to string
	fs.BoolVar(&park, "park", false, "close this session while waiting; otag resumes it with the answer")
	fs.StringVar(&to, "to", "coord", "coord (the human when no coordinator runs) or human")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) == 0 {
		return result{}, usageErr(`usage: otag ask "question" [--park] [--to coord|human]`)
	}
	if _, err := a.openDB(ctx); err != nil {
		return result{}, err
	}
	c, err := a.msgCaller(ctx, "ask")
	if err != nil {
		return result{}, err
	}
	switch {
	case c.run == nil:
		return result{}, usageErr("otag ask runs inside a otag run; outside one, use otag send")
	case !c.child():
		return result{}, usageErr("a coordinator asks the human with otag send human \"question\"")
	}
	var recipient msg.Recipient
	switch to {
	case "coord", msg.RoleCoordinator:
		recipient = msg.Recipient{Kind: msg.ToRole, Ref: msg.RoleCoordinator}
	case "human":
		recipient = msg.Recipient{Kind: msg.ToHuman, Ref: "human"}
	default:
		return result{}, usageErr("--to wants coord or human")
	}
	body := strings.Join(pos, " ")
	res, err := mutate(ctx, a, requestKey("ask", c.id, fs, pos), func(tx *sql.Tx) (sendResult, error) {
		m, replayed, err := msg.Ask(ctx, tx, msg.AskInput{
			RunID: c.run.ID, ClientMsgID: clientMsgID(a), Body: body, To: recipient, Park: park,
		}, c.actor)
		return sendResult{Message: m, Replayed: replayed}, err
	})
	if err != nil {
		return result{}, err
	}
	res.Replayed = res.Replayed || a.replayed
	parked := res.Message.Park
	res.Park = &parked
	res.Instruction = fmt.Sprintf("Question #%d is sent; task %s waits for the answer. End your turn now: "+
		"the answer arrives as your next message (or run `otag inbox --wait`).", res.Message.ID, c.run.TaskID)
	if parked {
		res.Instruction = fmt.Sprintf("Question #%d is sent; task %s waits for the answer. End your turn now: "+
			"otag closes this session and resumes it with the answer.", res.Message.ID, c.run.TaskID)
	}
	a.attempt(ctx, &res, false)
	return result{data: res, human: func(w io.Writer) { _, _ = fmt.Fprintln(w, res.Instruction) }}, nil
}

// inboxResult is the data of otag inbox.
type inboxResult struct {
	Messages []msg.Item `json:"messages"`
	TimedOut bool       `json:"timed_out"`
}

func cmdInbox(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("inbox")
	var wait, peek, all bool
	var timeout time.Duration
	var limit int
	fs.BoolVar(&wait, "wait", false, "wait until a message arrives")
	fs.DurationVar(&timeout, "timeout", 10*time.Minute, "with --wait: give up after this long")
	fs.BoolVar(&peek, "peek", false, "show without marking read")
	fs.BoolVar(&all, "all", false, "also messages already read (implies --peek)")
	fs.IntVar(&limit, "limit", 50, "max messages")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) > 0 {
		return result{}, usageErr("usage: otag inbox [--wait [--timeout 10m]] [--peek] [--all] [--limit N]")
	}
	if _, err := a.openDB(ctx); err != nil {
		return result{}, err
	}
	c, err := a.msgCaller(ctx, "inbox")
	if err != nil {
		return result{}, err
	}
	var res inboxResult
	deadline := time.Now().Add(timeout)
	for {
		if res.Messages, err = msg.Inbox(ctx, a.db, c.reader, all, limit); err != nil {
			return result{}, err
		}
		if len(res.Messages) > 0 && !peek && !all {
			// Select and mark read in one transaction: only what was taken is shown (a
			// dispatcher may have claimed an entry since the look above).
			err := a.db.Tx(ctx, func(tx *sql.Tx) (err error) {
				res.Messages, err = msg.Take(ctx, tx, c.reader, limit, c.actor)
				return err
			})
			if err != nil {
				return result{}, err
			}
		}
		if len(res.Messages) > 0 || !wait || all {
			break
		}
		if !time.Now().Before(deadline) {
			res.TimedOut = true
			break
		}
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if res.Messages == nil {
		res.Messages = []msg.Item{}
	}
	return result{data: res, human: func(w io.Writer) { printInbox(w, res) }}, nil
}

func printInbox(w io.Writer, res inboxResult) {
	if len(res.Messages) == 0 {
		if res.TimedOut {
			_, _ = fmt.Fprintln(w, "no messages (timed out)")
		} else {
			_, _ = fmt.Fprintln(w, "no messages")
		}
		return
	}
	for i, it := range res.Messages {
		if i > 0 {
			_, _ = fmt.Fprintln(w, "---")
		}
		_, _ = fmt.Fprintf(w, "%s (%s, %s):\n%s\n", msg.Headline(it.Message), it.CreatedAt, it.Delivery.State, it.Body)
		if it.Kind == msg.KindQuestion && it.ClosedAt == nil {
			_, _ = fmt.Fprintf(w, "reply with: otag reply %d \"<answer>\"\n", it.ID)
		}
	}
}

func cmdNeeds(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("needs")
	var all bool
	fs.BoolVar(&all, "all", false, "every project")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) > 0 {
		return result{}, usageErr("usage: otag needs [--all]")
	}
	if _, err := a.openDB(ctx); err != nil {
		return result{}, err
	}
	c, err := a.msgCaller(ctx, "needs")
	if err != nil {
		return result{}, err
	}
	if c.child() {
		return result{}, apperr.New(apperr.Forbidden, "inside run %s, otag needs is for the coordinator", c.run.ID)
	}
	pid := c.projectID()
	if all {
		pid = 0
	}
	n, err := msg.NeedsOf(ctx, a.db, pid)
	if err != nil {
		return result{}, err
	}
	return result{data: n, human: func(w io.Writer) { printNeeds(w, n) }}, nil
}

func printNeeds(w io.Writer, n *msg.Needs) {
	if len(n.Questions)+len(n.Deliveries)+len(n.Blocked)+len(n.Idle)+len(n.Answers) == 0 {
		_, _ = fmt.Fprintln(w, "nothing needs you")
		return
	}
	section := func(title string, count int) bool {
		if count == 0 {
			return false
		}
		_, _ = fmt.Fprintf(w, "%s (%d):\n", title, count)
		return true
	}
	if section("questions", len(n.Questions)) {
		for _, q := range n.Questions {
			_, _ = fmt.Fprintf(w, "  #%d from %s to %s, %s ago: %s\n    → %s\n", q.Message.ID, from(q.Message), q.To,
				time.Duration(q.AgeS)*time.Second, oneLine(q.Message.Body), q.Action)
		}
	}
	if section("deliveries", len(n.Deliveries)) {
		for _, d := range n.Deliveries {
			_, _ = fmt.Fprintf(w, "  delivery %d of #%d to %s: %s: %s\n    → %s\n", d.Delivery.ID, d.Message, d.Delivery.To.Recipient,
				d.Delivery.State, d.Body, d.Action)
		}
	}
	if section("blocked tasks", len(n.Blocked)) {
		for _, b := range n.Blocked {
			_, _ = fmt.Fprintf(w, "  %s %s: %s\n    → %s\n", b.Task, b.Title, b.Reason, b.Action)
		}
	}
	if section("idle runs", len(n.Idle)) {
		for _, i := range n.Idle {
			_, _ = fmt.Fprintf(w, "  %s (task %s %s) idle %s\n    → %s\n", i.Run, i.Task, i.Title, time.Duration(i.IdleS)*time.Second, i.Action)
		}
	}
	if section("answers not received", len(n.Answers)) {
		for _, x := range n.Answers {
			_, _ = fmt.Fprintf(w, "  task %s %s: answer #%d to #%d is %s\n    → %s\n", x.Task, x.Title, x.Answer, x.Question,
				x.Delivery.State, x.Action)
		}
	}
}

func from(m *msg.Message) string {
	if m.From.Run != nil {
		return *m.From.Run
	}
	return m.From.Kind
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return s
}
