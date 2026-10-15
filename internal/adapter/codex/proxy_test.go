package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTUIProxyPinsPolicyAndConfirmsSubscription(t *testing.T) {
	calls := make(chan map[string]any, 3)
	resumes := make(chan map[string]any, 3)
	endpoint := mockServer(t, func(ctx context.Context, ws *websocket.Conn, msg envelope) {
		if len(msg.ID) == 0 {
			return
		}
		var p map[string]any
		_ = json.Unmarshal(msg.Params, &p)
		result := any(map[string]any{})
		if msg.Method == "thread/resume" {
			resumes <- p
			result = map[string]any{"thread": map[string]any{"id": "s"}}
		}
		if msg.Method == "turn/start" {
			calls <- p
		}
		wsSend(ctx, ws, map[string]any{"id": msg.ID, "result": result})
	})
	dir, err := os.MkdirTemp("/private/tmp", "otag-relay-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	proxy, err := startTUIProxy(ctx, endpoint, dir, "s", "/work", turnPolicy(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	c, err := connect(ctx, "unix://"+filepath.Join(dir, "t.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if _, err := os.Stat(filepath.Join(dir, "tui-attached")); !os.IsNotExist(err) {
		t.Fatal("premature subscription")
	}
	if err := c.call(ctx, "thread/resume", map[string]any{"threadId": "s", "cwd": "/elsewhere", "sandbox": "danger-full-access", "permissions": ":danger-no-sandbox", "runtimeWorkspaceRoots": []string{"/"}}, nil); err != nil {
		t.Fatal(err)
	}
	if r := <-resumes; r["cwd"] != "/work" || r["sandbox"] != nil || r["permissions"] != nil || r["runtimeWorkspaceRoots"] != nil {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(dir, "tui-attached")); err != nil {
		t.Fatal("missing subscription marker", err)
	}
	if err := c.call(ctx, "turn/start", map[string]any{"threadId": "s", "approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"},
		"cwd": "/", "runtimeWorkspaceRoots": []string{"/"}, "environments": []any{map[string]any{"id": "remote"}}}, nil); err != nil {
		t.Fatal(err)
	}
	p := <-calls
	if p["permissions"] != "otag" || p["approvalPolicy"] != "on-request" || p["sandboxPolicy"] != nil || p["cwd"] != "/work" || p["runtimeWorkspaceRoots"] != nil || p["environments"] != nil {
		t.Fatal(p)
	}
	// A request for another thread closes the relay instead of being forwarded.
	if err := c.call(ctx, "turn/start", map[string]any{"threadId": "other"}, nil); err == nil {
		t.Fatal("foreign thread forwarded")
	}
	select {
	case p := <-calls:
		t.Fatal("forwarded", p)
	default:
	}
}
