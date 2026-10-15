package task

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
)

var actor = Actor{Name: "user:test"}

func setup(t *testing.T) (*db.DB, string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "otag.db")
	d, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var pid int64
	err = d.Tx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`INSERT INTO projects (name, path, prefix, config_hash) VALUES ('p', '/tmp/p', 'tag', 'h')
			RETURNING id`).Scan(&pid)
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, path, pid
}

func add(t *testing.T, d *db.DB, pid int64, in AddInput) *Task {
	t.Helper()
	in.ProjectID, in.Prefix = pid, "tag"
	if in.Title == "" {
		in.Title = "work"
	}
	var out *Task
	err := d.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		out, err = Add(context.Background(), tx, in, actor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tx[T any](d *db.DB, fn func(*sql.Tx) (T, error)) (T, error) {
	var out T
	err := d.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

func TestAddIDFormatAndEvent(t *testing.T) {
	d, _, pid := setup(t)
	task := add(t, d, pid, AddInput{Metadata: []byte(`{"a":1}`), ExternalRef: "gitlab#12"})
	if !regexp.MustCompile(`^tag-[0-9a-z]{3,4}$`).MatchString(task.ID) {
		t.Fatalf("id %q", task.ID)
	}
	if task.Status != Ready || task.Version != 1 || string(task.Metadata) != `{"a":1}` {
		t.Fatalf("task %+v", task)
	}
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM events WHERE task_id = ? AND kind = 'task.created' AND to_status = 'ready'`, task.ID).Scan(&n)
	if n != 1 {
		t.Fatal("missing task.created event")
	}
}

func TestIDCollisionFallsBack(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	zero := func() (int, error) { return 0, nil }
	if _, err := d.Exec(`INSERT INTO tasks (id, project_id, title) VALUES ('tag-000', ?, 'x')`, pid); err != nil {
		t.Fatal(err)
	}
	id, err := newIDWith(ctx, d, "tag", zero)
	if err != nil || id != "tag-0000" {
		t.Fatalf("got %q, %v", id, err)
	}
	if _, err := d.Exec(`INSERT INTO tasks (id, project_id, title) VALUES ('tag-0000', ?, 'x')`, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := newIDWith(ctx, d, "tag", zero); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("exhausted: %v", err)
	}
}

func TestConcurrentClaimExactlyOneWinner(t *testing.T) {
	_, path, pid := setup(t)
	d0, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	task := add(t, d0, pid, AddInput{})
	_ = d0.Close()

	const n = 20
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		winners []string
		codes   = map[string]int{}
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Separate *sql.DB per claimer, like separate processes.
			d, err := db.Open(context.Background(), path)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = d.Close() }()
			<-start
			who := Actor{Name: fmt.Sprintf("run:r%d", i)}
			_, err = tx(d, func(tx *sql.Tx) (*Task, error) { return Claim(context.Background(), tx, task.ID, 0, who) })
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners = append(winners, who.Name)
			} else {
				codes[apperr.Code(err)]++
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(winners) != 1 || codes[apperr.Conflict] != n-1 {
		t.Fatalf("winners %v, errors %v", winners, codes)
	}

	d, _ := db.Open(context.Background(), path)
	defer func() { _ = d.Close() }()
	got, _ := Get(context.Background(), d, task.ID)
	if got.Status != Claimed || got.ClaimedBy != winners[0] || got.Version != 2 {
		t.Fatalf("task after race: %+v", got)
	}
	var events int
	_ = d.QueryRow(`SELECT count(*) FROM events WHERE task_id = ? AND kind = 'task.claimed'`, task.ID).Scan(&events)
	if events != 1 {
		t.Fatalf("claim events = %d", events)
	}
}

func TestDependencyCycleRejected(t *testing.T) {
	d, _, pid := setup(t)
	a := add(t, d, pid, AddInput{Title: "a"})
	b := add(t, d, pid, AddInput{Title: "b", Deps: []DepRef{{"blocks", a.ID}}})
	c := add(t, d, pid, AddInput{Title: "c", Deps: []DepRef{{"blocks", b.ID}}})
	ctx := context.Background()

	dep := func(from, kind, on string, version int64) error {
		_, err := tx(d, func(tx *sql.Tx) (any, error) { return nil, AddDep(ctx, tx, from, kind, on, version, actor) })
		return err
	}
	for _, tc := range []struct{ from, on string }{{a.ID, c.ID}, {a.ID, b.ID}, {a.ID, a.ID}} {
		if err := dep(tc.from, "blocks", tc.on, 0); apperr.Code(err) != apperr.Cycle {
			t.Errorf("%s blocks-on %s: got %v", tc.from, tc.on, err)
		}
	}
	// related edges may go both ways; other kinds are checked per kind.
	if err := dep(a.ID, "related", c.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := dep(a.ID, "parent", c.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := dep(c.ID, "parent", a.ID, 0); apperr.Code(err) != apperr.Cycle {
		t.Fatalf("parent cycle: %v", err)
	}
}

func TestDepChangesBumpVersion(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	a := add(t, d, pid, AddInput{Title: "a"})
	b := add(t, d, pid, AddInput{Title: "b"})
	if b.Version != 1 {
		t.Fatalf("new task version %d", b.Version)
	}
	run := func(fn func(*sql.Tx) error) error {
		_, err := tx(d, func(tx *sql.Tx) (any, error) { return nil, fn(tx) })
		return err
	}
	if err := run(func(tx *sql.Tx) error { return AddDep(ctx, tx, b.ID, "blocks", a.ID, 1, actor) }); err != nil {
		t.Fatal(err)
	}
	// A stale version is rejected for both add and remove.
	if err := run(func(tx *sql.Tx) error { return AddDep(ctx, tx, b.ID, "related", a.ID, 1, actor) }); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("stale add: %v", err)
	}
	if err := run(func(tx *sql.Tx) error { return RemoveDep(ctx, tx, b.ID, "blocks", a.ID, 1, actor) }); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("stale remove: %v", err)
	}
	if err := run(func(tx *sql.Tx) error { return RemoveDep(ctx, tx, b.ID, "blocks", a.ID, 2, actor) }); err != nil {
		t.Fatal(err)
	}
	got, _ := Get(ctx, d, b.ID)
	if got.Version != 3 || len(got.Deps) != 0 {
		t.Fatalf("after add+remove: version %d deps %v", got.Version, got.Deps)
	}
}

func TestRetiredProjectCannotClaim(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	task := add(t, d, pid, AddInput{})
	if _, err := d.Exec(`UPDATE projects SET retired_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Claim(ctx, tx, task.ID, 0, actor) }); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("claim in retired project: %v", err)
	}
	if _, err := tx(d, func(tx *sql.Tx) (*Task, error) {
		return Add(ctx, tx, AddInput{ProjectID: pid, Prefix: "tag", Title: "x"}, actor)
	}); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("add in retired project: %v", err)
	}
}

func TestReadyViewRespectsBlockers(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	blocker := add(t, d, pid, AddInput{Title: "blocker"})
	blocked := add(t, d, pid, AddInput{Title: "blocked", Priority: 5, Deps: []DepRef{{"blocks", blocker.ID}}})
	add(t, d, pid, AddInput{Title: "draft", Draft: true})

	ready := func() []string {
		ts, err := List(ctx, d, Filter{ProjectID: pid, ReadyOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, x := range ts {
			ids = append(ids, x.ID)
		}
		return ids
	}
	if got := ready(); len(got) != 1 || got[0] != blocker.ID {
		t.Fatalf("ready = %v", got)
	}
	if _, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Claim(ctx, tx, blocked.ID, 0, actor) }); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("claiming blocked task: %v", err)
	}
	for _, s := range []string{Claimed, Running, Done} {
		if _, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Transition(ctx, tx, blocker.ID, s, "", 0, actor) }); err != nil {
			t.Fatal(err)
		}
	}
	if got := ready(); len(got) != 1 || got[0] != blocked.ID {
		t.Fatalf("ready after blocker done = %v", got)
	}
	next, err := tx(d, func(tx *sql.Tx) (*Task, error) { return ClaimNext(ctx, tx, pid, actor) })
	if err != nil || next.ID != blocked.ID {
		t.Fatalf("claim next: %v %v", next, err)
	}
}

func TestStatusMachine(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	move := func(id, to string) error {
		_, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Transition(ctx, tx, id, to, "r", 0, actor) })
		return err
	}
	task := add(t, d, pid, AddInput{Draft: true})
	invalid := []string{Claimed, Running, Waiting, Review, Done}
	for _, to := range invalid {
		if err := move(task.ID, to); apperr.Code(err) != apperr.InvalidTransition {
			t.Errorf("draft -> %s: %v", to, err)
		}
	}
	for _, to := range []string{Ready, Claimed, Running, Waiting, Running, Review, Ready, Blocked, Ready, Claimed, Running, Done} {
		if err := move(task.ID, to); err != nil {
			t.Fatalf("-> %s: %v", to, err)
		}
	}
	for _, to := range []string{Ready, Cancelled, Blocked, Running} {
		if err := move(task.ID, to); apperr.Code(err) != apperr.InvalidTransition {
			t.Errorf("done -> %s: %v", to, err)
		}
	}
	got, _ := Get(ctx, d, task.ID)
	if got.ClosedAt == "" || got.Status != Done {
		t.Fatalf("closed task %+v", got)
	}
	var events int
	_ = d.QueryRow(`SELECT count(*) FROM events WHERE task_id = ? AND to_status IS NOT NULL`, task.ID).Scan(&events)
	if events != 13 { // created + 12 transitions
		t.Fatalf("status events = %d", events)
	}
	var dur int
	_ = d.QueryRow(`SELECT count(*) FROM v_status_durations WHERE entity_id = ?`, task.ID).Scan(&dur)
	if dur != 13 {
		t.Fatalf("status durations rows = %d", dur)
	}

	other := add(t, d, pid, AddInput{})
	if _, err := tx(d, func(tx *sql.Tx) (*Task, error) { return Transition(ctx, tx, other.ID, Blocked, "", 99, actor) }); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("stale version: %v", err)
	}
	if err := move(other.ID, "bogus"); apperr.Code(err) != apperr.Invalid {
		t.Fatalf("unknown status: %v", err)
	}
}

func TestEdit(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	task := add(t, d, pid, AddInput{Metadata: []byte(`{"a":1}`)})
	title := "new title"
	got, err := tx(d, func(tx *sql.Tx) (*Task, error) {
		return Edit(ctx, tx, task.ID, EditInput{Title: &title, MetaSet: map[string]string{"b": "2", "c": "x"}}, actor)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != title || string(got.Metadata) != `{"a":1,"b":2,"c":"x"}` || got.Version != 2 {
		t.Fatalf("edited %+v", got)
	}
	if _, err := tx(d, func(tx *sql.Tx) (*Task, error) {
		return Edit(ctx, tx, task.ID, EditInput{Title: &title, ExpectVersion: 1}, actor)
	}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("stale edit: %v", err)
	}
}

// Recent orders by the last status change, so a Limit keeps the latest, whatever the
// priority.
func TestListRecent(t *testing.T) {
	d, _, pid := setup(t)
	old := add(t, d, pid, AddInput{Title: "old", Priority: 9})
	mid := add(t, d, pid, AddInput{Title: "mid", Priority: 5})
	young := add(t, d, pid, AddInput{Title: "young"})
	for i, id := range []string{old.ID, mid.ID, young.ID} {
		if _, err := d.Exec(`UPDATE tasks SET status = 'cancelled', status_changed_at = ? WHERE id = ?`,
			fmt.Sprintf("2026-10-1%dT00:00:00.000Z", i), id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(context.Background(), d, Filter{Statuses: []string{Cancelled}, Recent: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != young.ID || got[1].ID != mid.ID {
		t.Fatalf("recent: %+v", got)
	}
}
