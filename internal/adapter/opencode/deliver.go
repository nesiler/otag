package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// Message delivery (phase 4, docs/MESSAGES.md), against the installed v2 server
// (2.0.25): POST /api/session/<id>/prompt {text, delivery: queue|steer, id: msg_...}
// puts the text in the session inbox (session.inbox.enqueued) and the session takes
// it (session.inbox.delivered, at once when idle, at the next step boundary for a
// steer). Both frames carry data.inboxID, the id otag chose, so the sidecar forwards
// inbox.delivered and ParseEvent reports its inboxID in HookResult.Refs: the receipt
// proof. enqueued is not a proof (the turn may never take it).

// KindInboxDelivered is the SSE type that proves a delivery.
const KindInboxDelivered = "session.inbox.delivered"

// DeliverReceipt implements adapter.ReceiptDeliverer. The inbox id is a fresh msg_ id
// (OpenCode's own form) per attempt; an explicit msg_ Message.ID wins. No session, no
// server password, a refused connection or an HTTP 4xx wrap adapter.ErrNotSent; a
// timeout or 5xx after the request was sent is ambiguous.
func (a *Adapter) DeliverReceipt(ctx context.Context, t adapter.Target, m adapter.Message) (adapter.Receipt, error) {
	if t.HarnessSessionID == "" {
		return adapter.Receipt{}, fmt.Errorf("opencode: run %s has no session: %w", t.RunID, adapter.ErrNotSent)
	}
	c, err := a.targetClient(t)
	if err != nil {
		return adapter.Receipt{}, fmt.Errorf("opencode: %w: %w", err, adapter.ErrNotSent)
	}
	id := m.ID
	if !strings.HasPrefix(id, "msg_") {
		seed := m.Nonce
		if seed == "" {
			seed = m.ID
		}
		id = MessageID(seed, time.Now())
	}
	body := map[string]any{"text": m.Text, "delivery": "queue", "id": id}
	ch := adapter.ChanAPIStart
	if m.Steer {
		body["delivery"], ch = "steer", adapter.ChanAPISteer
	}
	var out json.RawMessage
	if err := c.do(ctx, "POST", "/api/session/"+url.PathEscape(t.HarnessSessionID)+"/prompt", body, &out); err != nil {
		if notSent(err) {
			return adapter.Receipt{}, fmt.Errorf("opencode: %w: %w", err, adapter.ErrNotSent)
		}
		return adapter.Receipt{}, fmt.Errorf("opencode: %w", err)
	}
	if got := inboxIDOf(out); got != "" {
		id = got
	}
	return adapter.Receipt{Channel: ch, Ref: id}, nil
}

// notSent reports an error that happened before the server took the request: a
// refused or failed connection, or a 4xx answer.
func notSent(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status/100 == 4
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

// inboxIDOf finds the inbox id in a prompt answer, if it has one: {"inboxID"} or
// {"id": "msg_..."}, at the top or under "data".
func inboxIDOf(b []byte) string {
	var v struct {
		InboxID string          `json:"inboxID"`
		ID      string          `json:"id"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	switch {
	case strings.HasPrefix(v.InboxID, "msg_"):
		return v.InboxID
	case strings.HasPrefix(v.ID, "msg_"):
		return v.ID
	case len(v.Data) > 0 && v.Data[0] == '{':
		return inboxIDOf(v.Data)
	}
	return ""
}

var idCounter atomic.Uint32

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// MessageID returns an OpenCode message id (msg_ + 12 hex of the ascending time
// part + 14 base62) whose random part derives from seed.
func MessageID(seed string, now time.Time) string {
	v := uint64(now.UnixMilli())*0x1000 + uint64(idCounter.Add(1)&0xfff)
	h := sha256.Sum256([]byte("otag/opencode/" + seed))
	var tail [14]byte
	for i := range tail {
		tail[i] = base62[int(h[i])%len(base62)]
	}
	return fmt.Sprintf("msg_%012x%s", v&0xffffffffffff, tail[:])
}

var _ adapter.ReceiptDeliverer = (*Adapter)(nil)
