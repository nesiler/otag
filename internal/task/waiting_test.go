package task

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
)

// waitingTask returns a task in status waiting whose run r1 has status runStatus.
func waitingTask(t *testing.T, d *db.DB, pid int64, runStatus string) *Task {
	t.Helper()
	ctx := context.Background()
	task := add(t, d, pid, AddInput{})
	ended := sql.NullString{String: "parked", Valid: runStatus == "ended"}
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT OR IGNORE INTO accounts (id, name, harness, config_hash) VALUES (1, 'f', 'fake', 'h');
INSERT OR IGNORE INTO profiles (id, name, account_id, config_hash) VALUES (1, 'p', 1, 'h');
INSERT OR IGNORE INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (1, 1, 1, 'fake', 'h', '{}')`)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id,
			harness, status, ended_reason) VALUES (?, ?, ?, 'child', 1, 1, 1, 'fake', ?, ?)`,
			task.ID+"-r1", pid, task.ID, runStatus, ended); err != nil {
			return err
		}
		for _, to := range []string{Claimed, Running} {
			if _, err := Transition(ctx, tx, task.ID, to, "", 0, actor); err != nil {
				return err
			}
		}
		if err := pointAtRun(ctx, tx, task.ID, task.ID+"-r1"); err != nil {
			return err
		}
		_, err = Transition(ctx, tx, task.ID, Waiting, "question #1: which db?", 0, actor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := Get(ctx, d, task.ID)
	return got
}

// pointAtRun points the task at a run whatever its status.
func pointAtRun(ctx context.Context, tx *sql.Tx, id, runID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET current_run_id = ?, version = version + 1 WHERE id = ?`, runID, id)
	return err
}

// The next run takes over a waiting task only once its run has ended; Transition to
// claimed reaches the same path (it used to reject waiting through Claim).
func TestClaimWaiting(t *testing.T) {
	d, _, pid := setup(t)
	ctx := context.Background()
	claim := func(id string, via func(*sql.Tx) (*Task, error)) (*Task, error) {
		return tx(d, via)
	}

	live := waitingTask(t, d, pid, "idle")
	_, err := claim(live.ID, func(tx *sql.Tx) (*Task, error) { return Transition(ctx, tx, live.ID, Claimed, "", 0, actor) })
	if apperr.Code(err) != apperr.Conflict {
		t.Fatalf("claim of a task waiting in a live run: %v", err)
	}
	// `otag task claim` still takes only ready tasks.
	if _, err := claim(live.ID, func(tx *sql.Tx) (*Task, error) { return Claim(ctx, tx, live.ID, 0, actor) }); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("Claim of a waiting task: %v", err)
	}

	parked := waitingTask(t, d, pid, "ended")
	if _, err := claim(parked.ID, func(tx *sql.Tx) (*Task, error) {
		return Transition(ctx, tx, parked.ID, Claimed, "", parked.Version+5, actor)
	}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("stale version: %v", err)
	}
	who := Actor{Name: "daemon", Source: "daemon"}
	got, err := claim(parked.ID, func(tx *sql.Tx) (*Task, error) {
		return Transition(ctx, tx, parked.ID, Claimed, "", parked.Version, who)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Claimed || got.ClaimedBy != "daemon" || got.StatusReason != "" || got.CurrentRunID != parked.ID+"-r1" {
		t.Fatalf("claimed: %+v", got)
	}
	var from, to string
	_ = d.QueryRow(`SELECT from_status, to_status FROM events WHERE task_id = ? AND kind = 'task.claimed'
		ORDER BY id DESC LIMIT 1`, parked.ID).Scan(&from, &to)
	if from != Waiting || to != Claimed {
		t.Fatalf("event %s -> %s", from, to)
	}
	if _, err := claim(parked.ID, func(tx *sql.Tx) (*Task, error) { return ClaimWaiting(ctx, tx, parked.ID, 0, actor) }); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("ClaimWaiting of a claimed task: %v", err)
	}
	// The other waiting edges still work.
	for _, to := range []string{Running, Waiting, Running} {
		if _, err := claim(live.ID, func(tx *sql.Tx) (*Task, error) { return Transition(ctx, tx, live.ID, to, "", 0, actor) }); err != nil {
			t.Fatalf("live -> %s: %v", to, err)
		}
	}
}

func TestClaimWaitingExactlyOneWinner(t *testing.T) {
	d, path, pid := setup(t)
	task := waitingTask(t, d, pid, "ended")
	const n = 10
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
		errs = map[string]int{}
		gate = make(chan struct{})
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := db.Open(context.Background(), path)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = d.Close() }()
			<-gate
			_, err = tx(d, func(tx *sql.Tx) (*Task, error) {
				return Transition(context.Background(), tx, task.ID, Claimed, "", 0, Actor{Name: fmt.Sprintf("c%d", i)})
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				errs[apperr.Code(err)]++
			}
		}()
	}
	close(gate)
	wg.Wait()
	if wins != 1 || errs[apperr.InvalidTransition]+errs[apperr.Conflict] != n-1 {
		t.Fatalf("wins %d errors %v", wins, errs)
	}
}
