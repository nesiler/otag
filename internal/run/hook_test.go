package run

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/proc"
	"github.com/nesiler/otag/internal/tmux"
)

type otherAdapter struct{ *fake.Adapter }

func (otherAdapter) Harness() string { return "other" }

func setupRun(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'f', 'fake', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'p', 1, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (1, 1, 1, 'fake', 'h', '{}');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'x', '/x', 'x', 'h');
INSERT INTO tasks (id, project_id, title, status) VALUES ('x-1', 1, 't', 'running');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('x-1-r1', 1, 'x-1', 'child', 1, 1, 1, 'fake', 'starting');
UPDATE tasks SET current_run_id = 'x-1-r1';
INSERT INTO sessions (run_id, harness) VALUES ('x-1-r1', 'fake');`)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHookHarnessMismatchKeepsPayload(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	reg := adapter.NewRegistry(fake.New(), otherAdapter{fake.New()})
	req := HookRequest{Harness: "other", Kind: "session_start", RunID: "x-1-r1", Payload: []byte(`{"session_id":"s"}`)}
	if _, err := HandleHook(ctx, d, reg, req); err == nil {
		t.Fatal("mismatch accepted")
	}
	var raw int
	_ = d.QueryRow(`SELECT count(*) FROM raw_hook_payloads WHERE harness = 'other' AND run_id = 'x-1-r1'`).Scan(&raw)
	if r, _ := Get(ctx, d, "x-1-r1"); raw != 1 || r.Status != Starting {
		t.Fatalf("raw %d status %s", raw, r.Status)
	}

	req.Harness = "fake"
	resp, err := HandleHook(ctx, d, reg, req)
	if err != nil || resp.EventID == 0 || resp.RunID != "x-1-r1" {
		t.Fatalf("%+v %v", resp, err)
	}
	r, _ := Get(ctx, d, "x-1-r1")
	if r.Status != Idle || r.Session.HarnessSessionID != "s" || r.FirstActivityAt == "" {
		t.Fatalf("after session_start: %+v %+v", r.Status, r.Session)
	}
	// A new session id on the same run rotates the sessions row (Claude /clear).
	req.Payload = []byte(`{"session_id":"s2"}`)
	if _, err := HandleHook(ctx, d, reg, req); err != nil {
		t.Fatal(err)
	}
	var active, ended int
	_ = d.QueryRow(`SELECT count(*) FILTER (WHERE status = 'active'), count(*) FILTER (WHERE end_reason = 'replaced')
		FROM sessions WHERE run_id = 'x-1-r1'`).Scan(&active, &ended)
	if r, _ := Get(ctx, d, "x-1-r1"); active != 1 || ended != 1 || r.Session.HarnessSessionID != "s2" {
		t.Fatalf("rotation: active %d replaced %d", active, ended)
	}
}

func TestUsageKeepsLargestCounts(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	reg := adapter.NewRegistry(fake.New())
	for _, p := range []string{
		`{"model":"m","input_tokens":3,"output_tokens":200,"id":"msg-1"}`, // read while being written
		`{"model":"m","input_tokens":3,"output_tokens":206,"id":"msg-1"}`, // final
		`{"model":"m","input_tokens":3,"output_tokens":100,"id":"msg-1"}`, // stale report
	} {
		req := HookRequest{Harness: "fake", Kind: "usage", RunID: "x-1-r1", Payload: []byte(p)}
		if _, err := HandleHook(ctx, d, reg, req); err != nil {
			t.Fatal(err)
		}
	}
	var rows, in, out int64
	_ = d.QueryRow(`SELECT count(*), sum(input_tokens), sum(output_tokens) FROM run_usage WHERE run_id = 'x-1-r1'`).
		Scan(&rows, &in, &out)
	if rows != 1 || in != 3 || out != 206 {
		t.Fatalf("rows %d input %d output %d", rows, in, out)
	}
}

func TestLateHookRetriesEndedRunCleanup(t *testing.T) {
	d := setupRun(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "killed")
	bin := filepath.Join(dir, "tmux")
	// The pane's process: the fake tmux below does not kill it with the window, so the
	// cleanup has to (SIGKILL after killGrace).
	agent := exec.Command("/bin/sh", "-c", `trap "" HUP; sleep 30`)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })
	old := killGrace
	killGrace = 100 * time.Millisecond
	t.Cleanup(func() { killGrace = old })
	pid := agent.Process.Pid
	// A fake tmux keeps the regression independent of the user's server. Cleanup
	// verifies the recorded pid before killing the window and records the command.
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n*list-panes*) printf '%%s\\n' '%%1\t%d\t0\t0\t0\t@1\t\t\totag-x\tx-1\totag';;\n*kill-window*) : > '%s';;\nesac\n", pid, marker)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE runs SET status='ended',ended_reason='done',cleanup_pending=1 WHERE id='x-1-r1'; UPDATE sessions SET tmux_pane='%1',pid=? WHERE run_id='x-1-r1';`, pid); err != nil {
		t.Fatal(err)
	}
	client := tmux.New("test-only", "/dev/null")
	client.Bin = bin
	req := HookRequest{Harness: "fake", Kind: "activity", RunID: "x-1-r1", Payload: []byte(`{"tool":"Bash"}`), Tmux: client}
	if _, err := HandleHook(context.Background(), d, adapter.NewRegistry(fake.New()), req); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("terminal hook did not retry cleanup", err)
	}
	if same, err := proc.Same(pid, proc.Birth(pid)); same || err != nil {
		t.Fatalf("the pane's process survived the cleanup: %v %v", same, err)
	}
	var pending int
	var cerr sql.NullString
	_ = d.QueryRow(`SELECT cleanup_pending, cleanup_error FROM runs WHERE id = 'x-1-r1'`).Scan(&pending, &cerr)
	if pending != 0 || cerr.Valid {
		t.Fatalf("cleanup not settled: pending %d error %v", pending, cerr)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM tasks WHERE id='x-1'`).Scan(&status); err != nil || status != "running" {
		t.Fatal("cleanup changed task state", status, err)
	}
}
