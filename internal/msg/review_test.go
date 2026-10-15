package msg

import (
	"database/sql"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/task"
)

// A Stop block is proven only by its own continuation; a Stop that is not a
// continuation makes the earlier block unknown.
func TestStopBlockCorrelation(t *testing.T) {
	d, _ := setup(t)
	a := send(t, d, SendInput{Body: "one", To: to(ToRun, "p-1-r1")})
	first := claim(t, d, "p-1-r1", adapter.ChanStopBlock)
	b := send(t, d, SendInput{Body: "two", To: to(ToRun, "p-1-r1")})
	second := claim(t, d, "p-1-r1", adapter.ChanStopBlock)
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Channel: adapter.ChanStopBlock, Nonce: second.Nonce}); len(got) != 1 || got[0] != b.Deliveries[0].ID {
		t.Fatalf("proof of the second block: %v", got)
	}
	if state(t, d, a.Deliveries[0].ID) != Dispatched {
		t.Fatal("the other attempt was proven")
	}
	moved := must(t, d, func(tx *sql.Tx) ([]int64, error) {
		return MarkUnknown(ctx, tx, "p-1-r1", adapter.ChanStopBlock, "", "stop without continuation", actor)
	})
	if len(moved) != 1 || moved[0] != a.Deliveries[0].ID || state(t, d, a.Deliveries[0].ID) != OutcomeUnknown {
		t.Fatalf("unknown %v", moved)
	}
	_ = first
}

// Due lists runs with injected work, least recently served first; ClaimInput.IDs
// restricts a claim.
func TestDueAndClaimIDs(t *testing.T) {
	d, _ := setup(t)
	if due, _ := Due(ctx, d, time.Time{}, 0); len(due) != 0 {
		t.Fatalf("due %v", due)
	}
	m1 := send(t, d, SendInput{Body: "c1", To: to(ToRole, RoleCoordinator)})
	send(t, d, SendInput{Body: "c2", To: to(ToRole, RoleCoordinator)})
	send(t, d, SendInput{Body: "x", To: to(ToRun, "p-1-r1")})
	due, err := Due(ctx, d, time.Time{}, 0)
	if err != nil || len(due) != 2 || due[0] != "p-coord-r1" {
		t.Fatalf("due %v %v", due, err)
	}
	at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-coord-r1", Channel: adapter.ChanModSubmit, IDs: []int64{m1.Deliveries[0].ID + 1}}, actor)
	})
	if at == nil || len(at.Items) != 1 || at.Items[0].Delivery.ID != m1.Deliveries[0].ID+1 {
		t.Fatalf("claim by id %+v", at)
	}
	// The coordinator has an attempt in flight: only p-1-r1 is due.
	if due, _ := Due(ctx, d, time.Time{}, 0); len(due) != 1 || due[0] != "p-1-r1" {
		t.Fatalf("due while in flight %v", due)
	}
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: ChannelProved})
	// Served just now: the coordinator comes after p-1-r1, which was never served.
	if due, _ := Due(ctx, d, time.Time{}, 0); len(due) != 2 || due[0] != "p-1-r1" {
		t.Fatalf("fair order %v", due)
	}
	// Reading never frees the injected slot of an attempt in flight.
	at = claim(t, d, "p-coord-r1", adapter.ChanModSubmit)
	r, _ := ReaderFor(ctx, d, "p-coord-r1")
	read := must(t, d, func(tx *sql.Tx) ([]int64, error) {
		return MarkRead(ctx, tx, []int64{at.Items[0].Delivery.ID}, r, actor)
	})
	if len(read) != 0 || state(t, d, at.Items[0].Delivery.ID) != Dispatched {
		t.Fatalf("read an injected attempt in flight: %v", read)
	}
	items, _ := Inbox(ctx, d, r, false, 0)
	for _, it := range items {
		if it.Delivery.ID == at.Items[0].Delivery.ID {
			t.Fatal("inbox lists an attempt in flight")
		}
	}
}

// An early proof older than the attempt does not prove it.
func TestOldProofNotConsumed(t *testing.T) {
	d, _ := setup(t)
	exec(t, d, `INSERT INTO message_proofs (project_id, run_id, ref, created_at) VALUES (1, 'p-1-r1', 'turn-9', ?)`,
		ts(time.Now().Add(-time.Minute)))
	m := send(t, d, SendInput{Body: "x", To: to(ToRun, "p-1-r1")})
	at := claim(t, d, "p-1-r1", adapter.ChanAPIStart)
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Sent, Receipt: adapter.Receipt{Ref: "turn-9"}})
	if state(t, d, m.Deliveries[0].ID) != Dispatched {
		t.Fatal("a stale proof proved a new attempt")
	}
}

func TestReplyReplayChecksSender(t *testing.T) {
	d, _ := setup(t)
	q := send(t, d, SendInput{Kind: KindQuestion, Body: "?", To: to(ToRun, "p-1-r1")})
	if _, err := reply(t, d, ReplyInput{ClientMsgID: "req:r", From: Sender{Kind: SenderRun, RunID: "p-1-r1"}, To: q.ID, Body: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reply(t, d, ReplyInput{ClientMsgID: "req:r", To: q.ID, Body: "a"}); apperr.Code(err) != apperr.RequestIDReused {
		t.Fatalf("replay by another sender: %v", err)
	}
}

// A waiting task whose answer cannot reach a run shows in needs.
func TestNeedsStuckAnswer(t *testing.T) {
	d, _ := setup(t)
	q, _, err := ask(t, d, AskInput{Body: "?"})
	if err != nil {
		t.Fatal(err)
	}
	ans, err := reply(t, d, ReplyInput{To: q.ID, Body: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := NeedsOf(ctx, d, 1); len(n.Answers) != 0 {
		t.Fatalf("live run: %+v", n.Answers)
	}
	at := claim(t, d, "p-1-r1", adapter.ChanModSubmit)
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Unknown})
	n, _ := NeedsOf(ctx, d, 1)
	if len(n.Answers) != 1 || n.Answers[0].Answer != ans.ID || n.Answers[0].Delivery.State != OutcomeUnknown {
		t.Fatalf("unknown answer: %+v", n.Answers)
	}
	// Parked asker (run status parked, the reserved form): the task can be claimed.
	exec(t, d, `UPDATE runs SET status = 'parked' WHERE id = 'p-1-r1'`)
	if _, err := inTx(t, d, func(tx *sql.Tx) (*task.Task, error) {
		return task.Transition(ctx, tx, "p-1", task.Claimed, "", 0, task.Actor{Name: "daemon"})
	}); err != nil {
		t.Fatalf("claim with a parked run: %v", err)
	}
}
