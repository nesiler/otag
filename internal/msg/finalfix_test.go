package msg

import (
	"database/sql"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
)

// Take selects and marks in one transaction: an entry a dispatcher claimed after the
// reader looked is not returned (the agent would see it twice).
func TestTakeSkipsClaimedEntries(t *testing.T) {
	d, _ := setup(t)
	a := send(t, d, SendInput{Body: "one", To: to(ToRun, "p-1-r1")})
	r, _ := ReaderFor(ctx, d, "p-1-r1")
	if seen, _ := Inbox(ctx, d, r, false, 0); len(seen) != 1 {
		t.Fatalf("look: %d", len(seen))
	}
	at := claim(t, d, "p-1-r1", adapter.ChanModSubmit) // the dispatcher wins the race
	if at == nil {
		t.Fatal("no claim")
	}
	b := send(t, d, SendInput{Body: "two", To: to(ToRun, "p-1-r1")})
	got := must(t, d, func(tx *sql.Tx) ([]Item, error) { return Take(ctx, tx, r, 0, actor) })
	if len(got) != 1 || got[0].Delivery.ID != b.Deliveries[0].ID || got[0].Delivery.State != Read {
		t.Fatalf("taken %+v", got)
	}
	if state(t, d, a.Deliveries[0].ID) != Dispatched {
		t.Fatal("the claimed entry was taken")
	}
}

// A stopping (or ended) run does not consume its queued answer: it stays queued for
// the resume.
func TestMarkReadRefusesGoneReader(t *testing.T) {
	d, _ := setup(t)
	q, _, err := ask(t, d, AskInput{RunID: "p-1-r1", Body: "?"})
	if err != nil {
		t.Fatal(err)
	}
	ans, err := reply(t, d, ReplyInput{To: q.ID, Body: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ReaderFor(ctx, d, "p-1-r1")
	for st, reason := range map[string]any{"stopping": nil, "ended": "parked"} {
		exec(t, d, `UPDATE runs SET status = ?, ended_reason = ? WHERE id = 'p-1-r1'`, st, reason)
		_, err := inTx(t, d, func(tx *sql.Tx) ([]Item, error) { return Take(ctx, tx, r, 0, actor) })
		if apperr.Code(err) != apperr.Conflict {
			t.Fatalf("%s reader: %v", st, err)
		}
		if s := state(t, d, ans.Deliveries[0].ID); s != Queued {
			t.Fatalf("%s reader consumed the answer: %s", st, s)
		}
	}
	if taskOf(t, d, "p-1") != "waiting" {
		t.Fatal("task left waiting")
	}
}

// An early receipt proof proves every delivery of the attempt, then is consumed.
func TestEarlyProofProvesWholeBatch(t *testing.T) {
	d, _ := setup(t)
	a := send(t, d, SendInput{Body: "one", To: to(ToRun, "p-1-r1")})
	b := send(t, d, SendInput{Body: "two", To: to(ToRun, "p-1-r1")})
	at := claim(t, d, "p-1-r1", adapter.ChanAPIStart)
	if len(at.Items) != 2 {
		t.Fatalf("batch of %d", len(at.Items))
	}
	prove(t, d, ProveInput{RunID: "p-1-r1", Refs: []string{"turn-7"}}) // before the receipt
	res := settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Sent, Receipt: adapter.Receipt{Ref: "turn-7"}})
	if len(res.Changed) != 2 || state(t, d, a.Deliveries[0].ID) != Delivered || state(t, d, b.Deliveries[0].ID) != Delivered {
		t.Fatalf("changed %v: %s %s", res.Changed, state(t, d, a.Deliveries[0].ID), state(t, d, b.Deliveries[0].ID))
	}
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM message_proofs`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d proofs left", n)
	}
}

// A Stop block its proof timeout made unknown is still proven by its continuation
// (same nonce); one a turn end made unknown is not.
func TestLateStopProof(t *testing.T) {
	d, _ := setup(t)
	a := send(t, d, SendInput{Body: "one", To: to(ToRun, "p-1-r1")})
	at := claim(t, d, "p-1-r1", adapter.ChanStopBlock)
	if moved := must(t, d, func(tx *sql.Tx) ([]int64, error) {
		return ProofTimeout(ctx, tx, time.Now().Add(time.Minute), adapter.ChanStopBlock, 0, actor)
	}); len(moved) != 1 {
		t.Fatalf("timeout moved %v", moved)
	}
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Channel: adapter.ChanStopBlock}); len(got) != 0 {
		t.Fatalf("a channel proof without the nonce proved %v", got)
	}
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Channel: adapter.ChanStopBlock, Nonce: at.Nonce}); len(got) != 1 ||
		state(t, d, a.Deliveries[0].ID) != Delivered {
		t.Fatalf("late proof %v: %s", got, state(t, d, a.Deliveries[0].ID))
	}
	b := send(t, d, SendInput{Body: "two", To: to(ToRun, "p-1-r1")})
	bt := claim(t, d, "p-1-r1", adapter.ChanStopBlock)
	must(t, d, func(tx *sql.Tx) ([]int64, error) {
		return MarkUnknown(ctx, tx, "p-1-r1", adapter.ChanStopBlock, "", "turn ended", actor)
	})
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Channel: adapter.ChanStopBlock, Nonce: bt.Nonce}); len(got) != 0 ||
		state(t, d, b.Deliveries[0].ID) != OutcomeUnknown {
		t.Fatalf("a block the turn end made unknown was proven: %v", got)
	}
}

// AwaitsAnswer: an open ask, or an answered one whose answer has not reached the run.
func TestAwaitsAnswer(t *testing.T) {
	d, _ := setup(t)
	if ok, _ := AwaitsAnswer(ctx, d, "p-1", "p-1-r1"); ok {
		t.Fatal("no ask")
	}
	q, _, err := ask(t, d, AskInput{RunID: "p-1-r1", Body: "?"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := AwaitsAnswer(ctx, d, "p-1", "p-1-r1"); !ok {
		t.Fatal("open ask")
	}
	if _, err := reply(t, d, ReplyInput{To: q.ID, Body: "yes"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := AwaitsAnswer(ctx, d, "p-1", "p-1-r1"); !ok {
		t.Fatal("queued answer")
	}
	at := claim(t, d, "p-1-r1", adapter.ChanModSubmit)
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: Unknown})
	if ok, _ := AwaitsAnswer(ctx, d, "p-1", "p-1-r1"); !ok {
		t.Fatal("unknown answer")
	}
	prove(t, d, ProveInput{RunID: "p-1-r1", Markers: []int64{at.Items[0].Delivery.ID}})
	if ok, _ := AwaitsAnswer(ctx, d, "p-1", "p-1-r1"); ok {
		t.Fatal("delivered answer")
	}
}
