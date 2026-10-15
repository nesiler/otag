package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/task"
)

var human = msg.Sender{Kind: msg.SenderHuman, Actor: "user:test"}

// msgSetup is setupRun with the run x-1-r1 in status, its data dir and a registry.
func msgSetup(t *testing.T, status string) (*db.DB, *adapter.Registry, func(string) string) {
	t.Helper()
	d := setupRun(t)
	if _, err := d.Exec(`UPDATE runs SET status = ? WHERE id = 'x-1-r1'`, status); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "OTAG_DATA_DIR" {
			return dir
		}
		return ""
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs", "x-1-r1"), 0o700); err != nil {
		t.Fatal(err)
	}
	return d, adapter.NewRegistry(fake.New()), getenv
}

func sendTo(t *testing.T, d *db.DB, body string) *msg.Message {
	t.Helper()
	var m *msg.Message
	err := d.Tx(context.Background(), func(tx *sql.Tx) (err error) {
		m, _, err = msg.Send(context.Background(), tx, msg.SendInput{ProjectID: 1, From: human, Body: body,
			To: []msg.Recipient{{Kind: msg.ToRun, Ref: "x-1-r1"}}}, msg.Actor{Name: "user:test"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func delivery(t *testing.T, d *db.DB, id int64) *msg.Delivery {
	t.Helper()
	dl, err := msg.GetDelivery(context.Background(), d, id)
	if err != nil {
		t.Fatal(err)
	}
	return dl
}

func fhook(t *testing.T, d *db.DB, reg *adapter.Registry, kind, payload string) HookResponse {
	t.Helper()
	resp, err := HandleHook(context.Background(), d, reg, HookRequest{Harness: "fake", Kind: kind, RunID: "x-1-r1",
		Payload: []byte(payload)})
	if err != nil || resp.MsgErr != nil {
		t.Fatalf("%s %s: %v / %v", kind, payload, err, resp.MsgErr)
	}
	return resp
}

func runStatus(t *testing.T, d *db.DB) string {
	r, err := Get(context.Background(), d, "x-1-r1")
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

func TestHookToolContext(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	m := sendTo(t, d, "also add a test")
	id := m.Deliveries[0].ID

	// A hook without a slot carries nothing.
	if resp := fhook(t, d, reg, "activity", `{"event_id":"e0"}`); resp.Output != nil || resp.Delivered != nil {
		t.Fatalf("no slot: %+v", resp)
	}
	resp := fhook(t, d, reg, "activity", `{"slot":"tool","event_id":"e1"}`)
	var out struct{ Context string }
	if json.Unmarshal(resp.Output, &out) != nil || !strings.Contains(out.Context, msg.Marker(id)) ||
		!strings.Contains(out.Context, "also add a test") || resp.Delivered == nil {
		t.Fatalf("output %s", resp.Output)
	}
	if dl := delivery(t, d, id); dl.State != msg.Dispatched || *dl.Channel != adapter.ChanToolContext {
		t.Fatalf("before print: %+v", dl)
	}
	if err := resp.Delivered(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dl := delivery(t, d, id); dl.State != msg.Delivered || *dl.Proof != msg.ProofChannel {
		t.Fatalf("after print: %s %v", dl.State, dl.Proof)
	}

	// A repeated hook (same event id) carries nothing new.
	m2 := sendTo(t, d, "second")
	if resp := fhook(t, d, reg, "activity", `{"slot":"tool","event_id":"e1"}`); resp.Output != nil {
		t.Fatalf("duplicate hook carried %s", resp.Output)
	}
	if dl := delivery(t, d, m2.Deliveries[0].ID); dl.State != msg.Queued {
		t.Fatalf("duplicate claimed: %s", dl.State)
	}
}

func TestHookStopBlock(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	fhook(t, d, reg, "turn_start", `{"event_id":"p1"}`)
	m := sendTo(t, d, "one more thing")
	id := m.Deliveries[0].ID

	resp := fhook(t, d, reg, "turn_end", `{"slot":"stop","session_id":"s1"}`)
	var block struct{ Decision, Reason string }
	if json.Unmarshal(resp.Output, &block) != nil || block.Decision != "block" || !strings.Contains(block.Reason, msg.Marker(id)) {
		t.Fatalf("stop output %s", resp.Output)
	}
	if resp.Delivered != nil {
		t.Fatal("a Stop block is proven by the continued Stop, not by printing")
	}
	if st := runStatus(t, d); st != Running {
		t.Fatalf("a blocked Stop ended the turn: %s", st)
	}
	if dl := delivery(t, d, id); dl.State != msg.Dispatched || dl.Receipt == nil || *dl.Receipt != "stop:s1" {
		t.Fatalf("blocked: %+v", dl)
	}
	// The continued Stop of another session proves nothing; of this one, the block.
	fhook(t, d, reg, "activity", `{"session_id":"s1"}`)
	resp = fhook(t, d, reg, "turn_end", `{"slot":"stop","stop_hook_active":true,"session_id":"other"}`)
	if dl := delivery(t, d, id); dl.State != msg.Dispatched {
		t.Fatalf("other session proved it: %s", dl.State)
	}
	fhook(t, d, reg, "turn_start", `{"event_id":"p2"}`) // (back to running for the next check)
	// A new turn's prompt does not end it; ending it without the continued Stop does.
	m2 := sendTo(t, d, "two")
	fhook(t, d, reg, "turn_end", `{"slot":"stop","session_id":"s1"}`) // blocks with m2; m1 becomes unknown
	if dl := delivery(t, d, id); dl.State != msg.OutcomeUnknown {
		t.Fatalf("not continued: %s", dl.State)
	}
	resp = fhook(t, d, reg, "turn_end", `{"slot":"stop","stop_hook_active":true,"session_id":"s1"}`)
	if dl := delivery(t, d, m2.Deliveries[0].ID); dl.State != msg.Delivered || *dl.Proof != msg.ProofChannel {
		t.Fatalf("continued: %s %v", dl.State, dl.Proof)
	}
	if resp.Output != nil || runStatus(t, d) != Idle {
		t.Fatalf("nothing queued: must not block (%s), run %s", resp.Output, runStatus(t, d))
	}
}

func TestHookStopBlockCap(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	fhook(t, d, reg, "turn_start", `{"event_id":"p1"}`)
	for i := 0; i < StopBlockLimit+1; i++ {
		sendTo(t, d, "msg")
		active := i > 0
		resp := fhook(t, d, reg, "turn_end", `{"slot":"stop","session_id":"s","stop_hook_active":`+map[bool]string{true: "true", false: "false"}[active]+`}`)
		if i < StopBlockLimit && resp.Output == nil {
			t.Fatalf("block %d missing", i)
		}
		if i == StopBlockLimit && resp.Output != nil {
			t.Fatalf("block %d beyond the cap: %s", i, resp.Output)
		}
	}
	if st := runStatus(t, d); st != Idle {
		t.Fatalf("after the cap the turn ends: %s", st)
	}
	// A new prompt resets the count; the queued message goes with it as context.
	resp := fhook(t, d, reg, "turn_start", `{"event_id":"p2","slot":"prompt"}`)
	if !strings.Contains(string(resp.Output), "[otag#d") {
		t.Fatalf("prompt context %s", resp.Output)
	}
}

// markerAdapter is the fake harness whose delivery's prompt hook arrives before
// DeliverReceipt returns (proof before receipt).
type markerAdapter struct {
	*fake.Adapter
	d   *db.DB
	reg *adapter.Registry
	t   *testing.T
}

func (m *markerAdapter) DeliverReceipt(ctx context.Context, t adapter.Target, msgIn adapter.Message) (adapter.Receipt, error) {
	b, _ := json.Marshal(map[string]string{"prompt": msgIn.Text})
	if _, err := HandleHook(ctx, m.d, m.reg, HookRequest{Harness: "fake", Kind: "turn_start", RunID: t.RunID, Payload: b}); err != nil {
		return adapter.Receipt{}, err
	}
	return adapter.Receipt{Ref: "turn-1"}, nil
}

func TestDispatchProofBeforeReceipt(t *testing.T) {
	d, _, getenv := msgSetup(t, Idle)
	ma := &markerAdapter{Adapter: fake.New(), d: d, t: t}
	ma.reg = adapter.NewRegistry(ma)
	m := sendTo(t, d, "hello")
	disp := &Dispatcher{DB: d, Adapters: ma.reg, Getenv: getenv, Actor: msg.Actor{Name: "user:test"}}
	ra := disp.Dispatch(context.Background(), "x-1-r1")
	if ra.Result != "delivered" || ra.Channel != adapter.ChanAPIStart {
		t.Fatalf("%+v", ra)
	}
	dl := delivery(t, d, m.Deliveries[0].ID)
	if dl.State != msg.Delivered || *dl.Proof != msg.ProofMarker || dl.Receipt == nil || *dl.Receipt != "turn-1" {
		t.Fatalf("%+v", dl)
	}
}

func TestDispatchOutcomes(t *testing.T) {
	d, reg, getenv := msgSetup(t, Idle)
	runDir := filepath.Join(getenv("OTAG_DATA_DIR"), "runs", "x-1-r1")
	disp := &Dispatcher{DB: d, Adapters: reg, Getenv: getenv, Actor: msg.Actor{Name: "user:test"}}
	ctx := context.Background()

	m := sendTo(t, d, "reply OK-1")
	id := m.Deliveries[0].ID
	rs := disp.AttemptRun(ctx, "x-1-r1")
	if len(rs) != 1 || rs[0].Delivery != id || rs[0].Result != "dispatched" {
		t.Fatalf("%+v", rs)
	}
	inbox, _ := os.ReadFile(filepath.Join(runDir, fake.InboxFile))
	if !strings.Contains(string(inbox), msg.Marker(id)+" ") || !strings.Contains(string(inbox), "reply OK-1") {
		t.Fatalf("inbox %q", inbox)
	}
	// One injected attempt in flight per run: the next message waits for the proof.
	m2 := sendTo(t, d, "second")
	if ra := disp.Dispatch(ctx, "x-1-r1"); ra.Result != "skipped" {
		t.Fatalf("second in flight: %+v", ra)
	}
	// The prompt with the marker proves it (also as a duplicate event).
	fhook(t, d, reg, "turn_start", `{"prompt":"`+msg.Marker(id)+` reply OK-1","event_id":"u1"}`)
	if dl := delivery(t, d, id); dl.State != msg.Delivered || *dl.Proof != msg.ProofMarker {
		t.Fatalf("proof: %+v", dl)
	}
	// Running: a steer (api_steer).
	ra := disp.Dispatch(ctx, "x-1-r1")
	if ra.Result != "dispatched" || ra.Channel != adapter.ChanAPISteer {
		t.Fatalf("steer: %+v", ra)
	}
	fhook(t, d, reg, "turn_start", `{"prompt":"`+msg.Marker(m2.Deliveries[0].ID)+`"}`)

	// Not sent: queued again with a retry time.
	_ = os.WriteFile(filepath.Join(runDir, fake.ModeFile), []byte("notsent"), 0o600)
	m3 := sendTo(t, d, "third")
	if r := disp.Attempt(ctx, m3.Deliveries[0].ID); r.Result != "queued" {
		t.Fatalf("not sent: %+v", r)
	}
	if dl := delivery(t, d, m3.Deliveries[0].ID); dl.State != msg.Queued || dl.NextAttemptAt == nil || dl.Attempts != 1 {
		t.Fatalf("requeued: %+v", dl)
	}
	// Ambiguous: outcome_unknown, never re-sent by itself.
	_ = os.WriteFile(filepath.Join(runDir, fake.ModeFile), []byte("unknown"), 0o600)
	_, _ = d.Exec(`UPDATE message_deliveries SET next_attempt_at = NULL`)
	if r := disp.Attempt(ctx, m3.Deliveries[0].ID); r.Result != "failed" {
		t.Fatalf("unknown: %+v", r)
	}
	if dl := delivery(t, d, m3.Deliveries[0].ID); dl.State != msg.OutcomeUnknown {
		t.Fatalf("unknown: %s", dl.State)
	}

	// Never into a harness dialog or a starting run.
	for _, st := range []string{Waiting, Starting} {
		_, _ = d.Exec(`UPDATE runs SET status = ? WHERE id = 'x-1-r1'`, st)
		sendTo(t, d, "while "+st)
		if ra := disp.Dispatch(ctx, "x-1-r1"); ra.Result != "skipped" {
			t.Fatalf("%s: %+v", st, ra)
		}
	}
}

func TestDispatchClaudeBusyKeepsHooks(t *testing.T) {
	r := &Run{Harness: "claude", Status: Running}
	if inj, why := injectPlan(r, claudeLike{}); inj.Channel != "" || why == "" {
		t.Fatalf("busy claude: %+v %q", inj, why)
	}
	r.Status = Idle
	if inj, _ := injectPlan(r, claudeLike{}); inj.Channel != adapter.ChanModSubmit {
		t.Fatalf("idle claude: %+v", inj)
	}
	r = &Run{Harness: "codex", Status: Idle, Session: &Session{ServerURL: "unix:///x/as.sock"}}
	if inj, _ := injectPlan(r, claudeLike{}); inj.Channel != adapter.ChanAPIStart {
		t.Fatalf("codex: %+v", inj)
	}
	r.Session.ServerURL = ""
	if inj, _ := injectPlan(r, claudeLike{}); inj.Channel != "" {
		t.Fatalf("codex hooks mode: %+v", inj)
	}
}

type claudeLike struct{ *fake.Adapter }

// TestDispatchRacesHook: a dispatcher and a hook claim the same queued messages at
// once; each delivery is taken by exactly one of them.
func TestDispatchRacesHook(t *testing.T) {
	d, reg, getenv := msgSetup(t, Running)
	disp := &Dispatcher{DB: d, Adapters: reg, Getenv: getenv, Actor: msg.Actor{Name: "daemon"}}
	ctx := context.Background()
	for round := 0; round < 10; round++ {
		m := sendTo(t, d, "race")
		var wg sync.WaitGroup
		var hookOut []byte
		var ra RunAttempt
		wg.Add(2)
		go func() {
			defer wg.Done()
			for {
				resp, err := HandleHook(ctx, d, reg, HookRequest{Harness: "fake", Kind: "activity", RunID: "x-1-r1",
					Payload: []byte(`{"slot":"tool"}`)})
				if err == nil {
					hookOut = resp.Output
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for {
				if ra = disp.Dispatch(ctx, "x-1-r1"); !strings.Contains(ra.Reason, "busy") {
					return
				}
			}
		}()
		wg.Wait()
		byHook := strings.Contains(string(hookOut), msg.Marker(m.Deliveries[0].ID))
		byDisp := len(ra.Deliveries) > 0
		if byHook == byDisp {
			t.Fatalf("round %d: hook %v dispatcher %v (%+v)", round, byHook, byDisp, ra)
		}
		// Settle whatever is in flight so the next round starts clean.
		_, _ = d.Exec(`UPDATE message_deliveries SET state = 'delivered', proof = 'channel' WHERE state = 'dispatched'`)
	}
}

func TestDialogKeepsAskWaiting(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	ctx := context.Background()
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, _, err := msg.Ask(ctx, tx, msg.AskInput{RunID: "x-1-r1", Body: "which db?"}, msg.Actor{Name: "run:x-1-r1"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fhook(t, d, reg, "permission", `{"text":"Allow curl?"}`)
	fhook(t, d, reg, "activity", `{}`) // the dialog closes
	tk, err := task.Get(ctx, d, "x-1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != task.Waiting || runStatus(t, d) != Running {
		t.Fatalf("task %s run %s", tk.Status, runStatus(t, d))
	}
}

func TestHookMessagingFailureKeepsEvent(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	sendTo(t, d, "x")
	// Break the deliveries table for writes: the claim fails, the event is kept.
	if _, err := d.Exec(`CREATE TRIGGER no_claim BEFORE UPDATE ON message_deliveries BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	resp, err := HandleHook(context.Background(), d, reg, HookRequest{Harness: "fake", Kind: "turn_end", RunID: "x-1-r1",
		Payload: []byte(`{"slot":"tool"}`)})
	if err != nil || resp.MsgErr == nil || resp.Output != nil || resp.EventID == 0 {
		t.Fatalf("%+v %v", resp, err)
	}
	if st := runStatus(t, d); st != Idle {
		t.Fatalf("event lost: %s", st)
	}
}
