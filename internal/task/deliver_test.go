package task

import (
	"context"
	"database/sql"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
)

// done -> delivered only through Deliver: a CAS with delivered_at, the note as the
// status reason and a task.delivered event. delivered is terminal; closed_at keeps the
// time the task became done.
func TestDeliver(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	move := func(id, to string) error {
		_, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Transition(ctx, tx, id, to, "", 0, actor) })
		return err
	}
	deliver := func(id, note string, version int64) (*Task, error) {
		return tx(d, func(tx *sql.Tx) (*Task, error) { return Deliver(ctx, tx, id, note, version, actor) })
	}
	task := add(t, d, pid, AddInput{})
	for _, from := range []string{Ready, Claimed, Running, Review} {
		if from != Ready {
			if err := move(task.ID, from); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := deliver(task.ID, "", 0); apperr.Code(err) != apperr.InvalidTransition {
			t.Fatalf("deliver from %s: %v", from, err)
		}
	}
	if err := move(task.ID, Done); err != nil {
		t.Fatal(err)
	}
	if err := move(task.ID, Delivered); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("Transition to delivered: %v", err)
	}
	done, _ := Get(ctx, d, task.ID)
	if _, err := deliver(task.ID, "", done.Version+1); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("stale version: %v", err)
	}
	got, err := deliver(task.ID, "merged as abc123", done.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Delivered || got.DeliveredAt == "" || got.StatusReason != "merged as abc123" ||
		got.ClosedAt != done.ClosedAt || got.Version != done.Version+1 {
		t.Fatalf("delivered %+v (done %+v)", got, done)
	}
	var data string
	_ = d.QueryRow(`SELECT data FROM events WHERE task_id = ? AND kind = 'task.delivered' AND from_status = 'done'
		AND to_status = 'delivered'`, task.ID).Scan(&data)
	if data != `{"note":"merged as abc123"}` {
		t.Fatalf("event data %q", data)
	}
	if _, err := deliver(task.ID, "", 0); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("second deliver: %v", err)
	}
	for _, to := range []string{Ready, Cancelled, Blocked, Done} {
		if err := move(task.ID, to); apperr.Code(err) != apperr.InvalidTransition {
			t.Errorf("delivered -> %s: %v", to, err)
		}
	}
	if !Terminal(Delivered) || CanTransition(Cancelled, Delivered) || !CanTransition(Done, Delivered) {
		t.Fatal("status machine")
	}
	// The schema holds the pair too.
	if _, err := d.Exec(`UPDATE tasks SET delivered_at = NULL WHERE id = ?`, task.ID); err == nil {
		t.Fatal("delivered without delivered_at accepted")
	}
}

// A delivered blocker is finished; list filters: --open leaves delivered out,
// --deliverable lists done tasks without review tasks.
func TestDeliveredInListsAndReadyView(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	blocker := add(t, d, pid, AddInput{Title: "blocker"})
	blocked := add(t, d, pid, AddInput{Title: "blocked", Deps: []DepRef{{"blocks", blocker.ID}}})
	other := add(t, d, pid, AddInput{Title: "other"})
	review := add(t, d, pid, AddInput{Title: "Review: other", Deps: []DepRef{{"review_of", other.ID}}})
	for _, id := range []string{blocker.ID, other.ID, review.ID} {
		for _, s := range []string{Claimed, Running, Done} {
			if _, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Transition(ctx, tx, id, s, "", 0, actor) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Deliver(ctx, tx, blocker.ID, "", 0, actor) }); err != nil {
		t.Fatal(err)
	}
	ids := func(f Filter) []string {
		f.ProjectID = pid
		ts, err := List(ctx, d, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	if got := ids(Filter{ReadyOnly: true}); len(got) != 1 || got[0] != blocked.ID {
		t.Fatalf("ready = %v", got)
	}
	if got := ids(Filter{Open: true}); len(got) != 1 || got[0] != blocked.ID {
		t.Fatalf("open = %v", got)
	}
	if got := ids(Filter{Deliverable: true}); len(got) != 1 || got[0] != other.ID {
		t.Fatalf("deliverable = %v", got)
	}
	if got := ids(Filter{Statuses: []string{Delivered}}); len(got) != 1 || got[0] != blocker.ID {
		t.Fatalf("delivered = %v", got)
	}
}
