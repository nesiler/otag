package daemon

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/run"
)

func (e *testEnv) send(to msg.Recipient, kind, body string) *msg.Message {
	e.t.Helper()
	var m *msg.Message
	err := e.db.Tx(e.ctx, func(tx *sql.Tx) (err error) {
		m, _, err = msg.Send(e.ctx, tx, msg.SendInput{ProjectID: e.project, Kind: kind, Body: body, To: []msg.Recipient{to},
			From: msg.Sender{Kind: msg.SenderHuman, Actor: "user:test"}}, msg.Actor{Name: "user:test"})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *testEnv) delivery(id int64) *msg.Delivery {
	e.t.Helper()
	d, err := msg.GetDelivery(e.ctx, e.db, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// The daemon delivers a queued message into an idle fake run (its inbox), the run's
// prompt hook shows the marker, and the delivery is proven.
func TestMessagesDispatchIdleRun(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.inbox]
account = "f"
args = ["start", "idle", "inbox:20", "wait"]
`)
	tk := e.addTask("t", "inbox")
	r := e.start(tk)
	e.waitFor("idle", false, func() bool { return e.get(r.ID).Status == run.Idle })
	m := e.send(msg.Recipient{Kind: msg.ToRun, Ref: r.ID}, msg.KindInfo, "reply OK-1")
	id := m.Deliveries[0].ID
	e.waitFor("delivered", true, func() bool { return e.delivery(id).State == msg.Delivered })
	dl := e.delivery(id)
	if *dl.Channel != adapter.ChanAPIStart || *dl.Proof != msg.ProofMarker || dl.Receipt == nil {
		t.Fatalf("%+v", dl)
	}
	b, _ := os.ReadFile(filepath.Join(e.m.Getenv("OTAG_DATA_DIR"), "runs", r.ID, fake.InboxFile+".taken"))
	if len(b) == 0 {
		t.Fatal("the harness never took the message")
	}
}

// A dispatched delivery without proof becomes outcome_unknown after its proof
// timeout and is notified once; a message to the human is notified once.
func TestMessagesProofTimeoutAndNotices(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.idle]
account = "f"
args = ["start", "idle", "wait"]
`)
	tk := e.addTask("t", "idle")
	r := e.start(tk)
	e.waitFor("idle", false, func() bool { return e.get(r.ID).Status == run.Idle })
	m := e.send(msg.Recipient{Kind: msg.ToRun, Ref: r.ID}, msg.KindInfo, "nobody takes this")
	id := m.Deliveries[0].ID
	e.d.Tick(e.ctx)
	if dl := e.delivery(id); dl.State != msg.Dispatched {
		t.Fatalf("not dispatched: %s %v", dl.State, dl.LastError)
	}
	e.clock = e.clock.Add(ProofTimeouts[adapter.ChanAPIStart] + time.Minute)
	e.d.Tick(e.ctx)
	if dl := e.delivery(id); dl.State != msg.OutcomeUnknown {
		t.Fatalf("after the proof timeout: %s", dl.State)
	}
	e.d.Tick(e.ctx)
	if ns := e.notifications("msg.outcome_unknown"); len(ns) != 1 || ns[0].RunID != r.ID {
		t.Fatalf("unknown notices %+v", ns)
	}

	q := e.send(msg.Recipient{Kind: msg.ToHuman, Ref: "human"}, msg.KindQuestion, "which branch?")
	e.d.Tick(e.ctx)
	e.d.Tick(e.ctx)
	ns := e.notifications("msg.human")
	if len(ns) != 1 || ns[0].SentAt == "" {
		t.Fatalf("human notices %+v", ns)
	}
	var mid int64
	_ = e.db.QueryRow(`SELECT message_id FROM notifications WHERE id = ?`, ns[0].ID).Scan(&mid)
	if mid != q.ID {
		t.Fatalf("notice for message %d, want %d", mid, q.ID)
	}
}
