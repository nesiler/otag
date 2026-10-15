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

	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/task"
)

// A Stop block whose Settle fails is undone by its savepoint: nothing is printed, the
// turn end stands, and the messages stay queued for the next hook (no double delivery).
func TestStopBlockSettleFailurePrintsNothing(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	ctx := context.Background()
	fhook(t, d, reg, "turn_start", `{"event_id":"p1"}`)
	m := sendTo(t, d, "one more thing")
	id := m.Deliveries[0].ID
	// The claim (receipt_ref NULL) passes; the Sent settle (receipt stop:s1) fails.
	if _, err := d.Exec(`CREATE TRIGGER no_receipt BEFORE UPDATE OF receipt_ref ON message_deliveries
		WHEN NEW.receipt_ref IS NOT NULL BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	resp, err := HandleHook(ctx, d, reg, HookRequest{Harness: "fake", Kind: "turn_end", RunID: "x-1-r1",
		Payload: []byte(`{"slot":"stop","session_id":"s1"}`)})
	if err != nil || resp.MsgErr == nil {
		t.Fatalf("%v / %v", err, resp.MsgErr)
	}
	if strings.Contains(string(resp.Output), "block") || strings.Contains(string(resp.Output), msg.Marker(id)) {
		t.Fatalf("printed a block that was undone: %s", resp.Output)
	}
	if st := runStatus(t, d); st != Idle {
		t.Fatalf("the turn end was dropped: %s", st)
	}
	if dl := delivery(t, d, id); dl.State != msg.Queued || dl.Attempts != 0 {
		t.Fatalf("claim not undone: %+v", dl)
	}
}

// A "not now" refusal (adapter.ErrNotReady) requeues without using up attempts: it
// never makes the delivery dead.
func TestDispatchNotReadyKeepsAttempts(t *testing.T) {
	d, reg, getenv := msgSetup(t, Idle)
	ctx := context.Background()
	runDir := filepath.Join(getenv("OTAG_DATA_DIR"), "runs", "x-1-r1")
	_ = os.WriteFile(filepath.Join(runDir, fake.ModeFile), []byte("notready"), 0o600)
	disp := &Dispatcher{DB: d, Adapters: reg, Getenv: getenv, Actor: msg.Actor{Name: "daemon"}}
	m := sendTo(t, d, "hello")
	id := m.Deliveries[0].ID
	for i := 0; i < msg.DefaultMaxAttempts+2; i++ {
		_, _ = d.Exec(`UPDATE message_deliveries SET next_attempt_at = NULL`)
		ra := disp.Dispatch(ctx, "x-1-r1")
		if ra.Result != "queued" || !strings.Contains(ra.Reason, "not now") {
			t.Fatalf("try %d: %+v", i, ra)
		}
		if dl := delivery(t, d, id); dl.State != msg.Queued || dl.Attempts != 0 || dl.NextAttemptAt == nil {
			t.Fatalf("try %d: %+v", i, dl)
		}
	}
	// Ready again: it goes.
	_ = os.Remove(filepath.Join(runDir, fake.ModeFile))
	_, _ = d.Exec(`UPDATE message_deliveries SET next_attempt_at = NULL`)
	if ra := disp.Dispatch(ctx, "x-1-r1"); ra.Result != "dispatched" {
		t.Fatalf("ready: %+v", ra)
	}
	if dl := delivery(t, d, id); dl.Attempts != 1 {
		t.Fatalf("attempts %d", dl.Attempts)
	}
}

// The plan's status holds in the claim: a run that left it is not served.
func TestDispatchEligible(t *testing.T) {
	d, reg, getenv := msgSetup(t, Idle)
	ctx := context.Background()
	disp := &Dispatcher{DB: d, Adapters: reg, Getenv: getenv, Actor: msg.Actor{Name: "daemon"}}
	if !disp.Eligible(ctx, "x-1-r1") {
		t.Fatal("idle fake run not eligible")
	}
	for _, st := range []string{Waiting, Starting, Ended} {
		_, _ = d.Exec(`UPDATE runs SET status = ? WHERE id = 'x-1-r1'`, st)
		if disp.Eligible(ctx, "x-1-r1") {
			t.Fatalf("%s run eligible", st)
		}
	}
	if disp.Eligible(ctx, "nope") {
		t.Fatal("unknown run eligible")
	}
}

// A complete cleanup of an ended run removes its delivery inbox; a live run, or a
// failed cleanup, keeps it.
func TestCleanupDropsInbox(t *testing.T) {
	d, _, getenv := msgSetup(t, Idle)
	ctx := context.Background()
	inbox := filepath.Join(getenv("OTAG_DATA_DIR"), "runs", "x-1-r1", InboxDir)
	_ = os.MkdirAll(inbox, 0o700)
	_ = os.WriteFile(filepath.Join(inbox, ".mod-ready"), []byte("1"), 0o600)
	_ = os.WriteFile(filepath.Join(inbox, "n1.json"), []byte(`{"text":"x"}`), 0o600)
	m := &Manager{DB: d, Getenv: getenv}
	if err := m.settleCleanup(ctx, "x-1-r1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inbox); err != nil {
		t.Fatal("a live run's inbox was removed")
	}
	_, _ = d.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'stopped', cleanup_pending = 1 WHERE id = 'x-1-r1'`)
	if err := m.settleCleanup(ctx, "x-1-r1", errors.New("pane still there")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inbox); err != nil {
		t.Fatal("removed after a failed cleanup")
	}
	if err := m.settleCleanup(ctx, "x-1-r1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inbox); !os.IsNotExist(err) {
		t.Fatalf("inbox left after the run ended: %v", err)
	}
}

// A closed harness dialog of a new run moves the task back to running even when an
// earlier, gone run of the task left an ask open: only the run's own ask holds it.
func TestDialogIgnoresGoneRunsAsk(t *testing.T) {
	d, reg, _ := msgSetup(t, Running)
	ctx := context.Background()
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, _, err := msg.Ask(ctx, tx, msg.AskInput{RunID: "x-1-r1", Body: "which db?"}, msg.Actor{Name: "run:x-1-r1"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`
UPDATE runs SET status = 'ended', ended_reason = 'exited' WHERE id = 'x-1-r1';
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('x-1-r2', 1, 'x-1', 'child', 1, 1, 1, 'fake', 'running');
UPDATE tasks SET current_run_id = 'x-1-r2', status = 'running' WHERE id = 'x-1';
INSERT INTO sessions (run_id, harness) VALUES ('x-1-r2', 'fake');`); err != nil {
		t.Fatal(err)
	}
	hook := func(kind, payload string) {
		t.Helper()
		if _, err := HandleHook(ctx, d, reg, HookRequest{Harness: "fake", Kind: kind, RunID: "x-1-r2",
			Payload: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	hook("permission", `{"text":"Allow curl?"}`)
	if tk, _ := task.Get(ctx, d, "x-1"); tk.Status != task.Waiting {
		t.Fatalf("dialog: task %s", tk.Status)
	}
	hook("activity", `{}`) // the dialog closes
	if tk, _ := task.Get(ctx, d, "x-1"); tk.Status != task.Running {
		t.Fatalf("the gone run's ask held the task: %s", tk.Status)
	}
}

// An inbox recreated after cleanup (the mod's ready file) is swept once the run has
// been ended for the settle time.
func TestSweepInboxes(t *testing.T) {
	d, _, getenv := msgSetup(t, Idle)
	if _, err := d.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'done', cleanup_pending = 0, ended_at = ?
		WHERE id = 'x-1-r1'`, time.Now().Add(-time.Minute).UTC().Format(db.TimeFormat)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(paths.RunDir(getenv, "x-1-r1"), InboxDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, ".mod-ready"), []byte("1"), 0o600)
	if err := SweepInboxes(context.Background(), d, getenv, time.Now(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("inbox left: %v", err)
	}
}
