package opencode

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

func TestDeliverReceipt(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	a.Getenv = targetEnv(t, "al-1-r1")
	f, srv := newFakeServer(t, spec)
	tg := adapter.Target{RunID: "al-1-r1", HarnessSessionID: "ses_a", ServerURL: srv.URL}
	rc, err := a.DeliverReceipt(context.Background(), tg, adapter.Message{ID: "n1", Nonce: "n1", Text: "[otag#d1] hi"})
	if err != nil || rc.Channel != adapter.ChanAPIStart || rc.Ref != "msg_1" {
		t.Fatalf("%+v %v", rc, err)
	}
	b := f.bodies["POST /api/session/ses_a/prompt"]
	if id, _ := b["id"].(string); !regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(id) || b["delivery"] != "queue" {
		t.Fatalf("body %v", b)
	}
	rc, err = a.DeliverReceipt(context.Background(), tg, adapter.Message{Nonce: "n2", Text: "x", Steer: true})
	if err != nil || rc.Channel != adapter.ChanAPISteer || f.bodies["POST /api/session/ses_a/prompt"]["delivery"] != "steer" {
		t.Fatalf("steer %+v %v", rc, err)
	}
	// Unknown session (404 from the server), no session, no password: not sent.
	for _, tg := range []adapter.Target{
		{RunID: "al-1-r1", HarnessSessionID: "ses_zzz", ServerURL: srv.URL},
		{RunID: "al-1-r1", ServerURL: srv.URL},
		{RunID: "nope", HarnessSessionID: "ses_a", ServerURL: srv.URL},
		{RunID: "al-1-r1", HarnessSessionID: "ses_a", ServerURL: "http://127.0.0.1:1"},
	} {
		if _, err := a.DeliverReceipt(context.Background(), tg, adapter.Message{Nonce: "n3", Text: "x"}); !errors.Is(err, adapter.ErrNotSent) {
			t.Errorf("%+v: %v", tg, err)
		}
	}
}

func TestInboxDeliveredIsProof(t *testing.T) {
	a := New()
	res, err := a.ParseEvent(adapter.HookContext{}, KindInboxDelivered,
		[]byte(`{"id":"evt_1","type":"session.inbox.delivered","data":{"sessionID":"ses_a","inboxID":"msg_x"}}`))
	if err != nil || len(res.Events) != 0 || len(res.Refs) != 1 || res.Refs[0] != "msg_x" {
		t.Fatalf("%+v %v", res, err)
	}
	res, _ = a.ParseEvent(adapter.HookContext{}, "session.inbox.enqueued",
		[]byte(`{"id":"evt_2","type":"session.inbox.enqueued","data":{"sessionID":"ses_a","inboxID":"msg_x"}}`))
	if len(res.Refs) != 0 {
		t.Fatal("enqueued is no proof")
	}
	if !forwardRoot[KindInboxDelivered] {
		t.Fatal("the sidecar must forward inbox.delivered")
	}
}

func TestMessageIDAscending(t *testing.T) {
	now := time.UnixMilli(1791655053849)
	a, b := MessageID("s", now), MessageID("s", now.Add(time.Millisecond))
	if !strings.HasPrefix(a, "msg_") || len(a) != 30 || a >= b {
		t.Fatalf("%s %s", a, b)
	}
}
