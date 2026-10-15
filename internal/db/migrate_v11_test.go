package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A v10 database upgrades to v11: projects get the MR flow settings with their
// defaults (branch_prefix feature/ for new branches, push coordinator), a task that
// already has a worktree keeps its otag/<task> branch (backfilled from its first
// workspace), a task without one gets none, the review override and push record start
// empty, and reviews can be marked stale. The new CHECKs hold.
func TestMigrateV10ToV11MRFlow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) < 11 || migs[10].name != "0011_mr_flow.sql" {
		t.Fatalf("migration 11 is %v", migs[10].name)
	}
	v10, err := open(ctx, path, migs[:10], Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = v10.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(v8Fixture + `
INSERT INTO tasks (id, project_id, title, status, version, closed_at) VALUES ('p-3', 1, 'c', 'done', 4, '2026-10-01T10:00:00.000Z');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status, ended_reason)
VALUES ('p-3-r1', 1, 'p-3', 'child', 1, 1, 1, 'fake', 'ended', 'done'),
       ('p-3-r2', 1, 'p-3', 'child', 1, 1, 1, 'fake', 'ended', 'done');
INSERT INTO workspaces (project_id, task_id, run_id, mode, path, branch, status)
VALUES (1, 'p-3', 'p-3-r1', 'worktree', '/w/p-3', 'otag/p-3', 'kept'),
       (1, 'p-3', 'p-3-r2', 'worktree', '/w/p-3', 'otag/p-3', 'kept'),
       (1, 'p-1', 'p-1-r1', 'same', '/p', NULL, 'active');
INSERT INTO reviews (reviewed_task_id, reviewed_run_id, reviewer_actor, verdict) VALUES ('p-3', 'p-3-r2', 'user:x', 'accept');`)
		return err
	})
	_ = v10.Close()
	if err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Version != d.Latest || d.Latest < 11 {
		t.Fatalf("version %d latest %d", d.Version, d.Latest)
	}
	var base, prefix, remote, push string
	if err := d.QueryRow(`SELECT base, branch_prefix, remote, push FROM projects WHERE id = 1`).
		Scan(&base, &prefix, &remote, &push); err != nil {
		t.Fatal(err)
	}
	if base != "" || prefix != "feature/" || remote != "" || push != "coordinator" {
		t.Fatalf("project settings %q %q %q %q", base, prefix, remote, push)
	}
	branches := map[string]sql.NullString{}
	rows, err := d.Query(`SELECT id, branch FROM tasks`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var b sql.NullString
		if err := rows.Scan(&id, &b); err != nil {
			t.Fatal(err)
		}
		branches[id] = b
	}
	_ = rows.Close()
	if branches["p-3"].String != "otag/p-3" || branches["p-1"].Valid || branches["p-2"].Valid {
		t.Fatalf("branches %v", branches)
	}
	var version int
	var none int
	var reviewProfile, pushSHA sql.NullString
	_ = d.QueryRow(`SELECT version, review_none, review_profile_id, push_sha FROM tasks WHERE id = 'p-3'`).
		Scan(&version, &none, &reviewProfile, &pushSHA)
	if version != 4 || none != 0 || reviewProfile.Valid || pushSHA.Valid {
		t.Fatalf("p-3 version %d review_none %d review_profile %v push_sha %v", version, none, reviewProfile, pushSHA)
	}
	if _, err := d.Exec(`UPDATE projects SET push = 'everyone' WHERE id = 1`); err == nil {
		t.Fatal("push 'everyone' accepted")
	}
	if _, err := d.Exec(`UPDATE tasks SET review_none = 1, review_profile_id = 1 WHERE id = 'p-3'`); err == nil {
		t.Fatal("review none together with a review profile accepted")
	}
	if _, err := d.Exec(`UPDATE tasks SET review_profile_id = 99 WHERE id = 'p-3'`); err == nil {
		t.Fatal("review profile without a profile row accepted")
	}
	if _, err := d.Exec(`UPDATE reviews SET stale_at = '2026-10-02T00:00:00.000Z'`); err != nil {
		t.Fatal(err)
	}
	if rows, err := d.Query(`PRAGMA foreign_key_check`); err != nil || rows.Next() {
		t.Fatalf("foreign keys broken: %v", err)
	} else {
		_ = rows.Close()
	}
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM ready_tasks WHERE id = 'p-2' AND branch IS NULL`).Scan(&n)
	if n != 1 {
		t.Fatal("ready_tasks does not see the new columns")
	}
}
