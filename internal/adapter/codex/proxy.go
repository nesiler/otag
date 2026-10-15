package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/websocket"
)

// A per-run relay observes a successful TUI subscription. The listener can then
// distinguish requests that the TUI can answer from early, unreplayable ones.
// It also pins every TUI turn/start to the run policy, including slash-command
// permission changes and local account defaults: the profile, approvals, the
// working directory and the workspace roots (both "for this turn and subsequent
// turns" in 0.161.0), and execution environments. A TUI thread/resume cannot
// replace the roots or choose a sandbox either.
type tuiProxy struct {
	*http.Server
	cancel context.CancelFunc
}

func (p *tuiProxy) Close() error { p.cancel(); return p.Server.Close() }

func startTUIProxy(ctx context.Context, endpoint, dir, sid, workdir string, policy map[string]any) (*tuiProxy, error) {
	ctx, cancel := context.WithCancel(ctx)
	path := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		cancel()
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
		}}
		defer transport.CloseIdleConnections()
		upstream, _, err := websocket.Dial(r.Context(), "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			http.Error(w, "Codex server unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = upstream.CloseNow() }()
		downstream, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = downstream.CloseNow() }()
		upstream.SetReadLimit(128 << 20)
		downstream.SetReadLimit(128 << 20)
		relayCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		// Channel carries resume IDs to the single upstream reader without data races.
		resumeIDs := make(chan string, 16)
		done := make(chan error, 1)
		go func() {
			defer cancel()
			for {
				typ, b, err := downstream.Read(relayCtx)
				if err != nil {
					done <- err
					return
				}
				var msg envelope
				if json.Unmarshal(b, &msg) == nil {
					if msg.Method == "thread/resume" {
						select {
						case resumeIDs <- string(msg.ID):
						case <-relayCtx.Done():
							done <- relayCtx.Err()
							return
						}
					}
					if msg.Method == "thread/resume" || msg.Method == "turn/start" {
						var err error
						if b, err = pinTUIRequest(msg, sid, workdir, policy); err != nil {
							done <- err
							return
						}
					}
				}
				if err := upstream.Write(relayCtx, typ, b); err != nil {
					done <- err
					return
				}
			}
		}()
		subscribed := false
		defer func() {
			if subscribed {
				_ = os.Remove(filepath.Join(dir, "tui-attached"))
			}
		}()
		ids := map[string]bool{}
		for {
			typ, b, err := upstream.Read(relayCtx)
			if err != nil {
				break
			}
			for len(resumeIDs) > 0 {
				ids[<-resumeIDs] = true
			}
			var msg envelope
			if json.Unmarshal(b, &msg) == nil && ids[string(msg.ID)] && len(msg.Error) == 0 {
				var result threadResult
				if json.Unmarshal(msg.Result, &result) == nil && result.Thread.ID == sid {
					if err := os.WriteFile(filepath.Join(dir, "tui-attached"), nil, 0o600); err != nil {
						break
					}
					subscribed = true
				}
				delete(ids, string(msg.ID))
			}
			if err := downstream.Write(relayCtx, typ, b); err != nil {
				break
			}
		}
		cancel()
		_ = downstream.CloseNow()
		_ = upstream.CloseNow()
		<-done
	})}
	go func() { _ = server.Serve(ln) }()
	go func() { <-ctx.Done(); _ = server.Close() }()
	return &tuiProxy{Server: server, cancel: cancel}, nil
}

// pinTUIRequest rewrites a TUI thread/resume or turn/start to the run policy
// (turnPolicy of the thread).
func pinTUIRequest(msg envelope, sid, workdir string, policy map[string]any) ([]byte, error) {
	var p map[string]any
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, err
	}
	if p == nil || p["threadId"] != sid {
		return nil, fmt.Errorf("TUI attempted a different thread")
	}
	delete(p, "runtimeWorkspaceRoots")
	delete(p, "sandbox")
	delete(p, "sandboxPolicy")
	if msg.Method == "turn/start" {
		delete(p, "permissions")
		delete(p, "approvalsReviewer")
		for k, v := range policy {
			p[k] = v
		}
		p["cwd"] = workdir
		delete(p, "environments")

	} else {
		// Resume attaches to the listener's loaded thread; it only must not widen it.
		delete(p, "permissions")
		if _, ok := p["cwd"]; ok {
			p["cwd"] = workdir
		}
	}
	msg.Params, _ = json.Marshal(p)
	return json.Marshal(msg)
}
