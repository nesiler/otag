package msg

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/task"
)

var (
	ctx   = context.Background()
	human = Sender{Kind: SenderHuman, Actor: "user:test"}
	actor = Actor{Name: "user:test"}
)

// setup: project 1 with task p-1 running in idle run p-1-r1, ready task p-2, closed
// task p-3, live coordinator p-coord-r1 and project 2 with task q-1.
func setup(t *testing.T) (*db.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "otag.db")
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exec(t, d, `
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'f', 'fake', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'p', 1, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (1, 1, 1, 'fake', 'h', '{}');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'p', '/p', 'p', 'h'), (2, 'q', '/q', 'q', 'h');
INSERT INTO tasks (id, project_id, title, status) VALUES ('p-1', 1, 'one', 'running'), ('p-2', 1, 'two', 'ready'),
	('p-3', 1, 'three', 'done'), ('q-1', 2, 'other', 'ready');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status, ended_reason)
VALUES ('p-1-r1', 1, 'p-1', 'child', 1, 1, 1, 'fake', 'idle', NULL),
       ('p-coord-r1', 1, NULL, 'coordinator', 1, 1, 1, 'fake', 'idle', NULL),
       ('p-3-r1', 1, 'p-3', 'child', 1, 1, 1, 'fake', 'ended', 'done');
UPDATE tasks SET current_run_id = 'p-1-r1' WHERE id = 'p-1';`)
	return d, path
}

func exec(t *testing.T, d *db.DB, q string, args ...any) {
	t.Helper()
	if err := d.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func inTx[T any](t *testing.T, d *db.DB, fn func(*sql.Tx) (T, error)) (T, error) {
	t.Helper()
	var out T
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

func must[T any](t *testing.T, d *db.DB, fn func(*sql.Tx) (T, error)) T {
	t.Helper()
	out, err := inTx(t, d, fn)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func send(t *testing.T, d *db.DB, in SendInput) *Message {
	t.Helper()
	if in.ProjectID == 0 {
		in.ProjectID = 1
	}
	if in.From.Kind == "" {
		in.From = human
	}
	return must(t, d, func(tx *sql.Tx) (*Message, error) {
		m, _, err := Send(ctx, tx, in, actor)
		return m, err
	})
}

func to(kind, ref string) []Recipient { return []Recipient{{Kind: kind, Ref: ref}} }

func state(t *testing.T, d *db.DB, id int64) string {
	t.Helper()
	dl, err := GetDelivery(ctx, d, id)
	if err != nil {
		t.Fatal(err)
	}
	return dl.State
}

func taskOf(t *testing.T, d *db.DB, id string) string {
	t.Helper()
	tk, err := task.Get(ctx, d, id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status
}

func claim(t *testing.T, d *db.DB, run, channel string) *Attempt {
	t.Helper()
	return must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: run, Channel: channel}, actor)
	})
}

func settle(t *testing.T, d *db.DB, in SettleInput) SettleResult {
	t.Helper()
	return must(t, d, func(tx *sql.Tx) (SettleResult, error) { return Settle(ctx, tx, in, actor) })
}

func prove(t *testing.T, d *db.DB, in ProveInput) []int64 {
	t.Helper()
	return must(t, d, func(tx *sql.Tx) ([]int64, error) { return Prove(ctx, tx, in, actor) })
}

func TestResolve(t *testing.T) {
	d, _ := setup(t)
	cases := map[string]Recipient{
		"coord": {ToRole, RoleCoordinator}, "coordinator": {ToRole, RoleCoordinator}, "human": {ToHuman, "human"},
		"p-1-r1": {ToRun, "p-1-r1"}, "p-2": {ToTask, "p-2"},
	}
	for addr, want := range cases {
		got, err := Resolve(ctx, d, 1, addr)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", addr, got, err)
		}
	}
	for _, addr := range []string{"nope", "q-1", ""} {
		if _, err := Resolve(ctx, d, 1, addr); err == nil {
			t.Errorf("%q resolved", addr)
		}
	}
}

func TestSendBindsAndIsIdempotent(t *testing.T) {
	d, _ := setup(t)
	in := SendInput{ClientMsgID: "req:1", Kind: KindCommand, Body: "check the tests",
		To: []Recipient{{ToRun, "p-1-r1"}, {ToTask, "p-2"}, {ToRole, RoleCoordinator}, {ToHuman, "human"}, {ToTask, "p-2"}}}
	m := send(t, d, in)
	if len(m.Deliveries) != 4 || m.Kind != KindCommand || m.From.Kind != SenderHuman || m.ClientMsgID != "req:1" {
		t.Fatalf("message %+v", m)
	}
	want := []struct{ kind, run, task string }{
		{ToRun, "p-1-r1", "p-1"}, {ToTask, "", "p-2"}, {ToRole, "p-coord-r1", ""}, {ToHuman, "", ""},
	}
	for i, w := range want {
		dl := m.Deliveries[i]
		if dl.To.Kind != w.kind || str(dl.To.Run) != w.run || str(dl.To.Task) != w.task || dl.State != Queued {
			t.Errorf("delivery %d: %+v", i, dl)
		}
	}
	again, replayed, err := inTx2(t, d, func(tx *sql.Tx) (*Message, bool, error) { return Send(ctx, tx, withDefaults(in), actor) })
	if err != nil || !replayed || again.ID != m.ID {
		t.Fatalf("replay: %v %v", replayed, err)
	}
	in.Body = "something else"
	if _, _, err := inTx2(t, d, func(tx *sql.Tx) (*Message, bool, error) { return Send(ctx, tx, withDefaults(in), actor) }); apperr.Code(err) != apperr.RequestIDReused {
		t.Fatalf("reused id: %v", err)
	}
	var created, queued int
	_ = d.QueryRow(`SELECT count(*) FILTER (WHERE kind = 'msg.created'), count(*) FILTER (WHERE kind = 'msg.queued' AND to_status = 'queued')
		FROM events WHERE kind LIKE 'msg.%'`).Scan(&created, &queued)
	if created != 1 || queued != 4 {
		t.Fatalf("events: %d created, %d queued", created, queued)
	}

	bad := []SendInput{
		{Body: "x", To: to(ToTask, "p-3")},                               // closed task
		{Body: "x", To: to(ToRun, "p-3-r1")},                             // ended run, closed task
		{Body: " ", To: to(ToHuman, "human")},                            // empty
		{Body: strings.Repeat("x", MaxBody+1), To: to(ToHuman, "human")}, // too long
		{Body: "x"}, // nobody
		{Body: "x", Kind: "shout", To: to(ToHuman, "human")},                                              // kind
		{Body: "x", To: to(ToTask, "p-2"), From: Sender{Kind: SenderRun, RunID: "p-1-r1"}, TaskID: "q-1"}, // other project's task
		{Body: "x", To: to(ToRun, "p-1-r1"), From: Sender{Kind: SenderRun, RunID: "p-1-r1"}},              // itself
	}
	for i, b := range bad {
		if _, _, err := inTx2(t, d, func(tx *sql.Tx) (*Message, bool, error) { return Send(ctx, tx, withDefaults(b), actor) }); err == nil {
			t.Errorf("bad send %d accepted", i)
		}
	}
	// A run that ended with its task still open: the message follows the task.
	exec(t, d, `UPDATE runs SET status = 'ended', ended_reason = 'crashed' WHERE id = 'p-1-r1'`)
	m = send(t, d, SendInput{Body: "x", To: to(ToRun, "p-1-r1")})
	if dl := m.Deliveries[0]; str(dl.To.Run) != "p-1-r1" || str(dl.To.Task) != "p-1" {
		t.Fatalf("ended run: %+v", dl)
	}
}

func withDefaults(in SendInput) SendInput {
	if in.ProjectID == 0 {
		in.ProjectID = 1
	}
	if in.From.Kind == "" {
		in.From = human
	}
	return in
}

func inTx2[A, B any](t *testing.T, d *db.DB, fn func(*sql.Tx) (A, B, error)) (A, B, error) {
	t.Helper()
	var a A
	var b B
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		a, b, err = fn(tx)
		return err
	})
	return a, b, err
}

func ask(t *testing.T, d *db.DB, in AskInput) (*Message, bool, error) {
	t.Helper()
	if in.RunID == "" {
		in.RunID = "p-1-r1"
	}
	return inTx2(t, d, func(tx *sql.Tx) (*Message, bool, error) { return Ask(ctx, tx, in, actor) })
}

func reply(t *testing.T, d *db.DB, in ReplyInput) (*Message, error) {
	t.Helper()
	if in.From.Kind == "" {
		in.From = human
	}
	m, _, err := inTx2(t, d, func(tx *sql.Tx) (*Message, bool, error) { return Reply(ctx, tx, in, actor) })
	return m, err
}

// The whole ask round trip: the task waits until the answer reaches the run, not
// when the answer is written.
func TestAskAnswerRoundTrip(t *testing.T) {
	d, _ := setup(t)
	q, replayed, err := ask(t, d, AskInput{Body: "Which database?"})
	if err != nil || replayed {
		t.Fatal(err, replayed)
	}
	tk, _ := task.Get(ctx, d, "p-1")
	if tk.Status != task.Waiting || tk.StatusReason != "question #"+itoa(q.ID)+": Which database?" {
		t.Fatalf("task after ask: %s %q", tk.Status, tk.StatusReason)
	}
	if !q.Ask || q.Kind != KindQuestion || str(q.Task) != "p-1" || str(q.Deliveries[0].To.Run) != "p-coord-r1" {
		t.Fatalf("question %+v", q)
	}
	// The same question again is the open one; another one is refused.
	if again, replayed, err := ask(t, d, AskInput{Body: "Which database?"}); err != nil || !replayed || again.ID != q.ID {
		t.Fatalf("repeat: %v %v", replayed, err)
	}
	if _, _, err := ask(t, d, AskInput{Body: "Another?"}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("second ask: %v", err)
	}
	if ok, _ := HasOpenAsk(ctx, d, "p-1"); !ok {
		t.Fatal("no open ask")
	}

	// The coordinator answers: the question closes, its copy is read, the task waits on.
	coord := Sender{Kind: SenderRun, RunID: "p-coord-r1"}
	ans, err := reply(t, d, ReplyInput{From: coord, To: q.ID, Body: "SQLite."})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Kind != KindAnswer || *ans.ReplyTo != q.ID || ans.Deliveries[0].To.Kind != ToRun || str(ans.Deliveries[0].To.Run) != "p-1-r1" {
		t.Fatalf("answer %+v", ans)
	}
	q, _ = Get(ctx, d, q.ID)
	if str(q.ClosedReason) != ClosedAnswered || *q.AnswerID != ans.ID || q.Deliveries[0].State != Read {
		t.Fatalf("question after answer: %+v", q)
	}
	if taskOf(t, d, "p-1") != task.Waiting {
		t.Fatal("task left waiting before the answer arrived")
	}
	if _, err := reply(t, d, ReplyInput{To: q.ID, Body: "Postgres."}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("second answer: %v", err)
	}

	// Delivered by an injected attempt; the receipt alone proves nothing.
	at := claim(t, d, "p-1-r1", adapter.ChanModSubmit)
	if at == nil || len(at.Items) != 1 || at.Items[0].Delivery.ID != ans.Deliveries[0].ID {
		t.Fatalf("attempt %+v", at)
	}
	if !strings.HasPrefix(at.Text(0), Marker(ans.Deliveries[0].ID)+" answer #") {
		t.Fatalf("text %q", at.Text(0))
	}
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Sent})
	if taskOf(t, d, "p-1") != task.Waiting || state(t, d, ans.Deliveries[0].ID) != Dispatched {
		t.Fatal("sent is not delivered")
	}
	payload := []byte(`{"prompt":"` + at.Text(0) + `"}`)
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Markers: ScanMarkers(payload), EventID: 0}); len(got) != 1 {
		t.Fatalf("proven %v", got)
	}
	if taskOf(t, d, "p-1") != task.Running || state(t, d, ans.Deliveries[0].ID) != Delivered {
		t.Fatal("answer delivered but task not running")
	}
	// A new question may be asked now.
	if _, _, err := ask(t, d, AskInput{Body: "Another?"}); err != nil {
		t.Fatal(err)
	}
}

// The asker parks (its run ends): the answer goes to the task and reaches the
// task's next run; the waiting task is claimed for it.
func TestAnswerFollowsTheTask(t *testing.T) {
	d, _ := setup(t)
	q, _, err := ask(t, d, AskInput{Body: "Go on?", Park: true})
	if err != nil || !q.Park {
		t.Fatal(err)
	}
	exec(t, d, `UPDATE runs SET status = 'ended', ended_reason = 'parked' WHERE id = 'p-1-r1'`)
	asks, err := OpenAsks(ctx, d, AskFilter{ProjectID: 1})
	if err != nil || len(asks) != 1 || asks[0].Asker != "p-1-r1" || str(asks[0].AskerEndedReason) != "parked" || asks[0].TaskStatus != task.Waiting {
		t.Fatalf("open asks %+v %v", asks, err)
	}
	ans, err := reply(t, d, ReplyInput{To: q.ID, Body: "Yes."})
	if err != nil {
		t.Fatal(err)
	}
	dl := ans.Deliveries[0]
	if dl.To.Kind != ToTask || dl.To.Recipient != "p-1" || dl.To.Run != nil {
		t.Fatalf("answer to a parked run: %+v", dl)
	}
	// P4's resume: claim the waiting task, create the next run, launch it with the answer.
	must(t, d, func(tx *sql.Tx) (*task.Task, error) {
		return task.Transition(ctx, tx, "p-1", task.Claimed, "", 0, task.Actor{Name: "daemon"})
	})
	exec(t, d, `INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
		VALUES ('p-1-r2', 1, 'p-1', 'child', 1, 1, 1, 'fake', 'queued'); UPDATE tasks SET current_run_id = 'p-1-r2' WHERE id = 'p-1'`)
	if at := claim(t, d, "p-1-r2", adapter.ChanModSubmit); at != nil {
		t.Fatal("injected into a queued run")
	}
	at := claim(t, d, "p-1-r2", adapter.ChanLaunchPrompt)
	if at == nil || len(at.Items) != 1 || str(at.Items[0].Delivery.To.Run) != "p-1-r2" {
		t.Fatalf("launch attempt %+v", at)
	}
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Sent})
	if got := prove(t, d, ProveInput{RunID: "p-1-r2", Markers: []int64{dl.ID}}); len(got) != 1 {
		t.Fatalf("launch proof %v", got)
	}
}

// One injected attempt in flight per run; hook channels are not serialized; runs
// in a dialog or gone take nothing injected.
func TestClaimSerializesInjectedAttempts(t *testing.T) {
	d, _ := setup(t)
	a := send(t, d, SendInput{Body: "first", To: to(ToRun, "p-1-r1")})
	b := send(t, d, SendInput{Body: "second", To: to(ToTask, "p-1")})
	at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanAPIStart, Max: 1}, actor)
	})
	if at == nil || len(at.Items) != 1 || at.Items[0].Delivery.ID != a.Deliveries[0].ID {
		t.Fatalf("first attempt %+v", at)
	}
	if again := claim(t, d, "p-1-r1", adapter.ChanAPISteer); again != nil {
		t.Fatalf("second injected attempt while one is in flight: %+v", again)
	}
	hook := claim(t, d, "p-1-r1", adapter.ChanToolContext)
	if hook == nil || len(hook.Items) != 1 || hook.Items[0].Delivery.ID != b.Deliveries[0].ID || hook.Nonce == at.Nonce {
		t.Fatalf("hook attempt %+v", hook)
	}
	settle(t, d, SettleInput{Nonce: hook.Nonce, Outcome: ChannelProved})
	if state(t, d, b.Deliveries[0].ID) != Delivered {
		t.Fatal("hook output did not prove")
	}
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Unknown, Error: "timeout after write"})
	c := send(t, d, SendInput{Body: "third", To: to(ToRun, "p-1-r1")})
	exec(t, d, `UPDATE runs SET status = 'waiting' WHERE id = 'p-1-r1'`)
	if got := claim(t, d, "p-1-r1", adapter.ChanModSubmit); got != nil {
		t.Fatal("injected into a run in a dialog")
	}
	exec(t, d, `UPDATE runs SET status = 'running' WHERE id = 'p-1-r1'`)
	if got := claim(t, d, "p-1-r1", adapter.ChanModSubmit); got == nil || got.Items[0].Delivery.ID != c.Deliveries[0].ID {
		t.Fatalf("after the unknown outcome: %+v", got)
	}
	if _, err := inTx(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanCLI}, actor)
	}); apperr.Code(err) != apperr.Invalid {
		t.Fatalf("claim on cli: %v", err)
	}
	exec(t, d, `UPDATE runs SET status = 'ended', ended_reason = 'done' WHERE id = 'p-1-r1'`)
	send(t, d, SendInput{Body: "late", To: to(ToTask, "p-1")})
	if got := claim(t, d, "p-1-r1", adapter.ChanToolContext); got != nil {
		t.Fatal("claimed for an ended run")
	}
}

func TestClaimBudget(t *testing.T) {
	d, _ := setup(t)
	for range 3 {
		send(t, d, SendInput{Body: strings.Repeat("x", 400), To: to(ToRun, "p-1-r1")})
	}
	at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanToolContext, MaxChars: 900}, actor)
	})
	if len(at.Items) != 2 {
		t.Fatalf("budget took %d", len(at.Items))
	}
	big := send(t, d, SendInput{Body: strings.Repeat("y", 5000), To: to(ToRun, "p-1-r1")})
	_ = big
	at = must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanToolContext, MaxChars: 300}, actor)
	})
	if len(at.Items) != 1 { // the oldest always goes, cut by Render
		t.Fatalf("over budget took %d", len(at.Items))
	}
	if text := at.Text(300); len(text) > 300 || !strings.HasPrefix(text, Marker(at.Items[0].Delivery.ID)) {
		t.Fatalf("cut text %d %q", len(text), text[:40])
	}
}

func TestSettleOutcomes(t *testing.T) {
	d, _ := setup(t)
	defer func() { clock = time.Now }()
	m := send(t, d, SendInput{Body: "hello", To: to(ToRun, "p-1-r1")})
	id := m.Deliveries[0].ID

	// Not sent: back in the queue, not due before the backoff.
	at := claim(t, d, "p-1-r1", adapter.ChanAPIStart)
	res := settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: NotSent, Error: "connection refused"})
	if len(res.Changed) != 1 || state(t, d, id) != Queued {
		t.Fatalf("not sent: %+v %s", res, state(t, d, id))
	}
	if got := claim(t, d, "p-1-r1", adapter.ChanAPIStart); got != nil {
		t.Fatal("claimed before the backoff")
	}
	clock = func() time.Time { return time.Now().Add(time.Hour) }
	at2 := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanAPIStart, Now: clock()}, actor)
	})
	if at2 == nil || at2.Nonce == at.Nonce || at2.Items[0].Delivery.Attempts != 2 {
		t.Fatalf("retry %+v", at2)
	}
	// The old nonce settles nothing.
	if res := settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Unknown}); !res.Stale {
		t.Fatalf("stale nonce %+v", res)
	}
	// Proof before the receipt: the marker proves it; the late receipt is still stored.
	prove(t, d, ProveInput{RunID: "p-1-r1", Markers: []int64{id}})
	if res := settle(t, d, SettleInput{Nonce: at2.Nonce, Outcome: Sent, Receipt: adapter.Receipt{Ref: "turn-1"}}); res.Stale || len(res.Changed) != 0 {
		t.Fatalf("late receipt %+v", res)
	}
	dl, _ := GetDelivery(ctx, d, id)
	if dl.State != Delivered || str(dl.Receipt) != "turn-1" || str(dl.Proof) != ProofMarker {
		t.Fatalf("delivered %+v", dl)
	}

	// A receipt proof (turn/started) that arrives before Settle stores the receipt.
	m2 := send(t, d, SendInput{Body: "two", To: to(ToRun, "p-1-r1")})
	at3 := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanAPIStart, Now: clock()}, actor)
	})
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Refs: []string{"turn-2"}}); len(got) != 0 {
		t.Fatalf("early proof matched %v", got)
	}
	res = settle(t, d, SettleInput{Nonce: at3.Nonce, Outcome: Sent, Receipt: adapter.Receipt{Ref: "turn-2", Channel: adapter.ChanAPISteer}})
	dl, _ = GetDelivery(ctx, d, m2.Deliveries[0].ID)
	if len(res.Changed) != 1 || dl.State != Delivered || str(dl.Proof) != ProofReceipt || str(dl.Channel) != adapter.ChanAPISteer {
		t.Fatalf("early proof: %+v %+v", res, dl)
	}
	var proofs int
	_ = d.QueryRow(`SELECT count(*) FROM message_proofs`).Scan(&proofs)
	if proofs != 0 {
		t.Fatalf("%d early proofs left", proofs)
	}

	// Proof timeout -> outcome_unknown; a late correlated proof still delivers it.
	m3 := send(t, d, SendInput{Body: "three", To: to(ToRun, "p-1-r1")})
	at4 := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanAPIStart, Now: clock()}, actor)
	})
	settle(t, d, SettleInput{Nonce: at4.Nonce, Outcome: Sent, Receipt: adapter.Receipt{Ref: "turn-3"}})
	moved := must(t, d, func(tx *sql.Tx) ([]int64, error) {
		return ProofTimeout(ctx, tx, clock().Add(time.Minute), "", 0, actor)
	})
	if len(moved) != 1 || state(t, d, m3.Deliveries[0].ID) != OutcomeUnknown {
		t.Fatalf("timeout %v", moved)
	}
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Refs: []string{"turn-3"}}); len(got) != 1 {
		t.Fatalf("late proof %v", got)
	}

	// Definite failures up to the limit make it dead.
	m4 := send(t, d, SendInput{Body: "four", To: to(ToRun, "p-1-r1")})
	for i := 1; ; i++ {
		clock = func() time.Time { return time.Now().Add(time.Duration(i) * time.Hour) }
		at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
			return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanAPIStart, Now: clock()}, actor)
		})
		if at == nil {
			t.Fatalf("attempt %d not claimed", i)
		}
		settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: NotSent, MaxAttempts: 3})
		if st := state(t, d, m4.Deliveries[0].ID); st == Dead {
			if i != 3 {
				t.Fatalf("dead after %d", i)
			}
			break
		}
	}
	// A person re-sends it.
	dl = must(t, d, func(tx *sql.Tx) (*Delivery, error) { return Requeue(ctx, tx, m4.Deliveries[0].ID, actor) })
	if dl.State != Queued || dl.Attempts != 0 {
		t.Fatalf("requeued %+v", dl)
	}
	if _, err := inTx(t, d, func(tx *sql.Tx) (*Delivery, error) { return Requeue(ctx, tx, id, actor) }); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("requeue of a delivered message: %v", err)
	}
	if _, err := inTx(t, d, func(tx *sql.Tx) (SettleResult, error) {
		return Settle(ctx, tx, SettleInput{Nonce: at4.Nonce, Outcome: "maybe"}, actor)
	}); err == nil {
		t.Fatal("unknown outcome accepted")
	}
}

// Markers only prove the run's own deliveries.
func TestProveOwnRunOnly(t *testing.T) {
	d, _ := setup(t)
	m := send(t, d, SendInput{Body: "for the coordinator", To: to(ToRole, RoleCoordinator)})
	at := claim(t, d, "p-coord-r1", adapter.ChanModSubmit)
	if at == nil {
		t.Fatal("coordinator claimed nothing")
	}
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Markers: []int64{m.Deliveries[0].ID}}); len(got) != 0 {
		t.Fatalf("another run proved %v", got)
	}
	if got := prove(t, d, ProveInput{RunID: "p-coord-r1", Channel: adapter.ChanModSubmit}); len(got) != 1 {
		t.Fatalf("channel proof %v", got)
	}
}

func TestInboxAndRead(t *testing.T) {
	d, _ := setup(t)
	child := Sender{Kind: SenderRun, RunID: "p-1-r1"}
	toHuman := send(t, d, SendInput{From: Sender{Kind: SenderRun, RunID: "p-coord-r1"}, Body: "status?", To: to(ToHuman, "human")})
	toCoord := send(t, d, SendInput{From: child, Body: "fyi", To: to(ToRole, RoleCoordinator)})
	toChild := send(t, d, SendInput{Body: "do x", Kind: KindCommand, To: to(ToTask, "p-1")})

	ids := func(items []Item) []int64 {
		var out []int64
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	humanR := Reader{ProjectID: 1}
	coordR, _ := ReaderFor(ctx, d, "p-coord-r1")
	childR, _ := ReaderFor(ctx, d, "p-1-r1")
	check := func(r Reader, want ...int64) {
		t.Helper()
		got, err := Inbox(ctx, d, r, false, 0)
		if err != nil || len(got) != len(want) {
			t.Fatalf("inbox of %+v: %v %v, want %v", r, ids(got), err, want)
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("inbox of %+v: %v, want %v", r, ids(got), want)
			}
		}
	}
	check(humanR, toHuman.ID)
	check(coordR, toCoord.ID)
	check(childR, toChild.ID)

	// Without a live coordinator, coordinator messages are the human's.
	exec(t, d, `UPDATE runs SET status = 'ended', ended_reason = 'stopped' WHERE id = 'p-coord-r1'`)
	check(humanR, toHuman.ID, toCoord.ID)
	read := must(t, d, func(tx *sql.Tx) ([]int64, error) {
		return MarkRead(ctx, tx, []int64{toCoord.Deliveries[0].ID, toChild.Deliveries[0].ID}, humanR, actor)
	})
	if len(read) != 1 || read[0] != toCoord.Deliveries[0].ID {
		t.Fatalf("human read %v", read)
	}
	check(humanR, toHuman.ID)
	if all, _ := Inbox(ctx, d, humanR, true, 0); len(all) != 2 {
		t.Fatalf("all: %v", ids(all))
	}
	// The child reads its task's message directly (channel cli).
	must(t, d, func(tx *sql.Tx) ([]int64, error) {
		return MarkRead(ctx, tx, []int64{toChild.Deliveries[0].ID}, childR, actor)
	})
	dl, _ := GetDelivery(ctx, d, toChild.Deliveries[0].ID)
	if dl.State != Read || str(dl.Channel) != adapter.ChanCLI || str(dl.Proof) != ProofCLI || str(dl.To.Run) != "p-1-r1" {
		t.Fatalf("read by the child: %+v", dl)
	}
	check(childR)
}

// An answer the run reads itself (otag inbox) also moves the task back to running.
func TestAnswerReadWithInbox(t *testing.T) {
	d, _ := setup(t)
	q, _, err := ask(t, d, AskInput{Body: "ok?", To: Recipient{Kind: ToHuman, Ref: "human"}})
	if err != nil {
		t.Fatal(err)
	}
	ans, err := reply(t, d, ReplyInput{To: q.ID, Body: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ReaderFor(ctx, d, "p-1-r1")
	// A harness dialog is open: the answer is read but the task stays waiting.
	exec(t, d, `UPDATE runs SET status = 'waiting' WHERE id = 'p-1-r1'`)
	must(t, d, func(tx *sql.Tx) ([]int64, error) { return MarkRead(ctx, tx, []int64{ans.Deliveries[0].ID}, r, actor) })
	if taskOf(t, d, "p-1") != task.Waiting {
		t.Fatal("task running while a dialog is open")
	}
	ans2, err := reply(t, d, ReplyInput{To: q.ID, Body: "and more", Kind: KindInfo})
	if err != nil || ans2.Kind != KindInfo {
		t.Fatalf("follow-up %v", err)
	}
	exec(t, d, `UPDATE runs SET status = 'idle' WHERE id = 'p-1-r1'; UPDATE tasks SET status = 'waiting' WHERE id = 'p-1'`)
	q2, _, err := ask(t, d, AskInput{Body: "again?", To: Recipient{Kind: ToHuman, Ref: "human"}})
	if err != nil {
		t.Fatal(err)
	}
	ans3, _ := reply(t, d, ReplyInput{To: q2.ID, Body: "fine"})
	must(t, d, func(tx *sql.Tx) ([]int64, error) { return MarkRead(ctx, tx, []int64{ans3.Deliveries[0].ID}, r, actor) })
	if taskOf(t, d, "p-1") != task.Running {
		t.Fatal("answer read but task not running")
	}
}

func TestCloseAsksAndSupersede(t *testing.T) {
	d, _ := setup(t)
	exec(t, d, `UPDATE runs SET status = 'ended', ended_reason = 'done' WHERE id = 'p-coord-r1'`)
	q, _, err := ask(t, d, AskInput{Body: "?"})
	if err != nil {
		t.Fatal(err)
	}
	// The asker crashed; the task's next run asks anew: the old ask is superseded.
	exec(t, d, `UPDATE runs SET status = 'ended', ended_reason = 'crashed' WHERE id = 'p-1-r1';
		INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
		VALUES ('p-1-r2', 1, 'p-1', 'child', 1, 1, 1, 'fake', 'running');
		UPDATE tasks SET current_run_id = 'p-1-r2', status = 'running' WHERE id = 'p-1'`)
	q2, _, err := ask(t, d, AskInput{RunID: "p-1-r2", Body: "new?"})
	if err != nil {
		t.Fatal(err)
	}
	q, _ = Get(ctx, d, q.ID)
	if str(q.ClosedReason) != ClosedSuperseded || q.Deliveries[0].State != Expired {
		t.Fatalf("superseded %+v", q)
	}
	must(t, d, func(tx *sql.Tx) (bool, error) { return true, CloseAsks(ctx, tx, "p-1", ClosedAskerDone, actor) })
	q2, _ = Get(ctx, d, q2.ID)
	if str(q2.ClosedReason) != ClosedAskerDone || q2.Deliveries[0].State != Expired {
		t.Fatalf("closed %+v", q2)
	}
	if _, err := reply(t, d, ReplyInput{To: q2.ID, Body: "late"}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("answer to a closed ask: %v", err)
	}
	must(t, d, func(tx *sql.Tx) (bool, error) { return true, CloseAsks(ctx, tx, "p-1", ClosedAskerDone, actor) })
}

func TestExpire(t *testing.T) {
	d, _ := setup(t)
	past := ts(time.Now().Add(-time.Minute))
	old := send(t, d, SendInput{Body: "old", To: to(ToRun, "p-1-r1"), ExpiresAt: past})
	fresh := send(t, d, SendInput{Body: "fresh", To: to(ToTask, "p-2")})
	exec(t, d, `UPDATE tasks SET status = 'cancelled' WHERE id = 'p-2'`)
	toCoord := send(t, d, SendInput{Body: "c", To: to(ToRun, "p-coord-r1")})
	exec(t, d, `UPDATE runs SET status = 'ended', ended_reason = 'stopped' WHERE id = 'p-coord-r1'`)
	if at := claim(t, d, "p-1-r1", adapter.ChanToolContext); at != nil {
		t.Fatal("claimed an expired message")
	}
	exec(t, d, `INSERT INTO message_proofs (project_id, run_id, ref, created_at) VALUES (1, 'p-1-r1', 'x', ?)`,
		ts(time.Now().Add(-2*time.Hour)))
	res := must(t, d, func(tx *sql.Tx) (ExpireResult, error) { return Expire(ctx, tx, time.Time{}, 0, actor) })
	if res.Expired != 1 || res.Dead != 2 || res.Proofs != 1 {
		t.Fatalf("expire %+v", res)
	}
	if state(t, d, old.Deliveries[0].ID) != Expired || state(t, d, fresh.Deliveries[0].ID) != Dead || state(t, d, toCoord.Deliveries[0].ID) != Dead {
		t.Fatal("states after expire")
	}
}

func TestNeeds(t *testing.T) {
	d, _ := setup(t)
	m := send(t, d, SendInput{Body: "lost?", To: to(ToRole, RoleCoordinator)})
	at := claim(t, d, "p-coord-r1", adapter.ChanAPIStart)
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Unknown})
	q, _, err := ask(t, d, AskInput{Body: "need input"})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, d, `UPDATE tasks SET status = 'blocked', status_reason = 'quota' WHERE id = 'p-2';
		INSERT INTO tasks (id, project_id, title, status) VALUES ('p-4', 1, 'four', 'running');
		INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
		VALUES ('p-4-r1', 1, 'p-4', 'child', 1, 1, 1, 'fake', 'idle');`)
	n, err := NeedsOf(ctx, d, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Questions) != 1 || n.Questions[0].Message.ID != q.ID || n.Questions[0].To != "coordinator p-coord-r1" {
		t.Fatalf("questions %+v", n.Questions)
	}
	if len(n.Deliveries) != 1 || n.Deliveries[0].Message != m.ID || n.Deliveries[0].Delivery.State != OutcomeUnknown {
		t.Fatalf("deliveries %+v", n.Deliveries)
	}
	if len(n.Blocked) != 1 || n.Blocked[0].Task != "p-2" || n.Blocked[0].Reason != "quota" {
		t.Fatalf("blocked %+v", n.Blocked)
	}
	// p-1-r1 is idle but has an open ask; p-4-r1 is idle without one.
	if len(n.Idle) != 1 || n.Idle[0].Run != "p-4-r1" {
		t.Fatalf("idle %+v", n.Idle)
	}
	if other, _ := NeedsOf(ctx, d, 2); len(other.Questions)+len(other.Deliveries)+len(other.Blocked)+len(other.Idle) != 0 {
		t.Fatalf("project 2 needs %+v", other)
	}
}

func TestRenderAndScan(t *testing.T) {
	coordRole, run, tk := RoleCoordinator, "OTAG-coord-r1", "OTAG-7"
	reply := int64(42)
	items := []Item{
		{Message: Message{ID: 57, Kind: KindAnswer, ReplyTo: &reply, Body: "Use SQLite.",
			From: From{Kind: SenderRun, Run: &run, Role: &coordRole}}, Delivery: Delivery{ID: 130}},
		{Message: Message{ID: 44, Kind: KindQuestion, Body: "Which port?",
			From: From{Kind: SenderRun, Run: strp("OTAG-9-r1"), Task: &tk}}, Delivery: Delivery{ID: 131}},
		{Message: Message{ID: 45, Kind: KindInfo, Body: "fyi", From: From{Kind: SenderHuman}}, Delivery: Delivery{ID: 132}},
	}
	want := "[otag#d130] answer #57 to question #42, from the coordinator (run OTAG-coord-r1):\nUse SQLite.\n---\n" +
		"[otag#d131] question #44 from run OTAG-9-r1 (task OTAG-7):\nWhich port?\nReply with: otag reply 44 \"<answer>\"\n---\n" +
		"[otag#d132] message #45 from the human:\nfyi"
	if got := Render(items, 0); got != want {
		t.Fatalf("render:\n%s\nwant:\n%s", got, want)
	}
	items[0].Body = strings.Repeat("é", 3000)
	got := Render(items, 1000)
	if len(got) > 1000 || !strings.Contains(got, "[otag#d130]") || !strings.Contains(got, "[otag#d132]") || !strings.Contains(got, "truncated") {
		t.Fatalf("cut render (%d): %q", len(got), got)
	}
	if !utf8Valid(got) {
		t.Fatal("cut inside a rune")
	}
	if ids := ScanMarkers([]byte(got + ` "[otag#d131]" [otag#dx] [otag#d0] [otag#d99999999999999999999]`)); len(ids) != 3 || ids[0] != 130 || ids[2] != 132 {
		t.Fatalf("scan %v", ids)
	}
	if ScanMarkers([]byte(strings.Repeat("a", 1<<20))) != nil {
		t.Fatal("markers in plain text")
	}
}

func strp(s string) *string { return &s }

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }
