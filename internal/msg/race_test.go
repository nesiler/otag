package msg

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
)

// race runs fn n times concurrently, each with its own *db.DB (like separate
// processes: CLI, daemon, hook bridge), and returns the results.
func race[T any](t *testing.T, path string, n int, fn func(i int, d *db.DB) (T, error)) ([]T, []error) {
	t.Helper()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		gate = make(chan struct{})
		outs []T
		errs []error
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := db.Open(ctx, path)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = d.Close() }()
			<-gate
			out, err := fn(i, d)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				outs = append(outs, out)
			}
		}()
	}
	close(gate)
	wg.Wait()
	return outs, errs
}

// Concurrent claimers (daemon, CLI, mod pull) of one run: exactly one injected attempt
// carries each delivery, and only one injected attempt is in flight.
func TestRaceClaimOneInjectedAttempt(t *testing.T) {
	d, path := setup(t)
	for i := range 3 {
		send(t, d, SendInput{Body: fmt.Sprint("m", i), To: to(ToRun, "p-1-r1")})
	}
	channels := []string{adapter.ChanAPIStart, adapter.ChanAPISteer, adapter.ChanModSubmit}
	outs, errs := race(t, path, 12, func(i int, d *db.DB) (*Attempt, error) {
		var at *Attempt
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			at, err = Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: channels[i%3], Max: 1}, Actor{Name: "c"})
			return err
		})
		return at, err
	})
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	var got []*Attempt
	for _, at := range outs {
		if at != nil {
			got = append(got, at)
		}
	}
	if len(got) != 1 || len(got[0].Items) != 1 {
		t.Fatalf("%d attempts won", len(got))
	}
	var dispatched int
	_ = d.QueryRow(`SELECT count(*) FROM message_deliveries WHERE state = 'dispatched'`).Scan(&dispatched)
	if dispatched != 1 {
		t.Fatalf("%d dispatched", dispatched)
	}
}

// Hook channels of concurrent hooks never carry the same delivery twice.
func TestRaceClaimHookChannels(t *testing.T) {
	d, path := setup(t)
	for i := range 20 {
		send(t, d, SendInput{Body: fmt.Sprint("m", i), To: to(ToTask, "p-1")})
	}
	outs, errs := race(t, path, 8, func(i int, d *db.DB) (*Attempt, error) {
		var at *Attempt
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			at, err = Claim(ctx, tx, ClaimInput{RunID: "p-1-r1", Channel: adapter.ChanToolContext, Max: 3}, Actor{Name: "h"})
			return err
		})
		return at, err
	})
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	seen := map[int64]bool{}
	total := 0
	for _, at := range outs {
		if at == nil {
			continue
		}
		for _, it := range at.Items {
			if seen[it.Delivery.ID] {
				t.Fatalf("delivery %d claimed twice", it.Delivery.ID)
			}
			seen[it.Delivery.ID] = true
			total++
		}
	}
	if total != 20 {
		t.Fatalf("claimed %d of 20", total)
	}
}

// Concurrent answers to one question: exactly one closes it.
func TestRaceOneAnswer(t *testing.T) {
	d, path := setup(t)
	q, _, err := ask(t, d, AskInput{Body: "which?"})
	if err != nil {
		t.Fatal(err)
	}
	outs, errs := race(t, path, 8, func(i int, d *db.DB) (*Message, error) {
		var m *Message
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			m, _, err = Reply(ctx, tx, ReplyInput{From: human, To: q.ID, Body: fmt.Sprint("answer ", i)}, actor)
			return err
		})
		return m, err
	})
	if len(outs) != 1 || len(errs) != 7 {
		t.Fatalf("%d answers, errors %v", len(outs), errs)
	}
	for _, err := range errs {
		if apperr.Code(err) != apperr.Conflict {
			t.Fatalf("loser: %v", err)
		}
	}
	var answers int
	_ = d.QueryRow(`SELECT count(*) FROM messages WHERE reply_to_id = ? AND kind = 'answer'`, q.ID).Scan(&answers)
	if answers != 1 {
		t.Fatalf("%d answers stored", answers)
	}
}

// A proof racing the attempt's settle (proof before or after the receipt) ends
// delivered either way, with the receipt stored.
func TestRaceProofAndSettle(t *testing.T) {
	for round := range 10 {
		d, path := setup(t)
		m := send(t, d, SendInput{Body: "x", To: to(ToRun, "p-1-r1")})
		at := claim(t, d, "p-1-r1", adapter.ChanAPIStart)
		ref := fmt.Sprint("turn-", round)
		_, errs := race(t, path, 2, func(i int, d *db.DB) (bool, error) {
			return true, d.Tx(ctx, func(tx *sql.Tx) error {
				if i == 0 {
					_, err := Prove(ctx, tx, ProveInput{RunID: "p-1-r1", Refs: []string{ref}}, actor)
					return err
				}
				_, err := Settle(ctx, tx, SettleInput{Nonce: at.Nonce, Outcome: Sent, Receipt: adapter.Receipt{Ref: ref}}, actor)
				return err
			})
		})
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		dl, _ := GetDelivery(ctx, d, m.Deliveries[0].ID)
		if dl.State != Delivered || str(dl.Receipt) != ref {
			t.Fatalf("round %d: %s receipt %v", round, dl.State, str(dl.Receipt))
		}
	}
}
