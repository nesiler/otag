package run

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/task"
)

// turnProver is the fake harness as one whose launch prompt has no marker hook
// (OpenCode): its turn start proves the launch prompt.
type turnProver struct{ *fake.Adapter }

func (turnProver) LaunchProvedByTurnStart() bool { return true }

func TestLaunchPromptProvedByTurnStart(t *testing.T) {
	for _, byTurn := range []bool{false, true} {
		d, reg, _ := msgSetup(t, Starting)
		if byTurn {
			reg = adapter.NewRegistry(turnProver{fake.New()})
		}
		ctx := context.Background()
		m := sendTo(t, d, "the answer")
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			at, err := msg.Claim(ctx, tx, msg.ClaimInput{RunID: "x-1-r1", Channel: adapter.ChanLaunchPrompt}, msg.Actor{Name: "t"})
			if err != nil || at == nil {
				t.Fatalf("claim %v %v", at, err)
			}
			_, err = msg.Settle(ctx, tx, msg.SettleInput{Nonce: at.Nonce, Outcome: msg.Sent,
				Receipt: adapter.Receipt{Channel: adapter.ChanLaunchPrompt}}, msg.Actor{Name: "t"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		fhook(t, d, reg, "turn_start", `{"event_id":"p1"}`) // no prompt with the marker
		want := msg.Dispatched
		if byTurn {
			want = msg.Delivered
		}
		if dl := delivery(t, d, m.Deliveries[0].ID); dl.State != want {
			t.Fatalf("byTurn %v: %s, want %s", byTurn, dl.State, want)
		}
	}
}

// A closed dialog leaves the task waiting while the answer to the run's (closed) ask
// has not reached it; the answer arriving moves it.
func TestDialogKeepsUndeliveredAnswerWaiting(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	ctx := context.Background()
	a := msg.Actor{Name: "t"}
	var q *msg.Message
	err := d.Tx(ctx, func(tx *sql.Tx) (err error) {
		q, _, err = msg.Ask(ctx, tx, msg.AskInput{RunID: "x-1-r1", Body: "which db?"}, a)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var ans *msg.Message
	if err := d.Tx(ctx, func(tx *sql.Tx) (err error) {
		ans, _, err = msg.Reply(ctx, tx, msg.ReplyInput{To: q.ID, From: human, Body: "SQLite"}, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fhook(t, d, reg, "permission", `{"text":"Allow curl?"}`)
	fhook(t, d, reg, "activity", `{}`) // the dialog closes; the answer is still queued
	if tk, _ := task.Get(ctx, d, "x-1"); tk.Status != task.Waiting {
		t.Fatalf("task %s with the answer queued", tk.Status)
	}
	r, _ := msg.ReaderFor(ctx, d, "x-1-r1")
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := msg.MarkRead(ctx, tx, []int64{ans.Deliveries[0].ID}, r, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if tk, _ := task.Get(ctx, d, "x-1"); tk.Status != task.Running {
		t.Fatalf("task %s after the answer was read", tk.Status)
	}
}

// A continued Stop proves its block even after the block's proof timeout.
func TestContinuedStopAfterProofTimeout(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	ctx := context.Background()
	fhook(t, d, reg, "turn_start", `{"event_id":"p1"}`)
	m := sendTo(t, d, "one more thing")
	fhook(t, d, reg, "turn_end", `{"slot":"stop","session_id":"s1"}`)
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := msg.ProofTimeout(ctx, tx, time.Now().Add(time.Hour), adapter.ChanStopBlock, 0, msg.Actor{Name: "t"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if dl := delivery(t, d, m.Deliveries[0].ID); dl.State != msg.OutcomeUnknown {
		t.Fatalf("timed out: %s", dl.State)
	}
	fhook(t, d, reg, "turn_end", `{"slot":"stop","stop_hook_active":true,"session_id":"other"}`)
	if dl := delivery(t, d, m.Deliveries[0].ID); dl.State != msg.OutcomeUnknown {
		t.Fatalf("another session proved it: %s", dl.State)
	}
	fhook(t, d, reg, "turn_end", `{"slot":"stop","stop_hook_active":true,"session_id":"s1"}`)
	if dl := delivery(t, d, m.Deliveries[0].ID); dl.State != msg.Delivered {
		t.Fatalf("late continued Stop: %s", dl.State)
	}
}

// A continued Stop of a later turn never proves an older turn's timed-out block.
func TestContinuedStopOfLaterTurn(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	ctx := context.Background()
	fhook(t, d, reg, "turn_start", `{"event_id":"p1"}`)
	m := sendTo(t, d, "one more thing")
	fhook(t, d, reg, "turn_end", `{"slot":"stop","session_id":"s1"}`)
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := msg.ProofTimeout(ctx, tx, time.Now().Add(time.Hour), adapter.ChanStopBlock, 0, msg.Actor{Name: "t"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	fhook(t, d, reg, "turn_start", `{"event_id":"p2"}`)
	fhook(t, d, reg, "turn_end", `{"slot":"stop","stop_hook_active":true,"session_id":"s1"}`)
	if dl := delivery(t, d, m.Deliveries[0].ID); dl.State != msg.OutcomeUnknown {
		t.Fatalf("a later turn proved it: %s", dl.State)
	}
}
