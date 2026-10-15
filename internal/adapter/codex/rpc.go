package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// rpc multiplexes one app-server connection. A single reader goroutine owns the
// socket: responses go to the waiter of their id, server requests and
// notifications to the dispatch path (observe). No call path reads the socket.
//
// A connection starts queued: incoming messages are kept in arrival order and a
// call dispatches the ones that arrived before its response on the caller's
// goroutine before returning, so a listener that reconciles step by step sees
// the stream in order (as if it read the socket itself). goLive drains the queue
// and lets the reader dispatch from then on; calls made after that (ctl
// deliveries) only wait for their response. Server requests are observed and
// never answered by this second client, except for rejectEarly.
//
// The handler runs on the reader goroutine once live: it must never call (it
// would wait for a response only the reader can deliver).
type rpc struct {
	ws  *websocket.Conn
	seq atomic.Int64

	mu       sync.Mutex
	onEvent  func(envelope)
	waiters  map[string]chan reply
	pending  map[string]envelope // open server requests by id
	live     bool
	queue    []envelope
	queued   int // messages ever queued; a reply's pos counts against it
	err      error
	done     chan struct{}
	dispatch sync.Mutex // serializes observe calls from flushes
	flushed  int
}

type reply struct {
	msg envelope
	pos int
}

// rpcError is an error response of the server: the request was refused.
type rpcError struct {
	method string
	raw    json.RawMessage
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex %s: %s", e.method, e.raw) }

// message is the server's error text.
func (e *rpcError) message() string {
	var v struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(e.raw, &v) == nil && v.Message != "" {
		return v.Message
	}
	return string(e.raw)
}

// errNotSent: the connection was gone before the request was written.
var errNotSent = errors.New("codex connection closed before the request was sent")

func connect(ctx context.Context, endpoint string) (*rpc, error) {
	if !strings.HasPrefix(endpoint, "unix://") {
		return nil, fmt.Errorf("codex endpoint must be unix://")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
	}}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(128 << 20)
	c := newRPC(ws)
	if err := c.call(dialCtx, "initialize", initializeParams(), nil); err != nil {
		c.close()
		return nil, err
	}
	if err := c.send(dialCtx, map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func newRPC(ws *websocket.Conn) *rpc {
	c := &rpc{ws: ws, waiters: map[string]chan reply{}, pending: map[string]envelope{}, done: make(chan struct{})}
	go c.readLoop()
	return c
}

func initializeParams() map[string]any {
	return map[string]any{"clientInfo": map[string]any{"name": "otag", "title": "otag", "version": "0.1"},
		"capabilities": map[string]any{"experimentalApi": true}}
}
func (c *rpc) close() { _ = c.ws.CloseNow() }
func (c *rpc) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// setHandler installs the dispatch callback (see rpc).
func (c *rpc) setHandler(f func(envelope)) {
	c.mu.Lock()
	c.onEvent = f
	c.mu.Unlock()
}

// wait returns when the reader stopped, with its error.
func (c *rpc) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.err
	}
}

func (c *rpc) readLoop() {
	var err error
	defer func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
		_ = c.ws.CloseNow()
	}()
	for {
		var b []byte
		if _, b, err = c.ws.Read(context.Background()); err != nil {
			return
		}
		var msg envelope
		if err = json.Unmarshal(b, &msg); err != nil {
			return
		}
		c.mu.Lock()
		if msg.Method == "" {
			if ch := c.waiters[string(msg.ID)]; ch != nil {
				delete(c.waiters, string(msg.ID))
				ch <- reply{msg: msg, pos: c.queued}
			}
			// A late response (its caller gave up) has nobody to go to.
			c.mu.Unlock()
			continue
		}
		if !c.live {
			c.queue = append(c.queue, msg)
			c.queued++
			c.mu.Unlock()
			continue
		}
		c.mu.Unlock()
		c.observe(msg)
	}
}

// flush dispatches the queued messages that arrived before position pos.
func (c *rpc) flush(pos int) {
	c.dispatch.Lock()
	defer c.dispatch.Unlock()
	for {
		c.mu.Lock()
		if c.live || c.flushed >= pos || len(c.queue) == 0 {
			c.mu.Unlock()
			return
		}
		msg := c.queue[0]
		c.queue = c.queue[1:]
		c.flushed++
		c.mu.Unlock()
		c.observe(msg)
	}
}

// goLive dispatches everything still queued, in order, then lets the reader
// dispatch new messages itself.
func (c *rpc) goLive() {
	c.dispatch.Lock()
	defer c.dispatch.Unlock()
	for {
		c.mu.Lock()
		if c.live {
			c.mu.Unlock()
			return
		}
		if len(c.queue) == 0 {
			c.live = true
			c.mu.Unlock()
			return
		}
		batch := c.queue
		c.queue = nil
		c.flushed += len(batch)
		c.mu.Unlock()
		for _, msg := range batch {
			c.observe(msg)
		}
	}
}

// call sends a request and waits for its response (15 s at most). Errors: a
// *rpcError (refused), errNotSent, or a transport/ctx error after the request was
// written (outcome unknown).
func (c *rpc) call(ctx context.Context, method string, params, result any) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", errNotSent, ctx.Err())
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	seq := c.seq.Add(1)
	id := strconv.FormatInt(seq, 10)
	ch := make(chan reply, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return errNotSent
	default:
	}
	c.waiters[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()
	if err := c.send(ctx, map[string]any{"id": seq, "method": method, "params": params}); err != nil {
		return err
	}
	var r reply
	select {
	case r = <-ch:
	case <-c.done:
		select {
		case r = <-ch:
		default:
			if c.err != nil {
				return fmt.Errorf("codex %s: connection lost: %w", method, c.err)
			}
			return fmt.Errorf("codex %s: connection lost", method)
		}
	case <-ctx.Done():
		return fmt.Errorf("codex %s: %w", method, ctx.Err())
	}
	c.flush(r.pos)
	if len(r.msg.Error) > 0 {
		return &rpcError{method: method, raw: r.msg.Error}
	}
	if result != nil {
		return json.Unmarshal(r.msg.Result, result)
	}
	return nil
}

// openRequests counts the open server requests, of thread sid when given.
func (c *rpc) openRequests(sid ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, msg := range c.pending {
		if len(sid) == 0 || requestThread(msg) == sid[0] {
			n++
		}
	}
	return n
}

func requestThread(msg envelope) string {
	var p payload
	if json.Unmarshal(msg.Params, &p) != nil {
		return ""
	}
	if p.ThreadID != "" {
		return p.ThreadID
	}
	return p.Thread.ID
}
