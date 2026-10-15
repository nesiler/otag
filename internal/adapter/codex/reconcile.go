package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/nesiler/otag/internal/adapter"
)

func firstTurn(sc adapter.Sidecar) string {
	if sc.Getenv == nil {
		return ""
	}
	dir := sc.Getenv("OTAG_CODEX_RUN_DIR")
	if dir == "" {
		return ""
	}
	b, _ := os.ReadFile(filepath.Join(dir, "first-turn"))
	return strings.TrimSpace(string(b))
}
func rememberTurn(sc adapter.Sidecar, id string) {
	if sc.Getenv == nil || id == "" {
		return
	}
	dir := sc.Getenv("OTAG_CODEX_RUN_DIR")
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "first-turn"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.WriteString(id)
		_ = f.Close()
	}
}

// Reconcile every turn since this launch's first turn, across bounded pages.
// Metadata-only resume avoids hydrating a potentially enormous thread twice.
// Without an anchor this launch has not started a turn yet: replaying the thread
// would report every historical turn (of a resumed session) as this run's.
func reconcileTurns(ctx context.Context, c *rpc, sc adapter.Sidecar, sid string, emit func(string, any)) error {
	anchor := firstTurn(sc)
	if anchor == "" {
		return nil
	}
	active := false
	cursor := ""
	for {
		var page struct {
			Data []turn `json:"data"`
			Next string `json:"nextCursor"`
		}
		params := map[string]any{"threadId": sid, "limit": 50, "itemsView": "full", "sortDirection": "asc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.call(ctx, "thread/turns/list", params, &page); err != nil {
			return err
		}
		for _, t := range page.Data {
			if t.ID == anchor {
				active = true
			}
			if !active {
				continue
			}
			kind := "turn/completed"
			if t.Status == "inProgress" {
				kind = "turn/started"
			}
			emit(kind, map[string]any{"threadId": sid, "turn": t})
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == cursor {
			return nil
		}
		cursor = page.Next
	}
}

// RPC turn pages omit token usage. The exact thread's persisted rollout supplies
// missed measurements using the same cumulative-counter dedupe keys as RPC.
func reconcileUsage(ctx context.Context, sc adapter.Sidecar, path, sid, model string, emit func(string, any)) error {
	anchor := firstTurn(sc)
	if anchor == "" {
		return nil // see reconcileTurns
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	active := false
	current := ""
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 128<<20)
	for scan.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw := scan.Bytes()
		if m := rolloutModel(raw); m != "" {
			model = m
		}
		kind, p, tid := rolloutEvent(raw, sid, current, model)
		if tid != "" {
			current = tid
		}
		if tid == anchor {
			active = true
		}
		if active && kind == "thread/tokenUsage/updated" {
			emit(kind, p)
		}
	}
	return scan.Err()
}

// Server requests received while the initial turn is being started cannot have
// reached the not-yet-attached TUI. Resolve them as errors so the tool can retry
// after attachment, instead of leaving an unanswerable approval/question hanging.
func (c *rpc) rejectEarly(ctx context.Context, sid ...string) error {
	c.mu.Lock()
	open := make(map[string]envelope, len(c.pending))
	for key, msg := range c.pending {
		if len(sid) == 0 || requestThread(msg) == sid[0] {
			open[key] = msg
		}
	}
	c.mu.Unlock()
	for key, msg := range open {
		if err := c.send(ctx, map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "otag: TUI is not attached yet; retry this request after attachment"}}); err != nil {
			return err
		}
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}
	return nil
}

// observe tracks open server requests and hands msg to the handler. It runs on
// the reader goroutine (live) or in a caller's flush, one message at a time.
func (c *rpc) observe(msg envelope) {
	c.mu.Lock()
	if len(msg.ID) > 0 && msg.Method != "" {
		c.pending[string(msg.ID)] = msg
	}
	if msg.Method == "serverRequest/resolved" {
		var p struct {
			ID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			delete(c.pending, string(p.ID))
		}
	}
	onEvent := c.onEvent
	c.mu.Unlock()
	if onEvent != nil {
		onEvent(msg)
	}
}
