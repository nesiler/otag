package msg

import (
	"database/sql"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/db"
)

// "Not now" refusals (NotReady) requeue soon and give the attempt back: however many
// there are, the delivery never goes dead; a definite NotSent still counts.
func TestSettleNotReadyNeverDead(t *testing.T) {
	d, _ := setup(t)
	defer func() { clock = time.Now }()
	m := send(t, d, SendInput{Body: "hello", To: to(ToRun, "p-1-r1")})
	id := m.Deliveries[0].ID
	base := time.Now()
	for i := 0; i < 3*DefaultMaxAttempts; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		clock = func() time.Time { return now }
		at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
			return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanModSubmit, Now: now}, actor)
		})
		if at == nil {
			t.Fatalf("try %d not claimed (%s)", i, state(t, d, id))
		}
		if res := settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: NotReady, Error: "mod not ready"}); len(res.Changed) != 1 {
			t.Fatalf("try %d: %+v", i, res)
		}
		dl, _ := GetDelivery(ctx, d, id)
		if dl.State != Queued || dl.Attempts != 0 || dl.NextAttemptAt == nil || str(dl.LastError) != "mod not ready" {
			t.Fatalf("try %d: %+v", i, dl)
		}
		// Retried soon, not after the NotSent backoff.
		next, err := time.Parse(db.TimeFormat, *dl.NextAttemptAt)
		if err != nil || next.Sub(now) > 10*time.Second {
			t.Fatalf("try %d: next attempt at %s (now %s) %v", i, *dl.NextAttemptAt, now, err)
		}
		if got := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
			return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanModSubmit, Now: now}, actor)
		}); got != nil {
			t.Fatalf("try %d: claimed again at once", i)
		}
	}
	// A NotReady requeue is not a sent attempt: its marker proves nothing.
	if got := prove(t, d, ProveInput{RunID: "p-1-r1", Markers: []int64{id}}); len(got) != 0 {
		t.Fatalf("marker of a never-sent delivery proved %v", got)
	}
	// A definite refusal still counts towards dead.
	later := base.Add(time.Hour)
	clock = func() time.Time { return later }
	at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanModSubmit, Now: later}, actor)
	})
	settle(t, d, SettleInput{Nonce: at.Nonce, Outcome: NotSent, MaxAttempts: 1})
	if st := state(t, d, id); st != Dead {
		t.Fatalf("definite refusal: %s", st)
	}
}

// ClaimInput.Status: the claim takes nothing once the run left the status the attempt
// was planned for.
func TestClaimRequiredStatus(t *testing.T) {
	d, _ := setup(t)
	send(t, d, SendInput{Body: "x", To: to(ToRun, "p-1-r1")})
	exec(t, d, `UPDATE runs SET status = 'running' WHERE id = 'p-1-r1'`)
	if at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanModSubmit, Status: "idle"}, actor)
	}); at != nil {
		t.Fatal("claimed for an idle-only channel while running")
	}
	exec(t, d, `UPDATE runs SET status = 'idle' WHERE id = 'p-1-r1'`)
	if at := must(t, d, func(tx *sql.Tx) (*Attempt, error) {
		return Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanModSubmit, Status: "idle"}, actor)
	}); at == nil {
		t.Fatal("not claimed while idle")
	}
}

// Runs no injected channel can serve are left out before the order and the limit:
// they would never be served, sort first forever and starve the others.
func TestDueSkipsIneligibleRuns(t *testing.T) {
	d, _ := setup(t)
	// p-1-r1 was served (an injected attempt) long ago; the coordinator never was.
	old := send(t, d, SendInput{Body: "old", To: to(ToRun, "p-1-r1")})
	exec(t, d, `UPDATE message_deliveries SET state = 'delivered', channel = 'api_start', proof = 'marker', delivered_at = '2020-01-01T00:00:01.000Z', injected = 1, dispatched_at = '2020-01-01T00:00:00.000Z'
		WHERE id = ?`, old.Deliveries[0].ID)
	send(t, d, SendInput{Body: "x", To: to(ToRun, "p-1-r1")})
	send(t, d, SendInput{Body: "c", To: to(ToRole, RoleCoordinator)})
	// Without a filter, the never-served coordinator takes the only slot, every time.
	if due, _ := Due(ctx, d, time.Time{}, 1); len(due) != 1 || due[0] != "p-coord-r1" {
		t.Fatalf("unfiltered %v", due)
	}
	asked := map[string]int{}
	due, err := DueRuns(ctx, d, DueInput{Limit: 1, Eligible: func(id string) bool {
		asked[id]++
		return id != "p-coord-r1"
	}})
	if err != nil || len(due) != 1 || due[0] != "p-1-r1" {
		t.Fatalf("filtered %v %v", due, err)
	}
	if asked["p-coord-r1"] != 1 || asked["p-1-r1"] != 1 || len(asked) != 2 {
		t.Fatalf("eligibility asked %v", asked)
	}
}

// HasOpenAskBy counts only the given run's asks.
func TestHasOpenAskBy(t *testing.T) {
	d, _ := setup(t)
	if _, _, err := ask(t, d, AskInput{RunID: "p-1-r1", Body: "which db?"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := HasOpenAskBy(ctx, d, "p-1", "p-1-r1"); err != nil || !ok {
		t.Fatalf("own ask: %v %v", ok, err)
	}
	if ok, _ := HasOpenAskBy(ctx, d, "p-1", "p-1-r2"); ok {
		t.Fatal("another run's ask counted")
	}
}
