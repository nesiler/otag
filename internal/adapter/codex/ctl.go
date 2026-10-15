package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ctlSocket is the listener's control socket in RunDir. It is shorter than
// as.sock, so Prepare's sun_path check covers it too.
const ctlSocket = "c.sock"

// Delivery refusal reasons of the control protocol (DeliveryError.Reason).
const (
	ReasonNotReady      = "not_ready"      // listener not connected/reconciled, or TUI not attached
	ReasonWaitingDialog = "waiting_dialog" // an approval or question is open
	ReasonBusy          = "busy"           // a turn is running and steer was not allowed
	ReasonBusyMismatch  = "busy_mismatch"  // steer refused: the expected turn is no longer active
	ReasonRejected      = "rejected"       // Codex refused the request
	ReasonUnknown       = "unknown"        // sent, but no answer: it may have been delivered
	ReasonBadRequest    = "bad_request"
)

// ctlRequest is one line of the control protocol:
//
//	{"op":"deliver","attempt":"<nonce>","thread":"<thread id>","text":"...","steer":true}
//
// thread is optional; when set it must be the listener's thread.
type ctlRequest struct {
	Op      string `json:"op"`
	Attempt string `json:"attempt"`
	Thread  string `json:"thread,omitempty"`
	Text    string `json:"text"`
	Steer   bool   `json:"steer,omitempty"`
}

// ctlResponse is the one-line answer:
//
//	{"ok":true,"kind":"start|steer","turn_id":"..."}
//	{"ok":false,"reason":"busy|busy_mismatch|waiting_dialog|not_ready|rejected|unknown|bad_request","error":"..."}
type ctlResponse struct {
	OK     bool   `json:"ok"`
	Kind   string `json:"kind,omitempty"`
	TurnID string `json:"turn_id,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ctlDeadline bounds one delivery inside the listener; clients wait longer.
const ctlDeadline = 10 * time.Second

// ctlMaxLine bounds a request line; ctlMemory bounds the remembered completed
// turns and attempt outcomes (memory only: a restarted listener forgets them).
const (
	ctlMaxLine = 1 << 20
	ctlMemory  = 256
)

// controller serves the control socket for one thread across reconnects. The
// listener keeps its view of the thread current (apply) and attaches the
// connection only once it is reconciled and live.
type controller struct {
	dir, sid, workdir string
	policy            map[string]any

	serial sync.Mutex // one delivery at a time

	mu       sync.Mutex
	c        *rpc
	status   string
	flags    []string
	active   string
	ended    map[string]bool // completed turns, the last ctlMemory (endOrder, FIFO)
	endOrder []string
	attempts map[string]ctlResponse // sent outcomes by attempt nonce
	order    []string
}

func newController(dir, sid, workdir string, policy map[string]any) *controller {
	return &controller{dir: dir, sid: sid, workdir: workdir, policy: policy, ended: map[string]bool{}, attempts: map[string]ctlResponse{}}
}

// serve listens on dir/c.sock (0600) until ctx ends.
func (k *controller) serve(ctx context.Context) (func(), error) {
	// The socket exists briefly before its chmod: only a private directory makes
	// that window harmless (otag creates RunDir 0700; enforce it).
	if info, err := os.Stat(k.dir); err != nil {
		return nil, err
	} else if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(k.dir, 0o700); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(k.dir, ctlSocket)
	_ = os.Remove(path) // a crashed predecessor's socket
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go k.handle(ctx, conn)
		}
	}()
	return cancel, nil
}

func (k *controller) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(ctlDeadline + 5*time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, ctlMaxLine)).ReadBytes('\n')
	var resp ctlResponse
	var req ctlRequest
	if err != nil {
		// Only a complete line is a request; say so, so the caller knows nothing was done.
		resp = ctlResponse{Reason: ReasonBadRequest, Error: "incomplete or oversized request line"}
	} else if err := json.Unmarshal(line, &req); err != nil {
		resp = ctlResponse{Reason: ReasonBadRequest, Error: "invalid request: " + err.Error()}
	} else {
		dctx, cancel := context.WithTimeout(ctx, ctlDeadline)
		resp = k.deliver(dctx, req)
		cancel()
	}
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
}

// reset forgets the live view before a (re)connection rebuilds it.
func (k *controller) reset() {
	k.mu.Lock()
	k.status, k.flags, k.active = "", nil, ""
	k.mu.Unlock()
}

// attach accepts deliveries over c; detach stops them (only if c is attached).
func (k *controller) attach(c *rpc) { k.mu.Lock(); k.c = c; k.mu.Unlock() }
func (k *controller) detach(c *rpc) {
	k.mu.Lock()
	if k.c == c {
		k.c = nil
	}
	k.mu.Unlock()
}

// apply updates the view of the thread from a notification (or a reconciled one).
func (k *controller) apply(method string, params []byte) {
	var p payload
	if json.Unmarshal(params, &p) != nil {
		return
	}
	thread := p.ThreadID
	if thread == "" {
		thread = p.Thread.ID
	}
	if thread != k.sid {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	switch method {
	case "turn/started":
		k.started(p.Turn.ID)
	case "turn/completed":
		if p.Turn.ID != "" && !k.ended[p.Turn.ID] {
			// Evict the oldest only: a late turn/started of a recently completed
			// turn must still be ignored after many completions.
			k.ended[p.Turn.ID] = true
			k.endOrder = append(k.endOrder, p.Turn.ID)
			if len(k.endOrder) > ctlMemory {
				delete(k.ended, k.endOrder[0])
				k.endOrder = k.endOrder[1:]
			}
		}
		if k.active == p.Turn.ID {
			k.active = ""
		}
	case "thread/status/changed":
		k.status, k.flags = p.Status.Type, p.Status.Flags
	}
}

// begin records a turn this client started (its response may precede turn/started).
func (k *controller) begin(id string) {
	k.mu.Lock()
	k.started(id)
	k.mu.Unlock()
}

// started records id as the active turn unless it already completed. k.mu held.
func (k *controller) started(id string) {
	if id != "" && !k.ended[id] {
		k.active = id
	}
}

func (k *controller) deliver(ctx context.Context, req ctlRequest) ctlResponse {
	switch {
	case req.Op != "deliver":
		return ctlResponse{Reason: ReasonBadRequest, Error: "unknown op " + req.Op}
	case req.Attempt == "" || strings.TrimSpace(req.Text) == "":
		return ctlResponse{Reason: ReasonBadRequest, Error: "attempt and text are required"}
	case req.Thread != "" && req.Thread != k.sid:
		return ctlResponse{Reason: ReasonBadRequest, Error: "this listener serves another thread"}
	}
	k.serial.Lock()
	defer k.serial.Unlock()
	if ctx.Err() != nil {
		return ctlResponse{Reason: ReasonNotReady, Error: "timed out behind another delivery"}
	}
	k.mu.Lock()
	if prior, ok := k.attempts[req.Attempt]; ok {
		k.mu.Unlock()
		return prior // never inject one attempt twice
	}
	c, status, flags, active := k.c, k.status, k.flags, k.active
	k.mu.Unlock()
	if c == nil {
		return ctlResponse{Reason: ReasonNotReady, Error: "the Codex listener is not connected"}
	}
	if _, err := os.Stat(filepath.Join(k.dir, "tui-attached")); err != nil {
		return ctlResponse{Reason: ReasonNotReady, Error: "the Codex TUI is not attached"}
	}
	for _, f := range flags {
		if f == "waitingOnApproval" || f == "waitingOnUserInput" {
			return ctlResponse{Reason: ReasonWaitingDialog, Error: "Codex is waiting on " + strings.TrimPrefix(f, "waitingOn")}
		}
	}
	if c.openRequests(k.sid) > 0 {
		return ctlResponse{Reason: ReasonWaitingDialog, Error: "an approval or question is open"}
	}
	if active == "" && status == "active" {
		// Active without a known turn (a reconnect before this launch's first
		// turn, or a stale status): ask for the latest turn.
		var page struct {
			Data []turn `json:"data"`
		}
		if err := c.call(ctx, "thread/turns/list", map[string]any{"threadId": k.sid, "limit": 1, "sortDirection": "desc"}, &page); err != nil {
			return ctlResponse{Reason: ReasonNotReady, Error: err.Error()}
		}
		if len(page.Data) > 0 && page.Data[0].Status == "inProgress" {
			active = page.Data[0].ID
		}
	}
	input := []any{map[string]any{"type": "text", "text": req.Text, "text_elements": []any{}}}
	clientID := "otag-" + req.Attempt
	if active == "" {
		// The same pins as the relay applies to TUI turns (turnPolicy, cwd).
		params := map[string]any{"threadId": k.sid, "input": input, "clientUserMessageId": clientID}
		for key, v := range k.policy {
			params[key] = v
		}
		if k.workdir != "" {
			params["cwd"] = k.workdir
		}
		var res struct {
			Turn turn `json:"turn"`
		}
		err := c.call(ctx, "turn/start", params, &res)
		if err == nil && res.Turn.ID == "" {
			return k.record(req.Attempt, ctlResponse{Reason: ReasonUnknown, Error: "codex turn/start returned no turn id"})
		}
		if err == nil {
			// No rememberTurn here: the reconcile anchor is the launch's first turn,
			// set (O_EXCL) by the listener's initial turn before ctl attaches, and
			// this turn's turn/started passes through the listener's handler too.
			k.begin(res.Turn.ID)
		}
		return k.outcome(req.Attempt, err, "start", res.Turn.ID, false)
	}
	if !req.Steer {
		return ctlResponse{Reason: ReasonBusy, Error: "turn " + active + " is running"}
	}
	params := map[string]any{"threadId": k.sid, "expectedTurnId": active, "input": input, "clientUserMessageId": clientID}
	var res struct {
		TurnID string `json:"turnId"`
	}
	err := c.call(ctx, "turn/steer", params, &res)
	if err == nil && res.TurnID == "" {
		res.TurnID = active
	}
	return k.outcome(req.Attempt, err, "steer", res.TurnID, true)
}

// outcome maps a call result. A refused steer is a mismatch: it never falls back
// to starting a turn. Delivered and unknown outcomes are remembered per attempt.
func (k *controller) outcome(attempt string, err error, kind, turnID string, steer bool) ctlResponse {
	var refused *rpcError
	switch {
	case err == nil:
		return k.record(attempt, ctlResponse{OK: true, Kind: kind, TurnID: turnID})
	case errors.As(err, &refused):
		// 0.162.1: "expected active turn id `A` but found `B`", "no active turn to steer".
		if text := refused.message(); steer && (strings.Contains(text, "expected active turn") || strings.Contains(text, "no active turn")) {
			return ctlResponse{Reason: ReasonBusyMismatch, Error: text}
		}
		return ctlResponse{Reason: ReasonRejected, Error: refused.message()}
	case errors.Is(err, errNotSent):
		return ctlResponse{Reason: ReasonNotReady, Error: err.Error()}
	default:
		return k.record(attempt, ctlResponse{Reason: ReasonUnknown, Error: err.Error()})
	}
}

func (k *controller) record(attempt string, r ctlResponse) ctlResponse {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.attempts[attempt] = r
	k.order = append(k.order, attempt)
	if len(k.order) > ctlMemory {
		delete(k.attempts, k.order[0])
		k.order = k.order[1:]
	}
	return r
}
