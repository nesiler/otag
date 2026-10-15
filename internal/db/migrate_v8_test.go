package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

const v8Fixture = `
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'f', 'fake', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'p', 1, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (1, 1, 1, 'fake', 'h', '{}');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'p', '/p', 'p', 'h');
INSERT INTO tasks (id, project_id, title, status) VALUES ('p-1', 1, 'a', 'running'), ('p-2', 1, 'b', 'ready');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('p-1-r1', 1, 'p-1', 'child', 1, 1, 1, 'fake', 'running');
UPDATE tasks SET current_run_id = 'p-1-r1' WHERE id = 'p-1';`

// A v7 database with schema-v1 message rows upgrades to v8: rows keep their ids (a
// notification points at one), questions are closed, undelivered rows become expired
// deliveries and nothing is left to deliver.
func TestMigrateV7ToV8KeepsMessages(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) < 8 || migs[7].name != "0008_messaging.sql" {
		t.Fatalf("migration 8 is %v", migs[7].name)
	}
	v7, err := open(ctx, path, migs[:7], Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = v7.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(v8Fixture + `
INSERT INTO messages (id, project_id, kind, from_run_id, from_actor, to_run_id, to_task_id, to_actor, reply_to_id, body, status)
VALUES (10, 1, 'question', 'p-1-r1', 'run:p-1-r1', NULL, NULL, 'coordinator', NULL, 'which db?', 'answered'),
       (11, 1, 'answer', NULL, 'user:x', 'p-1-r1', NULL, '', 10, 'sqlite', 'acked'),
       (12, 1, 'answer', NULL, 'user:x', 'p-1-r1', NULL, '', 10, 'second answer', 'delivered'),
       (13, 1, 'info', NULL, '', NULL, 'p-2', '', NULL, '', 'pending'),
       (14, 1, 'answer', NULL, 'user:x', NULL, NULL, 'human', NULL, 'orphan answer', 'cancelled');
INSERT INTO notifications (project_id, message_id, kind, title) VALUES (1, 10, 'msg.question', 'q');`)
		return err
	})
	_ = v7.Close()
	if err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Version != d.Latest || d.Latest < 8 {
		t.Fatalf("version %d latest %d", d.Version, d.Latest)
	}
	type row struct {
		kind, sender, task, body, closed, rkind, recipient, state, run string
	}
	want := map[int]row{
		10: {"question", "run", "p-1", "which db?", "v1", "role", "coordinator", "read", ""},
		11: {"answer", "human", "", "sqlite", "", "run", "p-1-r1", "read", "p-1-r1"},
		12: {"info", "human", "", "second answer", "", "run", "p-1-r1", "delivered", "p-1-r1"},
		13: {"info", "human", "p-2", "(empty)", "", "task", "p-2", "expired", ""},
		14: {"info", "human", "", "orphan answer", "", "human", "human", "expired", ""},
	}
	rows, err := d.Query(`SELECT m.id, m.kind, m.sender_kind, coalesce(m.task_id, ''), m.body, coalesce(m.closed_reason, ''),
		d.recipient_kind, d.recipient, d.state, coalesce(d.run_id, '')
		FROM messages m JOIN message_deliveries d ON d.message_id = m.id ORDER BY m.id`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var id int
		var r row
		if err := rows.Scan(&id, &r.kind, &r.sender, &r.task, &r.body, &r.closed, &r.rkind, &r.recipient, &r.state, &r.run); err != nil {
			t.Fatal(err)
		}
		if want[id] != r {
			t.Errorf("message %d: %+v, want %+v", id, r, want[id])
		}
		n++
	}
	_ = rows.Close()
	if n != len(want) {
		t.Fatalf("%d rows", n)
	}
	var queued int
	_ = d.QueryRow(`SELECT count(*) FROM message_deliveries WHERE state IN ('queued', 'dispatched')`).Scan(&queued)
	if queued != 0 {
		t.Fatalf("%d v1 deliveries left to deliver", queued)
	}
	exec := func(q string) error {
		return d.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(q); return err })
	}
	// notifications.message_id still points at messages, by id.
	if exec(`INSERT INTO notifications (project_id, message_id, kind, title) VALUES (1, 999, 'k', 't')`) == nil {
		t.Fatal("notification of an unknown message accepted")
	}
	if err := exec(`INSERT INTO notifications (project_id, message_id, kind, title) VALUES (1, 11, 'k', 't')`); err != nil {
		t.Fatal(err)
	}
	// New ids continue after the v1 rows.
	if err := exec(`INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, body) VALUES (1, 'c1', 'info', 'human', 'hi')`); err != nil {
		t.Fatal(err)
	}
	var last int
	_ = d.QueryRow(`SELECT max(id) FROM messages`).Scan(&last)
	if last != 15 {
		t.Fatalf("next id %d", last)
	}
	checkV8Constraints(t, d)
}

// A fresh database (every migration from empty) has the v8 constraints.
func TestV8FromEmpty(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	for _, name := range []string{"messages", "message_deliveries", "message_proofs", "messages_one_open_ask",
		"messages_one_answer", "message_deliveries_touch"} {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("missing %s (err %v)", name, err)
		}
	}
	if err := d.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(v8Fixture); return err }); err != nil {
		t.Fatal(err)
	}
	checkV8Constraints(t, d)
}

func checkV8Constraints(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string) error {
		return d.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(q); return err })
	}
	mustFail := func(what, q string) {
		t.Helper()
		if exec(q) == nil {
			t.Errorf("%s accepted", what)
		}
	}
	ask := `INSERT INTO messages (id, project_id, client_msg_id, kind, sender_kind, sender_run_id, task_id, body, ask)
		VALUES (%d, 1, '%s', 'question', 'run', 'p-1-r1', 'p-1', 'q?', 1)`
	if err := exec(fmt.Sprintf(ask, 100, "ask-1")); err != nil {
		t.Fatal(err)
	}
	mustFail("second open ask of a task", fmt.Sprintf(ask, 101, "ask-2"))
	mustFail("duplicate client_msg_id", `INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, body)
		VALUES (1, 'ask-1', 'info', 'human', 'x')`)
	mustFail("ask from a human", `INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, task_id, body, ask)
		VALUES (1, 'h', 'question', 'human', 'p-1', 'x', 1)`)
	mustFail("run sender without run", `INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, body)
		VALUES (1, 'r', 'info', 'run', 'x')`)
	mustFail("answer without question", `INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, body)
		VALUES (1, 'a', 'answer', 'human', 'x')`)
	mustFail("empty body", `INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, body)
		VALUES (1, 'e', 'info', 'human', '')`)
	// Closing the ask frees the slot.
	if err := exec(`UPDATE messages SET closed_at = 'now', closed_reason = 'answered' WHERE id = 100`); err != nil {
		t.Fatal(err)
	}
	if err := exec(fmt.Sprintf(ask, 101, "ask-2")); err != nil {
		t.Fatalf("new ask after the first closed: %v", err)
	}
	if err := exec(`INSERT INTO messages (id, project_id, client_msg_id, kind, sender_kind, reply_to_id, body)
		VALUES (102, 1, 'ans-1', 'answer', 'human', 100, 'yes')`); err != nil {
		t.Fatal(err)
	}
	mustFail("second answer", `INSERT INTO messages (project_id, client_msg_id, kind, sender_kind, reply_to_id, body)
		VALUES (1, 'ans-2', 'answer', 'human', 100, 'no')`)

	dl := `INSERT INTO message_deliveries (message_id, project_id, recipient_kind, recipient, run_id, task_id, state,
		attempt_nonce, channel) VALUES `
	mustFail("run recipient without run", dl+`(102, 1, 'run', 'p-1-r1', NULL, 'p-1', 'queued', NULL, NULL)`)
	mustFail("task recipient without task", dl+`(102, 1, 'task', 'p-1', NULL, NULL, 'queued', NULL, NULL)`)
	mustFail("human recipient bound to a run", dl+`(102, 1, 'human', 'human', 'p-1-r1', NULL, 'queued', NULL, NULL)`)
	mustFail("dispatched without nonce", dl+`(102, 1, 'run', 'p-1-r1', 'p-1-r1', 'p-1', 'dispatched', NULL, 'api_start')`)
	mustFail("delivered without channel", dl+`(102, 1, 'run', 'p-1-r1', 'p-1-r1', 'p-1', 'delivered', NULL, NULL)`)
	mustFail("unknown state", dl+`(102, 1, 'run', 'p-1-r1', 'p-1-r1', 'p-1', 'sent', NULL, NULL)`)
	mustFail("unknown run", dl+`(102, 1, 'run', 'nope', 'nope', NULL, 'queued', NULL, NULL)`)
	if err := exec(dl + `(102, 1, 'run', 'p-1-r1', 'p-1-r1', 'p-1', 'dispatched', 'n1', 'api_start'),
		(102, 1, 'task', 'p-2', NULL, 'p-2', 'queued', NULL, NULL), (102, 1, 'role', 'coordinator', NULL, NULL, 'queued', NULL, NULL)`); err != nil {
		t.Fatal(err)
	}
	mustFail("same recipient twice", dl+`(102, 1, 'task', 'p-2', NULL, 'p-2', 'queued', NULL, NULL)`)
	if err := exec(`INSERT INTO message_proofs (project_id, run_id, ref) VALUES (1, 'p-1-r1', 'turn-1')`); err != nil {
		t.Fatal(err)
	}
	mustFail("duplicate proof", `INSERT INTO message_proofs (project_id, run_id, ref) VALUES (1, 'p-1-r1', 'turn-1')`)
	var check string
	if err := d.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity: %q %v", check, err)
	}
}
