package daemon

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/run"
)

// A run with a dispatched delivery is working, whatever its status says: not parked.
func TestParkSkipsDispatchedDelivery(t *testing.T) {
	e := setup(t, parkProfiles)
	tk := e.addTask("which color", "main")
	r := e.start(tk)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	e.ask(r.ID, "which color?", true)
	e.hook(r.ID, "turn_end", `{"text":"asked","reason":"answer"}`)
	err := e.db.Tx(e.ctx, func(tx *sql.Tx) error {
		_, _, err := msg.Send(e.ctx, tx, msg.SendInput{ProjectID: e.get(r.ID).ProjectID, Kind: msg.KindCommand,
			From: msg.Sender{Kind: msg.SenderSystem, Actor: "t"}, TaskID: tk, Body: "keep going",
			To: []msg.Recipient{{Kind: msg.ToRun, Ref: r.ID}}}, msg.Actor{Name: "t"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE message_deliveries SET state = 'dispatched', attempt_nonce = 'n1', channel = 'hook' WHERE run_id = ?`, r.ID)
	_ = e.d.watchAsks(e.ctx, e.snapshot())
	if got := e.get(r.ID); got.Status == run.Ended {
		t.Fatalf("parked with a dispatched delivery: %s", got.EndedReason)
	}
	e.exec(`UPDATE message_deliveries SET state = 'read', read_at = created_at, closed_at = created_at WHERE run_id = ?`, r.ID)
	_ = e.d.watchAsks(e.ctx, e.snapshot())
	if got := e.get(r.ID); got.EndedReason != run.ReasonParked {
		t.Fatalf("not parked once settled: %s %s", got.Status, got.EndedReason)
	}
}

// Review runs are told to give a verdict; a queued reminder does not block the next one.
func TestReviewNudgeAndQueuedNudge(t *testing.T) {
	e := setup(t, parkProfiles)
	tk := e.addTask("work", "main")
	r := e.start(tk)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	e.exec(`UPDATE runs SET role = 'review' WHERE id = ?`, r.ID)
	e.hook(r.ID, "turn_end", `{"text":"stopped","reason":"answer"}`)
	e.clock = time.Now().Add(11 * time.Minute)
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	var body string
	if err := e.db.QueryRow(`SELECT body FROM messages WHERE client_msg_id = 'nudge:' || ? || ':1'`, r.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "otag review") || strings.Contains(body, "otag done") {
		t.Fatalf("review nudge: %s", body)
	}
	// Never delivered, still queued: the second one follows, then the human is told.
	e.clock = time.Now().Add(22 * time.Minute)
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	e.clock = time.Now().Add(33 * time.Minute)
	_ = e.d.watchIdle(e.ctx, e.snapshot())
	if n := len(e.notifications("run.idle")); n != 1 {
		t.Fatalf("%d run.idle notifications", n)
	}
	_, _ = e.m.Stop(e.ctx, r.ID, "test over")
}
