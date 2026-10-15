package run

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

func testManager(t *testing.T) *Manager {
	d := setupRun(t)
	return &Manager{DB: d, Adapters: adapter.NewRegistry(fake.New()), Tmux: tmux.New("otag-unused-"+t.Name(), os.DevNull),
		Getenv: func(string) string { return "" }, Actor: Actor{Name: "user:test"}}
}

// A claimed task whose run is still launching cannot get a second run (finding: two
// harnesses in one worktree).
func TestOneActiveRunPerTask(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	if _, err := m.DB.Exec(`UPDATE tasks SET status = 'claimed'`); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.create(ctx, StartInput{TaskID: "x-1", Profile: "p"})
	if apperr.Code(err) != apperr.Conflict {
		t.Fatalf("second start: %v", err)
	}
	// The unique index holds even if the check is bypassed.
	_, err = m.DB.Exec(`INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
		VALUES ('x-1-r2', 1, 'x-1', 'child', 1, 1, 1, 'fake', 'queued')`)
	if err == nil {
		t.Fatal("index allowed a second active run")
	}
}

// done needs a started run that owns its task (finding: done on a queued run left the
// task claimed).
func TestDoneRequiresStartedOwningRun(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'queued'; UPDATE tasks SET status = 'claimed'`)
	if _, err := m.Done(ctx, "x-1-r1", "x"); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("done on queued run: %v", err)
	}
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'; UPDATE tasks SET current_run_id = NULL, status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", "x"); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("done on a run that lost its task: %v", err)
	}
	var status, task string
	_ = m.DB.QueryRow(`SELECT r.status, t.status FROM runs r, tasks t`).Scan(&status, &task)
	if status != Running || task != "running" {
		t.Fatalf("state changed: run %s task %s", status, task)
	}
	_, _ = m.DB.Exec(`UPDATE tasks SET current_run_id = 'x-1-r1'`)
	res, err := m.Done(ctx, "x-1-r1", "x")
	if err != nil || res.TaskStatus != "done" || res.ReviewTask != "" {
		t.Fatalf("done: %+v %v", res, err)
	}
}

// A task cancelled (or blocked) while its run works: the task change and the run's
// move to stopping commit together, and the run's own `otag done` afterwards is refused
// with a clear error instead of finishing a cancelled task.
func TestStopTaskRunWithTaskChange(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	var runID string
	err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := task.Transition(ctx, tx, "x-1", task.Cancelled, "not needed", 0, task.Actor{Name: "user:test"}); err != nil {
			return err
		}
		var err error
		runID, err = StopTaskRun(ctx, tx, "x-1", "task x-1 cancelled", m.Actor)
		return err
	})
	if err != nil || runID != "x-1-r1" {
		t.Fatalf("stop with cancel: %q %v", runID, err)
	}
	_, err = m.Done(ctx, runID, "finished anyway")
	if apperr.Code(err) != apperr.InvalidTransition || !strings.Contains(err.Error(), "being stopped") {
		t.Fatalf("done while stopping: %v", err)
	}
	var data string
	_ = m.DB.QueryRow(`SELECT data FROM events WHERE run_id = ? AND to_status = 'stopping'`, runID).Scan(&data)
	if !strings.Contains(data, `"end_reason":"stopped"`) {
		t.Fatalf("stop intent %s", data)
	}
	// The stop finishes; the cancelled task stays cancelled.
	res, err := m.StopWith(ctx, runID, StopOptions{Reason: "task x-1 cancelled", EndReason: ReasonStopped,
		Expect: &Expect{Status: Stopping, HeartbeatAt: "*"}})
	if err != nil || res.Run.EndedReason != ReasonStopped || res.Run.TaskStatus != task.Cancelled {
		t.Fatalf("stop: %+v %v", res, err)
	}
	if res.Run.CleanupPending {
		t.Fatal("nothing to clean up, but cleanup still pending")
	}
	// No active run: nothing to stop.
	_ = m.DB.Tx(ctx, func(tx *sql.Tx) error { runID, err = StopTaskRun(ctx, tx, "x-1", "again", m.Actor); return err })
	if runID != "" || err != nil {
		t.Fatalf("second stop: %q %v", runID, err)
	}
}

// A previous run of the workspace that ended after Start cleared it (its cleanup still
// pending) is caught inside the creation transaction: nothing is created, retryably.
// A failed cleanup waits for cleanup_after and does not hold back others.
func TestCreateRechecksPendingCleanup(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'done', cleanup_pending = 1;
		UPDATE tasks SET status = 'ready', current_run_id = NULL`)
	_, _, err := m.create(ctx, StartInput{TaskID: "x-1", Profile: "p"})
	if !errors.Is(err, errCleanupPending) {
		t.Fatalf("create: %v", err)
	}
	if e, ok := apperr.As(err); !ok || !e.Retryable {
		t.Fatalf("not retryable: %v", err)
	}
	var n int
	_ = m.DB.QueryRow(`SELECT count(*) FROM runs`).Scan(&n)
	if s, _ := task.Get(ctx, m.DB, "x-1"); n != 1 || s.Status != task.Ready {
		t.Fatalf("%d runs, task %s", n, s.Status)
	}
	now := time.Now()
	if ids, _ := PendingCleanups(ctx, m.DB, now, 10); len(ids) != 1 {
		t.Fatalf("due %v", ids)
	}
	_ = m.settleCleanup(ctx, "x-1-r1", errors.New("survivor"))
	if ids, _ := PendingCleanups(ctx, m.DB, now, 10); len(ids) != 0 {
		t.Fatalf("failed cleanup due at once: %v", ids)
	}
	if ids, _ := PendingCleanups(ctx, m.DB, now.Add(CleanupRetry+time.Second), 10); len(ids) != 1 {
		t.Fatalf("failed cleanup not due after the retry interval: %v", ids)
	}
}

// Directories compare by identity: a case alias on a case-insensitive volume and a
// symlink are the same directory.
func TestSameDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Proj")
	other := filepath.Join(root, "other")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.MkdirAll(other, 0o755)
	_ = os.Symlink(dir, filepath.Join(root, "link"))
	if !sameDir(dir, filepath.Join(root, "link")) || sameDir(dir, other) || sameDir(dir, "") {
		t.Fatal("identity")
	}
	if _, err := os.Stat(filepath.Join(root, "proj")); err == nil && !sameDir(dir, filepath.Join(root, "proj")) {
		t.Fatal("case alias on a case-insensitive volume")
	}
}

// Adapter files cannot be written through symlinks or outside the run dir.
func TestWriteFilesStaysInRunDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run")
	outside := filepath.Join(root, "outside")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.MkdirAll(outside, 0o700)
	victim := filepath.Join(outside, "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	_ = os.Symlink(victim, filepath.Join(dir, "settings.json"))
	_ = os.Symlink(outside, filepath.Join(dir, "sub"))

	if err := writeFiles(dir, []adapter.File{{Path: "settings.json", Data: []byte("new")}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("wrote through a symlink")
	}
	if st, _ := os.Lstat(filepath.Join(dir, "settings.json")); st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("not replaced by a regular 0600 file: %v", st.Mode())
	}
	for _, p := range []string{"sub/x", "../x", "/etc/x", "a/../../x"} {
		if err := writeFiles(dir, []adapter.File{{Path: p, Data: []byte("x")}}); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Fatal("file created outside the run dir")
	}
	if err := writeFiles(dir, []adapter.File{{Path: "mod/hooks/hooks.json", Data: []byte("{}"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
}
