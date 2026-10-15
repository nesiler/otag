package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateV1DataToLatest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	v1, err := open(ctx, path, migs[:1], Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = v1.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'c1', 'claude', 'h');
INSERT INTO account_state (account_id) VALUES (1);
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'sonnet', 1, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (1, 1, 1, 'claude', 'h', '{}');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'p', '/p', 'p', 'h');
INSERT INTO projects (id, name, path, prefix, config_hash, config) VALUES (2, 's', '/s', 's', 'h', '{"guard":"strict"}');
INSERT INTO tasks (id, project_id, title) VALUES ('p-1', 1, 'a');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('r1', 1, 'p-1', 'child', 1, 1, 1, 'claude', 'ended'),
       ('r2', 1, 'p-1', 'child', 1, 1, 1, 'claude', 'running');
UPDATE tasks SET current_run_id = 'r2' WHERE id = 'p-1';
INSERT INTO sessions (id, run_id, harness, harness_session_id, tmux_pane) VALUES (1, 'r2', 'claude', 'sid', '%1');
INSERT INTO run_usage (run_id, session_id, input_tokens, source) VALUES ('r2', 1, 7, 'transcript');
INSERT INTO events (source, kind, entity_type, entity_id, project_id, run_id) VALUES ('t', 'k', 'run', 'r2', 1, 'r2');`)
		return err
	})
	_ = v1.Close()
	if err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Version != d.Latest {
		t.Fatalf("version %d", d.Version)
	}
	var reason, pane, aux string
	var tokens int
	err = d.QueryRow(`SELECT r.ended_reason, s.tmux_pane, s.aux_panes, u.input_tokens
		FROM runs r, sessions s, run_usage u WHERE r.id = 'r1' AND s.id = 1 AND u.session_id = 1`).
		Scan(&reason, &pane, &aux, &tokens)
	if err != nil || reason != "crashed" || pane != "%1" || aux != "[]" || tokens != 7 {
		t.Fatalf("data after migration: %v %q %q %q %d", err, reason, pane, aux, tokens)
	}
	// v7: projects default to light, one that said strict before keeps it; older runs
	// have no level (strict).
	var guards string
	var runGuards int
	if err := d.QueryRow(`SELECT group_concat(guard, ','), (SELECT count(guard) FROM runs) FROM (SELECT guard FROM projects ORDER BY id)`).
		Scan(&guards, &runGuards); err != nil || guards != "light,strict" || runGuards != 0 {
		t.Fatalf("guard after migration: %q %d %v", guards, runGuards, err)
	}
	var fk int
	if err := d.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {

		t.Fatalf("foreign_keys after migration = %d, %v", fk, err)
	}
	var check string
	if err := d.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity: %q %v", check, err)
	}
	exec := func(q string) error {
		return d.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(q); return err })
	}
	// Foreign keys still point at the rebuilt tables.
	if exec(`INSERT INTO sessions (run_id, harness) VALUES ('nope', 'claude')`) == nil {
		t.Fatal("session of unknown run accepted")
	}
	if exec(`UPDATE tasks SET current_run_id = 'nope' WHERE id = 'p-1'`) == nil {
		t.Fatal("dangling current_run_id accepted")
	}
	// One active run per task (v3).
	if exec(`INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
		VALUES ('dup', 1, 'p-1', 'child', 1, 1, 1, 'claude', 'starting')`) == nil {
		t.Fatal("second active run of a task accepted")
	}
	if err := exec(`UPDATE runs SET status = 'ended', ended_reason = 'done' WHERE id = 'r2'`); err != nil {
		t.Fatal(err)
	}
	// harness is free text now (validated in Go); idle is a run status.
	err = exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (2, 'f', 'fake', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (2, 'fk', 2, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (2, 2, 2, 'fake', 'h', '{}');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('r3', 1, 'p-1', 'child', 2, 2, 2, 'fake', 'idle');`)
	if err != nil {
		t.Fatal(err)
	}
	if exec(`UPDATE runs SET status = 'ended' WHERE id = 'r3'`) == nil {
		t.Fatal("ended run without ended_reason accepted")
	}
	for _, v := range []string{"v_profile_stats", "v_run_metrics"} {
		if _, err := d.Exec("SELECT * FROM " + v); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
}
