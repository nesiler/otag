package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
)

func TestMigrateEmptyAndRerun(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != d.Latest || d.Latest < 1 {
		t.Fatalf("version %d, latest %d", d.Version, d.Latest)
	}
	for _, name := range []string{"accounts", "profiles", "projects", "tasks", "task_deps", "runs", "sessions",
		"workspaces", "run_outputs", "run_usage", "reviews", "messages", "notifications", "events",
		"raw_hook_payloads", "quota_snapshots", "profile_state", "account_state", "profile_fallbacks",
		"profile_revisions", "request_log", "ready_tasks", "v_profile_stats", "v_run_metrics", "v_status_durations"} {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("missing %s (err %v)", name, err)
		}
	}
	var fk int
	if err := d.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v", fk, err)
	}
	var mode string
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	_ = d.Close()

	// Re-open: no-op migration, same version.
	d2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d2.Close() }()
	if d2.Version != d2.Latest {
		t.Fatalf("version after reopen %d", d2.Version)
	}
	// Views are valid SQL.
	for _, v := range []string{"ready_tasks", "v_profile_stats", "v_run_metrics", "v_status_durations"} {
		if _, err := d2.Exec("SELECT * FROM " + v); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
}

func TestUpdatedAtTrigger(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO accounts (name, harness, config_hash, updated_at) VALUES ('a', 'claude', 'h', '2000-01-01T00:00:00.000Z')`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE accounts SET config_hash = 'h2' WHERE name = 'a'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var u string
	_ = d.QueryRow(`SELECT updated_at FROM accounts WHERE name = 'a'`).Scan(&u)
	if u == "2000-01-01T00:00:00.000Z" {
		t.Fatal("updated_at not touched")
	}
}

func TestAnalyticsViews(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'c1', 'claude', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'sonnet', 1, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (1, 1, 1, 'claude', 'h', '{}');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'p', '/p', 'p', 'h');
INSERT INTO tasks (id, project_id, title) VALUES ('p-1', 1, 'a'), ('p-2', 1, 'b');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness,
                  queued_at, started_at, ended_at)
VALUES ('r1', 1, 'p-1', 'child', 1, 1, 1, 'claude', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:10.000Z', '2026-01-01T00:01:10.000Z'),
       ('r2', 1, 'p-2', 'child', 1, 1, 1, 'claude', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z', '2026-01-01T00:02:00.000Z');
INSERT INTO run_usage (run_id, input_tokens, output_tokens, source)
VALUES ('r1', 100, 10, 'transcript'), ('r1', 50, 5, 'transcript'), ('r2', 1, 1, 'estimate');
INSERT INTO reviews (reviewed_task_id, reviewed_run_id, verdict)
VALUES ('p-1', 'r1', 'reject'), ('p-1', 'r1', 'accept'), ('p-2', 'r2', 'accept'), ('p-2', 'r2', 'accept');`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var runs, in, out int
	var avgDur, rate float64
	err = d.QueryRow(`SELECT runs, input_tokens, output_tokens, avg_duration_s, reject_rate
		FROM v_profile_stats WHERE profile = 'sonnet'`).Scan(&runs, &in, &out, &avgDur, &rate)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 2 || in != 151 || out != 16 || int(avgDur+0.5) != 90 || rate != 0.25 {
		t.Fatalf("stats runs=%d in=%d out=%d avg=%f rate=%f", runs, in, out, avgDur, rate)
	}
}

func TestCrossEntityConsistency(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	exec := func(q string) error {
		return d.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(q); return err })
	}
	if err := exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'c1', 'claude', 'h'), (2, 'cx', 'codex', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'a', 1, 'h'), (2, 'b', 2, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config)
VALUES (1, 1, 1, 'claude', 'h', '{}'), (2, 2, 2, 'codex', 'h', '{}');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'p', '/p', 'p', 'h'), (2, 'q', '/q', 'q', 'h');
INSERT INTO tasks (id, project_id, title) VALUES ('p-1', 1, 'a'), ('p-2', 1, 'b'), ('q-1', 2, 'c');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness)
VALUES ('r1', 1, 'p-1', 'child', 1, 1, 1, 'claude');`); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"run task of other project": `INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness)
			VALUES ('x', 2, 'p-1', 'child', 1, 1, 1, 'claude')`,
		"revision of other profile": `INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness)
			VALUES ('x', 1, 'p-1', 'child', 1, 2, 1, 'claude')`,
		"wrong account for revision": `INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness)
			VALUES ('x', 1, 'p-1', 'child', 1, 1, 2, 'claude')`,
		"parent in other project": `INSERT INTO runs (id, project_id, task_id, role, parent_run_id, profile_id, profile_revision_id, account_id, harness)
			VALUES ('x', 2, 'q-1', 'child', 'r1', 1, 1, 1, 'claude')`,
		"current run of other task": `UPDATE tasks SET current_run_id = 'r1' WHERE id = 'p-2'`,
		"review run of other task":  `INSERT INTO reviews (reviewed_task_id, reviewed_run_id, verdict) VALUES ('p-2', 'r1', 'accept')`,
		"event task/project mismatch": `INSERT INTO events (source, kind, entity_type, entity_id, project_id, task_id)
			VALUES ('t', 'k', 'task', 'p-1', 2, 'p-1')`,
	}
	for name, q := range bad {
		if err := exec(q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := exec(`UPDATE tasks SET current_run_id = 'r1' WHERE id = 'p-1'`); err != nil {
		t.Fatalf("valid current_run_id: %v", err)
	}
}

func TestWriteRefusedAfterConcurrentUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	err = old.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO accounts (name, harness, config_hash) VALUES ('x', 'claude', 'h')`)
		return err
	})
	if apperr.Code(err) != apperr.SchemaTooNew {
		t.Fatalf("got %v", err)
	}
}

func TestNewerSchemaNotTouched(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA journal_mode = DELETE; CREATE TABLE future (x); PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !d.ReadOnly() || d.Version != 999 {
		t.Fatalf("readOnly=%v version=%d", d.ReadOnly(), d.Version)
	}
	_ = d.Close()

	raw, _ = sql.Open("sqlite", path)
	defer func() { _ = raw.Close() }()
	var mode string
	_ = raw.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "delete" {
		t.Fatalf("journal_mode changed to %q", mode)
	}
}

func TestPathEscapingAndPermissions(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "new dir")
	path := filepath.Join(dir, "odd?name#with%25.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("db not created at the literal path: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
	if dst, _ := os.Stat(dir); dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dst.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "odd?name#with%25.db") {
			t.Fatalf("unexpected file %q", e.Name())
		}
	}
	_ = os.Chmod(path, 0o644)
	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode not tightened: %v", st.Mode().Perm())
	}
}

func TestRefuseWriteOnNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if !d.ReadOnly() {
		t.Fatal("expected read-only")
	}
	err = d.Tx(ctx, func(*sql.Tx) error { return nil })
	if apperr.Code(err) != apperr.SchemaTooNew {
		t.Fatalf("got %v", err)
	}
	if _, err := d.Exec(`INSERT INTO accounts (name, harness, config_hash) VALUES ('x', 'claude', 'h')`); err == nil {
		t.Fatal("raw write should fail on query_only connection")
	}
	var n int
	if err := d.QueryRow("SELECT count(*) FROM tasks").Scan(&n); err != nil {
		t.Fatalf("reads must still work: %v", err)
	}
}
