package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/paths"
)

// HeartbeatTimeout is the default Adapter.Heartbeat: OpenCode sends ": heartbeat"
// every 15 s; a stream silent for longer (or a server that sends no response headers
// for that long) is considered dead and reopened.
const HeartbeatTimeout = 45 * time.Second

// Bounds of the sidecar's per-run state.
const (
	maxTree      = 500 // sessions followed (the run's and its subagents')
	maxTreeDepth = 4   // subagent nesting read from the API
)

// forwardAll are the SSE types the sidecar forwards for the run's session and its
// subagent sessions; forwardRoot only for the run's own session (a subagent's turn is
// not the run's turn).
var (
	forwardAll = map[string]bool{
		"permission.asked": true, "permission.replied": true,
		"form.created": true, "form.replied": true, "form.cancelled": true,
		"session.step.started": true, "session.step.ended": true, "session.step.failed": true,
		"session.tool.input.started": true, "session.tool.success": true, "session.tool.failed": true,
		"session.compaction.ended": true, "session.compaction.failed": true,
		"session.retry.scheduled": true,
	}
	forwardRoot = map[string]bool{
		"session.execution.started": true, "session.execution.succeeded": true,
		"session.execution.failed": true, "session.execution.interrupted": true,
		"session.deleted":  true,
		KindInboxDelivered: true,
	}
)

// RunSidecar implements adapter.SidecarRunner: it follows GET /api/event, forwards
// the run's events to the bridge (event id = the frame id, so replays dedupe), and
// after every reconnect reconciles what the stream may have missed.
// Args: --url URL --dir WORKDIR --session ID [--model provider/model] [--resume].
func (a *Adapter) RunSidecar(ctx context.Context, sc adapter.Sidecar, args []string) error {
	fs := flag.NewFlagSet("opencode sidecar", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("url", "", "server URL")
	sid := fs.String("session", "", "session id")
	model := fs.String("model", "", "profile model")
	dir := fs.String("dir", "", "workspace (the server location of the session)")
	resume := fs.Bool("resume", false, "the session is resumed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" || *sid == "" {
		return errors.New("opencode sidecar: --url and --session are required")
	}
	pw := sc.Getenv("OPENCODE_PASSWORD")
	if pw == "" && sc.RunID != "" {
		b, err := os.ReadFile(filepath.Join(paths.RunDir(sc.Getenv, sc.RunID), passwordFile))
		if err != nil {
			return fmt.Errorf("opencode sidecar: no password: %w", err)
		}
		pw = strings.TrimSpace(string(b))
	}
	s := &stream{
		c:     newClient(*base, pw),
		sc:    sc,
		root:  *sid,
		dir:   *dir,
		model: *model,
		tree:  map[string]bool{*sid: true},
		steps: map[string]string{},
		quiet: a.Heartbeat,
		retry: 100 * time.Millisecond,
	}
	if s.quiet <= 0 {
		s.quiet = HeartbeatTimeout
	}
	if *resume {
		s.reason = "resume"
	}
	return s.run(ctx)
}

// stream is the sidecar's state: which sessions belong to the run, and what earlier
// frames said that later ones need (last text, step model).
type stream struct {
	c      *client
	sc     adapter.Sidecar
	root   string
	dir    string // permission and form lists are per location
	model  string
	reason string
	tree   map[string]bool   // the run's session and its subagent sessions
	text   string            // last assistant text of the run's current turn
	steps  map[string]string // session -> model of its current step (cleared when its turn ends)
	quiet  time.Duration     // heartbeat and response-header timeout
	retry  time.Duration     // first Emit retry delay

	ready     bool
	connected int
	lostAt    time.Time // when the last connection ended
}

// errEmit means an event could not be recorded even after retries; the stream is
// reopened so the reconcile restores what was lost.
var errEmit = errors.New("events could not be recorded")

func (s *stream) run(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		before := s.connected
		err := s.follow(ctx)
		if s.connected > before {
			backoff = 500 * time.Millisecond // the server was there: retry soon
			s.lostAt = time.Now()
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			s.log("stream: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
	return nil
}

// follow reads one connection until it ends.
func (s *stream) follow(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := s.c.request(ctx, "GET", "/api/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The body is long-lived, the headers are not: a server that accepts the
	// connection but never answers is given up after s.quiet.
	var timedOut atomic.Bool
	hdr := time.AfterFunc(s.quiet, func() { timedOut.Store(true); cancel() })
	resp, err := (&http.Client{}).Do(req)
	hdr.Stop()
	if err != nil {
		if timedOut.Load() {
			return fmt.Errorf("no response headers within %s", s.quiet)
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET /api/event: HTTP %d", resp.StatusCode)
	}

	// Watchdog: no line (not even a heartbeat) for s.quiet closes the stream.
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	go func() {
		t := time.NewTicker(s.quiet / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > s.quiet {
					s.log("stream silent for %s; reconnecting", s.quiet)
					cancel()
					return
				}
			}
		}
	}()

	r := bufio.NewReaderSize(resp.Body, 64<<10)
	var buf strings.Builder
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			last.Store(time.Now().UnixNano())
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data:"):
			buf.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && buf.Len() > 0:
			ferr := s.frame(ctx, []byte(buf.String()))
			buf.Reset()
			if ferr != nil {
				return ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("stream closed")
			}
			return err
		}
	}
}

// frameHead is what the sidecar reads from a frame to route it.
type frameHead struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		SessionID string `json:"sessionID"`
		ParentID  string `json:"parentID"`
		Text      string `json:"text"`
		Model     *Model `json:"model"`
		Form      *struct {
			SessionID string `json:"sessionID"`
		} `json:"form"`
	} `json:"data"`
}

func (s *stream) frame(ctx context.Context, raw []byte) error {
	var f frameHead
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	if f.Type == "server.connected" {
		return s.connect(ctx)
	}
	sid := f.Data.SessionID
	if sid == "" && f.Data.Form != nil {
		sid = f.Data.Form.SessionID
	}
	if f.Type == "session.created" && s.tree[f.Data.ParentID] && sid != "" {
		s.adopt(sid)
		return nil
	}
	if sid == "" || !s.tree[sid] {
		return nil
	}
	root := sid == s.root
	switch f.Type {
	case "session.text.ended":
		if root {
			s.text = f.Data.Text
		}
		return nil
	case "session.execution.started":
		if root {
			s.text = ""
		}
	case "session.step.started":
		if m := f.Data.Model; m != nil {
			s.steps[sid] = m.ProviderID + "/" + m.ID
		}
	}
	if forwardAll[f.Type] || root && forwardRoot[f.Type] {
		switch f.Type {
		case "session.execution.succeeded":
			raw = withExtra(raw, extra{Text: s.text})
		case "session.step.ended", "session.step.failed", "session.compaction.ended", "session.compaction.failed":
			raw = withExtra(raw, extra{Model: s.modelOf(sid), Sub: !root})
		}
		if err := s.emit(ctx, f.Type, raw, f.ID); err != nil {
			return err
		}
	}
	// Forget what a finished turn or a deleted session no longer needs.
	switch f.Type {
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		delete(s.steps, sid)
	case "session.deleted":
		delete(s.steps, sid)
		if !root {
			delete(s.tree, sid)
		}
	}
	return nil
}

// connect handles server.connected: the first time it reports ready and binds the
// session; every time it re-reads the subagent tree; after a reconnect it reconciles.
func (s *stream) connect(ctx context.Context) error {
	s.connected++
	if !s.ready {
		s.ready = true
		if err := s.sc.Ready(); err != nil {
			s.log("ready: %v", err)
		}
		b, _ := json.Marshal(map[string]string{"sessionID": s.root, "reason": s.startReason()})
		if err := s.emit(ctx, KindSession, b, s.sc.RunID+":"+s.root+":session"); err != nil {
			s.ready = false // bind again on the next connection
			return err
		}
	}
	s.loadTree(ctx)
	if s.connected > 1 {
		return s.reconcile(ctx)
	}
	return nil
}

func (s *stream) modelOf(sid string) string {
	if m := s.steps[sid]; m != "" {
		return m
	}
	return s.model
}

func (s *stream) adopt(sid string) {
	if len(s.tree) < maxTree {
		s.tree[sid] = true
	}
}

// loadTree adds the subagent sessions the server knows (created while the stream was
// down, or before a resume), breadth first, bounded.
func (s *stream) loadTree(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	level := []string{s.root}
	for depth := 0; depth < maxTreeDepth && len(level) > 0; depth++ {
		var next []string
		for _, parent := range level {
			var got struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			q := url.Values{"parentID": {parent}, "limit": {"200"}}
			if err := s.c.do(ctx, "GET", "/api/session?"+q.Encode(), nil, &got); err != nil {
				s.log("session tree: %v", err)
				return
			}
			for _, c := range got.Data {
				if c.ID != "" && !s.tree[c.ID] && len(s.tree) < maxTree {
					s.tree[c.ID] = true
					next = append(next, c.ID)
				}
			}
		}
		level = next
	}
}

func (s *stream) startReason() string {
	if s.reason != "" {
		return s.reason
	}
	return "startup"
}

// withExtra adds the "otag" field to a frame.
func withExtra(raw []byte, x extra) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	b, _ := json.Marshal(x)
	m["otag"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// assistantMessage is the part of an assistant message the reconcile reads.
type assistantMessage struct {
	ID    string `json:"id"`
	Model *Model `json:"model"`
	Time  struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Cost   *float64 `json:"cost"`
	Tokens *tokens  `json:"tokens"`
}

// reconcile reads what the stream may have missed while it was down: OpenCode's
// local server keeps no replayable event log (the durable log needs events.persist,
// which `opencode serve` does not set), so it reads the current state instead:
// usage of the steps finished meanwhile (from the message list, same dedupe key as
// session.step.ended), whether the session still exists, is running, how its last
// turn ended, and pending permissions and forms. The state goes to the bridge as one
// otag.reconcile event, compared there with the run's status.
func (s *stream) reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r := Reconcile{SessionID: s.root}

	var info struct{ Data session }
	if err := s.c.do(ctx, "GET", "/api/session/"+url.PathEscape(s.root), nil, &info); err != nil {
		if !isStatus(err, 404) {
			s.log("reconcile: %v", err)
			return nil
		}
		r.Deleted = true
	}
	r.Outcome = info.Data.Outcome
	if !r.Deleted {
		if err := s.missedUsage(ctx); err != nil {
			return err
		}
		var active struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := s.c.do(ctx, "GET", "/api/session/active", nil, &active); err != nil {
			s.log("reconcile: %v", err)
			return nil
		}
		_, r.Active = active.Data[s.root]
		var perms struct {
			Data []permissionRequest `json:"data"`
		}
		if err := s.c.do(ctx, "GET", "/api/permission/request"+s.where(), nil, &perms); err != nil {
			s.log("reconcile: %v", err)
			return nil
		}
		for _, p := range perms.Data {
			if s.tree[p.SessionID] {
				r.Permissions = append(r.Permissions, p)
			}
		}
		var forms struct {
			Data []form `json:"data"`
		}
		if err := s.c.do(ctx, "GET", "/api/form"+s.where(), nil, &forms); err != nil {
			s.log("reconcile: %v", err)
			return nil
		}
		for _, f := range forms.Data {
			if s.tree[f.SessionID] {
				r.Forms = append(r.Forms, f)
			}
		}
	}
	b, _ := json.Marshal(r)
	return s.emit(ctx, KindReconcile, b, "")
}

// missedUsage emits usage for assistant messages completed since the connection was
// lost (a little slack for clock skew), as step.ended frames keyed like the live ones.
func (s *stream) missedUsage(ctx context.Context) error {
	since := s.lostAt.Add(-5 * time.Second).UnixMilli()
	for sid := range s.tree {
		var got struct {
			Data []assistantMessage `json:"data"`
		}
		q := url.Values{"type": {"assistant"}, "order": {"desc"}, "limit": {"50"}}
		if err := s.c.do(ctx, "GET", "/api/session/"+url.PathEscape(sid)+"/message?"+q.Encode(), nil, &got); err != nil {
			s.log("reconcile usage %s: %v", sid, err)
			continue
		}
		for _, m := range got.Data {
			if m.Tokens == nil || m.Time.Completed == 0 || m.Time.Completed < since {
				continue
			}
			model := s.modelOf(sid)
			if m.Model != nil {
				model = m.Model.ProviderID + "/" + m.Model.ID
			}
			frame, _ := json.Marshal(map[string]any{
				"type": "session.step.ended",
				"data": map[string]any{"sessionID": sid, "assistantMessageID": m.ID, "cost": m.Cost, "tokens": m.Tokens},
				"otag": extra{Model: model},
			})
			if err := s.emit(ctx, "session.step.ended", frame, "usage:"+m.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *stream) where() string {
	if s.dir == "" {
		return ""
	}
	return locationQuery(s.dir)
}

// emit records one event, retrying in order (the stream waits) when the bridge
// fails, e.g. on a locked database. After the retries the stream is reopened
// (errEmit), so the reconcile restores state that event would have changed.
func (s *stream) emit(ctx context.Context, kind string, payload []byte, id string) error {
	delay := s.retry
	for attempt := 1; ; attempt++ {
		_, err := s.sc.Emit(ctx, kind, payload, id)
		if err == nil {
			return nil
		}
		if attempt == 5 || ctx.Err() != nil {
			s.log("emit %s %s: %v (giving up)", kind, id, err)
			return errEmit
		}
		s.log("emit %s %s: %v (retrying)", kind, id, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// log goes to the sidecar window's stderr (Emit errors also reach the hook log).
func (s *stream) log(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "otag opencode sidecar %s: %s\n", time.Now().Format(time.TimeOnly), fmt.Sprintf(format, args...))
}
