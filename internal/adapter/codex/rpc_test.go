package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nesiler/otag/internal/adapter"
)

func mockServer(t *testing.T, handle func(context.Context, *websocket.Conn, envelope)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/private/tmp", "otag-codex-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "as.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		for {
			_, raw, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var msg envelope
			if json.Unmarshal(raw, &msg) != nil {
				return
			}
			handle(r.Context(), ws, msg)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + sock
}
func wsSend(ctx context.Context, ws *websocket.Conn, msg any) {
	b, _ := json.Marshal(msg)
	_ = ws.Write(ctx, websocket.MessageText, b)
}

func TestSetupAndSidecar(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			requests := make(chan envelope, 20)
			conflicted := false
			endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
				if len(msg.ID) == 0 {
					return
				}
				requests <- msg
				if msg.Method == "thread/resume" && !conflicted {
					conflicted = true
					wsSend(ctx, ws, map[string]any{"id": msg.ID, "error": map[string]any{"code": -32600, "message": "thread chosen already has an active writer"}})
					return
				}
				result := any(map[string]any{})
				switch msg.Method {
				case "thread/start", "thread/resume":
					result = map[string]any{"thread": map[string]any{"id": "chosen", "path": "rollout", "status": map[string]any{"type": "idle"}}}
				}
				wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": result})
				if msg.Method == "turn/start" {
					wsSend(ctx, ws, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "other", "turn": map[string]any{"id": "wrong"}}})
					wsSend(ctx, ws, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "chosen", "turn": map[string]any{"id": "right"}}})
					wsSend(ctx, ws, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "chosen", "turn": map[string]any{"id": "right", "status": "completed"}}})
				}
			})
			dir := t.TempDir()
			spec := adapter.RunSpec{Harness: "codex", RunDir: dir, OtagBin: "otag", Prompt: "hello", Instructions: "rules", Workdir: "/workspace", Profile: adapter.Profile{Model: "gpt-6.1-sol", Effort: "low"}}
			if resume {
				spec.ResumeSessionID = "chosen"
			}
			ls := &adapter.LaunchSpec{Argv: []string{"codex", "--remote", endpoint}, Server: &adapter.Process{URL: endpoint}}
			if err := New().SetupServer(context.Background(), spec, ls); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 0 || ls.HarnessSessionID != "" || !slices.Contains(ls.Argv, filepath.Join(dir, "tui.sh")) || len(ls.Sidecars) != 1 {
				t.Fatal("setup must defer thread creation to its owning listener", ls)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ready := false
			seen := []string{}
			sc := adapter.Sidecar{Getenv: func(k string) string { return ls.Sidecars[0].Env[k] }, Ready: func() error {
				b, err := os.ReadFile(filepath.Join(dir, "thread-id"))
				if err != nil || string(b) != "chosen\n" {
					t.Fatalf("thread id must be published before ready: %s %v", b, err)
				}
				wantRequests := 2
				if resume {
					wantRequests++ // one transient writer conflict before the retry
				}
				if len(requests) != wantRequests {
					t.Fatal("inference started before pane startup")
				}
				ready = true
				return os.WriteFile(filepath.Join(dir, "tui-ready"), nil, 0o600)
			}, Emit: func(_ context.Context, kind string, b []byte, _ string) ([]byte, error) {
				var p payload
				_ = json.Unmarshal(b, &p)
				if p.ThreadID == "other" {
					t.Error("unrelated thread emitted")
				}
				if kind == "SessionStart" && (p.Source == "resume") != resume {
					t.Error("wrong session source", p.Source)
				}
				seen = append(seen, kind)
				if kind == "turn/started" && !ready {
					t.Error("turn before ready")
				}
				if kind == "turn/completed" {
					cancel()
				}
				return nil, nil
			}}
			argv := ls.Sidecars[0].Argv
			if err := New().RunSidecar(ctx, sc, argv[slices.Index(argv, "--")+1:]); err != nil {
				t.Fatal(err)
			}
			if !ready || strings.Join(seen, ",") != "SessionStart,thread/status/changed,turn/started,turn/completed" {
				t.Fatal(seen)
			}
			if _, err := os.Stat(filepath.Join(dir, "turn-ready")); err != nil {
				t.Fatal("TUI handoff was not released", err)
			}
			for len(requests) > 0 {
				msg := <-requests
				var params map[string]any
				_ = json.Unmarshal(msg.Params, &params)
				if msg.Method == "thread/start" || msg.Method == "thread/resume" {
					if params["developerInstructions"] != "rules" || params["approvalPolicy"] != "on-request" || params["permissions"] != "otag" || (msg.Method == "thread/resume") != resume || (params["excludeTurns"] == true) != resume {
						t.Fatal(msg.Method, params)
					}
				}
				if msg.Method == "turn/start" && (params["threadId"] != "chosen" || params["approvalPolicy"] != "on-request" || params["permissions"] != "otag") {
					t.Fatal(params)
				}
			}
		})
	}
}

func TestReconnectReconcilesCompletedTurn(t *testing.T) {
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if len(msg.ID) == 0 {
			return
		}
		result := any(map[string]any{})
		if msg.Method == "thread/resume" {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			if p["excludeTurns"] != true {
				t.Error("reconnect resume hydrates history", p)
			}
			result = map[string]any{"thread": map[string]any{"id": "s", "status": map[string]any{"type": "idle"}}}
		}
		if msg.Method == "thread/turns/list" {
			result = map[string]any{"data": []any{map[string]any{"id": "missed", "status": "failed", "error": map[string]any{"message": "failure"}}}}
		}
		wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": result})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := true
	var completed bool
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "first-turn"), []byte("missed"), 0o600)
	err := listen(ctx, adapter.Sidecar{Getenv: func(string) string { return dir }, Emit: func(_ context.Context, kind string, b []byte, _ string) ([]byte, error) {
		if kind == "turn/completed" {
			r, e := New().ParseEvent(adapter.HookContext{}, kind, b)
			if e != nil || len(r.Events) != 2 || r.Events[1].Reason != "error" {
				t.Error(r, e)
			}
			completed = true
			cancel()
		}
		return nil, nil
	}}, nil, endpoint, "s", "model", &ready)
	if !completed || err == nil {
		t.Fatal(completed, err)
	}
}

func TestEarlyServerRequestResolved(t *testing.T) {
	responses := make(chan envelope, 2)
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if msg.Method == "" {
			responses <- msg
			return
		}
		if len(msg.ID) == 0 {
			return
		}
		if msg.Method == "turn/start" {
			wsSend(ctx, ws, map[string]any{"id": 99, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "s", "turnId": "t"}})
		}
		wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": map[string]any{}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err := c.call(ctx, "turn/start", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if c.openRequests() != 1 {
		t.Fatal("lost early request")
	}
	if err := c.rejectEarly(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-responses:
		if string(msg.ID) != "99" || !strings.Contains(string(msg.Error), "not attached") {
			t.Fatal(msg)
		}
	case <-ctx.Done():
		t.Fatal("unanswered early request")
	}
	if c.openRequests() != 0 {
		t.Fatal("request stayed open")
	}
	// Once the TUI is attached, requests remain owned by it until resolved.
	c.observe(envelope{ID: json.RawMessage(`"r"`), Method: "item/tool/requestUserInput"})
	c.observe(envelope{Method: "serverRequest/resolved", Params: json.RawMessage(`{"requestId":"r","threadId":"s"}`)})
	if c.openRequests() != 0 {
		t.Fatal("request stayed open after resolution")
	}
}

func TestReconcileAllPagesAndUsage(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "first-turn"), []byte("first"), 0o600)
	rollout := filepath.Join(root, "rollout")
	_ = os.WriteFile(rollout, []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"old"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"first"}}`+"\n"+`{"type":"turn_context","payload":{"model":"actual"}}`+"\n"+`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2,"total_tokens":12},"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2,"total_tokens":12}}}}`+"\n"), 0o600)
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if len(msg.ID) == 0 {
			return
		}
		result := any(map[string]any{})
		if msg.Method == "thread/turns/list" {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			if p["cursor"] == nil {
				result = map[string]any{"data": []any{map[string]any{"id": "old", "status": "completed"}, map[string]any{"id": "first", "status": "completed"}}, "nextCursor": "next"}
			} else {
				result = map[string]any{"data": []any{map[string]any{"id": "second", "status": "failed"}}}
			}
		}
		wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": result})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	sc := adapter.Sidecar{Getenv: func(string) string { return root }}
	var ids []string
	var usage *adapter.UsageData
	emit := func(kind string, p any) {
		b, _ := json.Marshal(p)
		var data payload
		_ = json.Unmarshal(b, &data)
		if kind == "turn/completed" {
			ids = append(ids, data.Turn.ID)
		}
		if kind == "thread/tokenUsage/updated" {
			r, e := New().ParseEvent(adapter.HookContext{}, kind, b)
			if e != nil {
				t.Fatal(e)
			}
			usage = r.Events[0].Usage
		}
	}
	if err := reconcileTurns(ctx, c, sc, "s", emit); err != nil {
		t.Fatal(err)
	}
	if err := reconcileUsage(ctx, sc, rollout, "s", "wrong", emit); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "first,second" || usage == nil || usage.InputTokens != 6 || usage.Model != "actual" {
		t.Fatal(ids, usage)
	}
	// No anchor: this launch has not started a turn, so history is not replayed.
	ids, usage = nil, nil
	_ = os.Remove(filepath.Join(root, "first-turn"))
	if err := reconcileTurns(ctx, c, sc, "s", emit); err != nil {
		t.Fatal(err)
	}
	if err := reconcileUsage(ctx, sc, rollout, "s", "wrong", emit); err != nil {
		t.Fatal(err)
	}
	if ids != nil || usage != nil {
		t.Fatal("replayed without anchor", ids, usage)
	}
}

func TestRPCResponseLargerThanOldLimit(t *testing.T) {
	text := strings.Repeat("x", 17<<20)
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if len(msg.ID) > 0 {
			result := any(map[string]any{})
			if msg.Method == "thread/read" {
				result = map[string]any{"data": text}
			}
			wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": result})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	var result struct {
		Data string `json:"data"`
	}
	if err := c.call(ctx, "thread/read", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != len(text) {
		t.Fatal("truncated response")
	}
}
