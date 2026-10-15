package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nesiler/otag/internal/adapter"
)

// Concurrent calls get their own responses although the server answers in
// reverse order, with server requests and notifications interleaved.
func TestMuxConcurrentOutOfOrder(t *testing.T) {
	const n = 8
	var held []envelope
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if len(msg.ID) == 0 || msg.Method == "" {
			return
		}
		if msg.Method == "initialize" {
			wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": map[string]any{}})
			return
		}
		held = append(held, msg)
		if len(held) < n {
			return
		}
		for i := len(held) - 1; i >= 0; i-- {
			wsSend(ctx, ws, map[string]any{"id": 1000 + i, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "s"}})
			wsSend(ctx, ws, map[string]any{"method": "item/started", "params": map[string]any{"threadId": "s", "item": map[string]any{"id": fmt.Sprint(i)}}})
			wsSend(ctx, ws, map[string]any{"id": held[i].ID, "result": map[string]any{"echo": held[i].Method}})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	var mu sync.Mutex
	var items []string
	c.setHandler(func(msg envelope) {
		if msg.Method == "item/started" {
			var p payload
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			items = append(items, p.Item.ID)
			mu.Unlock()
		}
	})
	c.goLive()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			method := fmt.Sprintf("m/%d", i)
			var res struct{ Echo string }
			if err := c.call(ctx, method, map[string]any{}, &res); err != nil || res.Echo != method {
				errs <- fmt.Errorf("%s: %q %v", method, res.Echo, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// The reader dispatched every notification, in arrival order, and kept the requests.
	deadline := time.Now().Add(2 * time.Second)
	for c.openRequests("s") != n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if c.openRequests("s") != n || len(items) != n || items[0] != fmt.Sprint(n-1) || items[n-1] != "0" {
		t.Fatal(c.openRequests("s"), items)
	}
}

// Before goLive a call dispatches only what arrived before its response.
func TestMuxQueuedOrder(t *testing.T) {
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if len(msg.ID) == 0 || msg.Method == "" {
			return
		}
		if msg.Method == "probe" {
			wsSend(ctx, ws, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "s", "turn": map[string]any{"id": "before"}}})
			wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": map[string]any{}})
			wsSend(ctx, ws, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "s", "turn": map[string]any{"id": "after"}}})
			return
		}
		wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": map[string]any{}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	seen := make(chan string, 4)
	c.setHandler(func(msg envelope) {
		var p payload
		_ = json.Unmarshal(msg.Params, &p)
		seen <- p.Turn.ID
	})
	if err := c.call(ctx, "probe", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || <-seen != "before" {
		t.Fatal("call must dispatch exactly the messages before its response")
	}
	// "after" stays queued (the listener is still reconciling) until goLive.
	time.Sleep(100 * time.Millisecond)
	if len(seen) != 0 {
		t.Fatal("dispatched before goLive")
	}
	c.goLive()
	select {
	case id := <-seen:
		if id != "after" {
			t.Fatal(id)
		}
	case <-ctx.Done():
		t.Fatal("queued message lost")
	}
}

func TestMuxDisconnect(t *testing.T) {
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if msg.Method == "initialize" {
			wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": map[string]any{}})
		}
		if msg.Method == "hang" {
			_ = ws.CloseNow()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	err = c.call(ctx, "hang", nil, nil)
	var refused *rpcError
	if err == nil || errors.As(err, &refused) || errors.Is(err, errNotSent) {
		t.Fatal("a lost response is an unknown outcome", err)
	}
	if c.wait(ctx) == nil {
		t.Fatal("reader error lost")
	}
	if err := c.call(ctx, "late", nil, nil); !errors.Is(err, errNotSent) {
		t.Fatal(err)
	}
}

// fakeThread is a Codex app-server holding one thread "s".
type fakeThread struct {
	mu       sync.Mutex
	active   string
	turns    int
	requests []envelope
	steerErr string
	// connections counts accepted initialize calls.
	connections int
	listDelay   time.Duration
	drop        chan struct{}
}

func (f *fakeThread) serve(t *testing.T) string {
	return mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if len(msg.ID) == 0 || msg.Method == "" {
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, msg)
		var p map[string]any
		_ = json.Unmarshal(msg.Params, &p)
		result := any(map[string]any{})
		var notify []any
		switch msg.Method {
		case "initialize":
			f.connections++
			if f.connections > 1 && f.drop != nil {
				// a reconnect: let the test observe it
				close(f.drop)
				f.drop = nil
			}
		case "thread/resume":
			status := map[string]any{"type": "idle"}
			if f.active != "" {
				status["type"] = "active"
			}
			result = map[string]any{"thread": map[string]any{"id": "s", "status": status}}
		case "thread/turns/list":
			delay := f.listDelay
			f.mu.Unlock()
			time.Sleep(delay)
			f.mu.Lock()
			result = map[string]any{"data": []any{}}
		case "turn/start":
			if f.active == "" {
				f.turns++
				f.active = fmt.Sprintf("t%d", f.turns)
				notify = append(notify, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "s", "turn": map[string]any{"id": f.active, "status": "inProgress"}}})
			}
			result = map[string]any{"turn": map[string]any{"id": f.active, "status": "inProgress"}}
		case "turn/steer":
			if f.steerErr != "" || p["expectedTurnId"] != f.active || f.active == "" {
				// The texts of codex 0.162.1.
				text := f.steerErr
				if text == "" && f.active == "" {
					text = "no active turn to steer"
				} else if text == "" {
					text = fmt.Sprintf("expected active turn id `%s` but found `%s`", p["expectedTurnId"], f.active)
				}
				f.mu.Unlock()
				wsSend(ctx, ws, map[string]any{"id": msg.ID, "error": map[string]any{"code": -32600, "message": text}})
				return
			}
			result = map[string]any{"turnId": f.active}
		}
		f.mu.Unlock()
		wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": result})
		for _, n := range notify {
			wsSend(ctx, ws, n)
		}
	})
}

func (f *fakeThread) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.Method)
	}
	return out
}

func (f *fakeThread) last(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].Method == method {
			var p map[string]any
			_ = json.Unmarshal(f.requests[i].Params, &p)
			return p
		}
	}
	return nil
}

// complete ends the active turn on the server only (its notification is not sent).
func (f *fakeThread) complete() {
	f.mu.Lock()
	f.active = ""
	f.mu.Unlock()
}

// ctlFixture is a live, attached controller over a fake thread.
func ctlFixture(t *testing.T, f *fakeThread, thread map[string]any) (*controller, *rpc, adapter.Target) {
	t.Helper()
	dir, err := os.MkdirTemp("/private/tmp", "otag-ctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := connect(ctx, f.serve(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	k := newController(dir, "s", "/work", turnPolicy(thread))
	stop, err := k.serve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	c.setHandler(func(msg envelope) {
		if len(msg.ID) == 0 {
			k.apply(msg.Method, msg.Params)
		}
	})
	c.goLive()
	k.apply("thread/status/changed", []byte(`{"threadId":"s","status":{"type":"idle"}}`))
	k.attach(c)
	if err := os.WriteFile(filepath.Join(dir, "tui-attached"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, ctlSocket))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("ctl socket mode", info, err)
	}
	return k, c, adapter.Target{RunID: "r", HarnessSessionID: "s", ServerURL: "unix://" + filepath.Join(dir, "as.sock")}
}

func reason(err error) string {
	var d *DeliveryError
	if errors.As(err, &d) {
		return d.Reason
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestDeliverStartSteerBusy(t *testing.T) {
	f := &fakeThread{}
	k, _, target := ctlFixture(t, f, map[string]any{"cwd": "/work"})
	ctx := context.Background()
	text := "[otag#d1] reply OK-1"
	r, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a1", Text: text})
	if err != nil || r != (Receipt{Kind: "start", TurnID: "t1"}) {
		t.Fatal(r, err)
	}
	p := f.last("turn/start")
	input, _ := json.Marshal(p["input"])
	if p["threadId"] != "s" || p["approvalPolicy"] != "on-request" || p["approvalsReviewer"] != "user" || p["permissions"] != "otag" || p["cwd"] != "/work" || p["clientUserMessageId"] != "otag-a1" || !strings.Contains(string(input), text) {
		t.Fatal("delivered turn is not pinned like a TUI turn", p)
	}
	// Busy without steer: refused, nothing sent.
	before := len(f.methods())
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a2", Text: "x"}); reason(err) != ReasonBusy {
		t.Fatal(err)
	}
	if len(f.methods()) != before {
		t.Fatal("busy delivery reached Codex", f.methods())
	}
	// Busy with steer: turn/steer against the active turn.
	r, err = DeliverReceipt(ctx, target, adapter.Message{ID: "a3", Text: "[otag#d3] steer", Steer: true})
	if err != nil || r != (Receipt{Kind: "steer", TurnID: "t1"}) || f.last("turn/steer")["expectedTurnId"] != "t1" {
		t.Fatal(r, err, f.last("turn/steer"))
	}
	// The same attempt again returns the recorded receipt without a second injection.
	before = len(f.methods())
	if r2, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a3", Text: "[otag#d3] steer", Steer: true}); err != nil || r2 != r || len(f.methods()) != before {
		t.Fatal("attempt injected twice", r2, err)
	}
	// Stale expectedTurnId: the turn ended on the server but the completion has
	// not arrived yet. The attempt fails; it never starts another turn.
	f.complete()
	starts := strings.Count(strings.Join(f.methods(), ","), "turn/start,")
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a4", Text: "x", Steer: true}); reason(err) != ReasonBusyMismatch {
		t.Fatal(err)
	}
	if strings.Count(strings.Join(f.methods(), ","), "turn/start,") != starts || slices.Contains(f.methods()[before:], "turn/start") {
		t.Fatal("steer mismatch fell back to turn/start", f.methods())
	}
	// Once the completion arrives the thread is idle again: a new turn.
	k.apply("turn/completed", []byte(`{"threadId":"s","turn":{"id":"t1","status":"completed"}}`))
	if r, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a5", Text: "y", Steer: true}); err != nil || r != (Receipt{Kind: "start", TurnID: "t2"}) {
		t.Fatal(r, err)
	}
	if err := New().Deliver(ctx, target, adapter.Message{ID: "a6", Text: "z"}); reason(err) != ReasonBusy {
		t.Fatal(err)
	}
	// A turn that moved on (another active turn): mismatch too.
	k.begin("t-old")
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a7", Text: "z", Steer: true}); reason(err) != ReasonBusyMismatch || !strings.Contains(err.Error(), "but found `t2`") {
		t.Fatal(err)
	}
	// Any other refusal is reported as such, not as a mismatch.
	k.begin("t2")
	f.mu.Lock()
	f.steerErr = "input exceeds the limit"
	f.mu.Unlock()
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a8", Text: "z", Steer: true}); reason(err) != ReasonRejected {
		t.Fatal(err)
	}
}

func TestDeliverRefusesDialogsAndUnready(t *testing.T) {
	f := &fakeThread{}
	k, c, target := ctlFixture(t, f, map[string]any{"sandbox": lightSandbox})
	ctx := context.Background()
	k.apply("thread/status/changed", []byte(`{"threadId":"s","status":{"type":"active","activeFlags":["waitingOnApproval"]}}`))
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "a", Text: "x", Steer: true}); reason(err) != ReasonWaitingDialog {
		t.Fatal(err)
	}
	k.apply("thread/status/changed", []byte(`{"threadId":"s","status":{"type":"idle"}}`))
	// An open server request (question) for the thread also blocks injection.
	c.observe(envelope{ID: json.RawMessage(`7`), Method: "item/tool/requestUserInput", Params: json.RawMessage(`{"threadId":"s"}`)})
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "b", Text: "x"}); reason(err) != ReasonWaitingDialog {
		t.Fatal(err)
	}
	c.observe(envelope{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"s","requestId":7}`)})
	if slices.Contains(f.methods(), "turn/start") || slices.Contains(f.methods(), "turn/steer") {
		t.Fatal("injected into an open dialog", f.methods())
	}
	// No attached TUI: nobody could answer an approval.
	_ = os.Remove(filepath.Join(k.dir, "tui-attached"))
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "c", Text: "x"}); reason(err) != ReasonNotReady {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(k.dir, "tui-attached"), nil, 0o600)
	k.detach(c)
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "d", Text: "x"}); reason(err) != ReasonNotReady {
		t.Fatal(err)
	}
	k.attach(c)
	if _, err := DeliverReceipt(ctx, adapter.Target{HarnessSessionID: "other", ServerURL: target.ServerURL}, adapter.Message{ID: "e", Text: "x"}); reason(err) != ReasonBadRequest {
		t.Fatal(err)
	}
	if _, err := DeliverReceipt(ctx, adapter.Target{ServerURL: "unix://" + filepath.Join(t.TempDir(), "as.sock")}, adapter.Message{ID: "f", Text: "x"}); reason(err) != ReasonNotReady {
		t.Fatal(err)
	}
	// A light thread's delivered turn gets the light pins.
	if r, err := DeliverReceipt(ctx, target, adapter.Message{ID: "g", Text: "x"}); err != nil || r.Kind != "start" {
		t.Fatal(r, err)
	}
	p := f.last("turn/start")
	if p["approvalPolicy"] != lightApproval || p["permissions"] != nil || p["sandboxPolicy"].(map[string]any)["type"] != "dangerFullAccess" || p["cwd"] != "/work" {
		t.Fatal(p)
	}
}

// An incomplete or oversized line gets an explicit bad_request, never silence.
func TestCtlIncompleteLine(t *testing.T) {
	f := &fakeThread{}
	k, _, _ := ctlFixture(t, f, map[string]any{})
	for _, data := range [][]byte{[]byte(`{"op":"deliver"`), []byte(strings.Repeat("x", ctlMaxLine+10))} {
		conn, err := net.Dial("unix", filepath.Join(k.dir, ctlSocket))
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = conn.Write(data); _ = conn.(*net.UnixConn).CloseWrite() }()
		answer, err := io.ReadAll(conn)
		_ = conn.Close()
		var resp ctlResponse
		if err != nil || json.Unmarshal(answer, &resp) != nil || resp.OK || resp.Reason != ReasonBadRequest {
			t.Fatalf("%q %v", answer, err)
		}
	}
	if len(f.methods()) != 1 { // initialize only
		t.Fatal(f.methods())
	}
}

// A late turn/started of a completed turn stays ignored after many completions.
func TestCtlEndedEviction(t *testing.T) {
	k := newController(t.TempDir(), "s", "", nil)
	done := func(id string) {
		k.apply("turn/completed", []byte(`{"threadId":"s","turn":{"id":"`+id+`"}}`))
	}
	done("late")
	for i := range ctlMemory - 1 {
		done(fmt.Sprint(i))
	}
	k.apply("turn/started", []byte(`{"threadId":"s","turn":{"id":"late"}}`))
	if k.active != "" {
		t.Fatal("revived a completed turn")
	}
	done("one-more") // evicts only the oldest ("late")
	k.apply("turn/started", []byte(`{"threadId":"s","turn":{"id":"0"}}`))
	if k.active != "" || len(k.ended) != ctlMemory {
		t.Fatal(k.active, len(k.ended))
	}
}

// Deliveries are serialized: of two concurrent ones on an idle thread exactly one
// starts the turn, the other sees it busy.
func TestDeliverSerialized(t *testing.T) {
	f := &fakeThread{}
	_, _, target := ctlFixture(t, f, map[string]any{})
	results := make(chan error, 2)
	for _, id := range []string{"x", "y"} {
		go func() {
			_, err := DeliverReceipt(context.Background(), target, adapter.Message{ID: id, Text: "m"})
			results <- err
		}()
	}
	got := []string{reason(<-results), reason(<-results)}
	slices.Sort(got)
	if got[0] != "" || got[1] != ReasonBusy || strings.Count(strings.Join(f.methods(), ","), "turn/start") != 1 {
		t.Fatal(got, f.methods())
	}
}

// After a reconnect the listener reconciles before it accepts deliveries again.
func TestReconnectReconcilesBeforeDelivery(t *testing.T) {
	reconnected := make(chan struct{})
	f := &fakeThread{listDelay: 300 * time.Millisecond, drop: reconnected}
	endpoint := f.serve(t)
	dir, err := os.MkdirTemp("/private/tmp", "otag-ctl-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	_ = os.WriteFile(filepath.Join(dir, "first-turn"), []byte("t0"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "tui-attached"), nil, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k := newController(dir, "s", "/work", turnPolicy(nil))
	stop, err := k.serve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	sc := adapter.Sidecar{Getenv: func(string) string { return dir }, Emit: func(context.Context, string, []byte, string) ([]byte, error) { return nil, nil }}
	target := adapter.Target{HarnessSessionID: "s", ServerURL: "unix://" + filepath.Join(dir, "as.sock")}
	done := make(chan error, 1)
	// First connection: drop it as soon as it is live.
	c1, err := connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	attached := true
	go func() {
		done <- listenRPC(ctx, sc, c1, k, "s", "m", &attached, &threadResult{Thread: thread{ID: "s", Status: status{Type: "idle"}}}, "", "", nil)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "probe", Text: "x"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first connection never accepted deliveries")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.complete()
	k.apply("turn/completed", []byte(`{"threadId":"s","turn":{"id":"t1"}}`))
	c1.close()
	if err := <-done; err == nil {
		t.Fatal("listener ignored the dropped connection")
	}
	if _, err := DeliverReceipt(ctx, target, adapter.Message{ID: "gap", Text: "x"}); reason(err) != ReasonNotReady {
		t.Fatal("delivered without a connection", err)
	}
	ready := true
	go func() { done <- watchServer(ctx, sc, k, endpoint, "s", "m", ready) }()
	<-reconnected // the reconnect initialized; reconciliation is in progress
	var during []string
	for start := time.Now(); time.Since(start) < 150*time.Millisecond; time.Sleep(20 * time.Millisecond) {
		_, err := DeliverReceipt(ctx, target, adapter.Message{ID: fmt.Sprint("r", len(during)), Text: "x"})
		during = append(during, reason(err))
	}
	for _, r := range during {
		if r != ReasonNotReady {
			t.Fatal("delivery accepted before reconciliation", during)
		}
	}
	for {
		r, err := DeliverReceipt(ctx, target, adapter.Message{ID: "after", Text: "[otag#d9] x"})
		if err == nil {
			if r.Kind != "start" || r.TurnID != "t2" {
				t.Fatal(r)
			}
			break
		}
		if reason(err) != ReasonNotReady || ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	methods := f.methods()
	second := slices.Index(methods[1:], "initialize") + 1
	tail := methods[second:]
	if i, j := slices.Index(tail, "thread/turns/list"), slices.Index(tail, "turn/start"); i < 0 || j < i {
		t.Fatal("turn/start before reconciliation", tail)
	}
	cancel()
	<-done
}
