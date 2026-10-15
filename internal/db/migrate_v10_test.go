package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A v9 database upgrades to v10: tasks is rebuilt with its rows, versions and the rows
// that point at it (runs, workspaces, messages, deps) intact; delivered is a status
// (only with delivered_at); ready_tasks treats a delivered blocker as finished and
// v_task_delivery reports the done -> delivered wait without review tasks.
func TestMigrateV9ToV10Delivery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) < 10 || migs[9].name != "0010_delivery.sql" {
		t.Fatalf("migration 10 is %v", migs[9].name)
	}
	v9, err := open(ctx, path, migs[:9], Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = v9.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(v8Fixture + `
INSERT INTO tasks (id, project_id, title, status, version, closed_at, metadata)
VALUES ('p-3', 1, 'c', 'done', 7, '2026-10-01T10:00:00.000Z', '{"k":1}'),
       ('p-4', 1, 'Review: c', 'done', 3, '2026-10-01T09:00:00.000Z', '{}');
INSERT INTO task_deps (task_id, depends_on_id, kind) VALUES ('p-2', 'p-3', 'blocks'), ('p-4', 'p-3', 'review_of');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status, ended_reason)
VALUES ('p-3-r1', 1, 'p-3', 'child', 1, 1, 1, 'fake', 'ended', 'done');
INSERT INTO workspaces (project_id, task_id, run_id, mode, path, branch, status)
VALUES (1, 'p-3', 'p-3-r1', 'worktree', '/w/p-3', 'otag/p-3', 'kept');
INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, task_id, body)
VALUES (1, 'c:1', 'info', 'system', 'p-3', 'hello');`)
		return err
	})
	_ = v9.Close()
	if err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Version != d.Latest || d.Latest < 10 {
		t.Fatalf("version %d latest %d", d.Version, d.Latest)
	}
	var n int
	var version int
	var meta, closed string
	_ = d.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n)
	_ = d.QueryRow(`SELECT version, metadata, closed_at FROM tasks WHERE id = 'p-3'`).Scan(&version, &meta, &closed)
	if n != 4 || version != 7 || meta != `{"k":1}` || closed != "2026-10-01T10:00:00.000Z" {
		t.Fatalf("tasks %d, p-3 version %d meta %s closed %s", n, version, meta, closed)
	}
	if rows, err := d.Query(`PRAGMA foreign_key_check`); err != nil || rows.Next() {
		t.Fatalf("foreign keys broken: %v", err)
	} else {
		_ = rows.Close()
	}
	// p-2 waits for its blocker p-3 (done counts as finished already).
	_ = d.QueryRow(`SELECT count(*) FROM ready_tasks WHERE id = 'p-2'`).Scan(&n)
	if n != 1 {
		t.Fatal("ready_tasks lost p-2")
	}
	if _, err := d.Exec(`UPDATE tasks SET status = 'delivered' WHERE id = 'p-3'`); err == nil {
		t.Fatal("delivered without delivered_at accepted")
	}
	if _, err := d.Exec(`UPDATE tasks SET status = 'delivered', delivered_at = '2026-10-01T12:30:00.000Z' WHERE id = 'p-3'`); err != nil {
		t.Fatal(err)
	}
	_ = d.QueryRow(`SELECT count(*) FROM ready_tasks WHERE id = 'p-2'`).Scan(&n)
	if n != 1 {
		t.Fatal("a delivered blocker holds p-2")
	}
	var updated, created string
	_ = d.QueryRow(`SELECT updated_at, created_at FROM tasks WHERE id = 'p-3'`).Scan(&updated, &created)
	if updated == created {
		t.Fatal("tasks_touch trigger missing")
	}
	var task, status string
	var wait sql.NullFloat64
	rows, err := d.Query(`SELECT task_id, status, wait_s FROM v_task_delivery`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n = 0
	for rows.Next() {
		n++
		if err := rows.Scan(&task, &status, &wait); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 || task != "p-3" || status != "delivered" || !wait.Valid || int(wait.Float64+0.5) != 9000 {
		t.Fatalf("v_task_delivery: %d rows, %s %s %v", n, task, status, wait)
	}
}
