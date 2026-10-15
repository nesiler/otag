package db

import (
	"context"
	"path/filepath"
	"testing"
)

// v9: one live coordinator per project (a unique index), and v_model_usage sums
// run_usage per reported model, falling back to the profile's model.
func TestV9CoordinatorIndexAndModelUsage(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Latest < 9 {
		t.Fatalf("latest %d", d.Latest)
	}
	if _, err := d.Exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'f', 'fake', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'p', 1, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config, model) VALUES (1, 1, 1, 'fake', 'h', '{}', 'conf-m');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'p', '/p', 'p', 'h');
INSERT INTO runs (id, project_id, role, profile_id, profile_revision_id, account_id, harness, status, ended_reason)
VALUES ('p-coord-r1', 1, 'coordinator', 1, 1, 1, 'fake', 'running', NULL),
       ('p-coord-r0', 1, 'coordinator', 1, 1, 1, 'fake', 'ended', 'stopped');
INSERT INTO run_usage (run_id, model, input_tokens, output_tokens, source) VALUES
  ('p-coord-r1', 'm1', 10, 1, 'transcript'), ('p-coord-r1', 'm1', 5, 1, 'transcript'),
  ('p-coord-r0', '', 7, 2, 'transcript');`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO runs (id, project_id, role, profile_id, profile_revision_id, account_id, harness, status)
		VALUES ('p-coord-r2', 1, 'coordinator', 1, 1, 1, 'fake', 'queued')`); err == nil {
		t.Fatal("a second live coordinator was accepted")
	}
	got := map[string][2]int{}
	rows, err := d.Query(`SELECT model, runs, input_tokens FROM v_model_usage`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m string
		var runs, in int
		if err := rows.Scan(&m, &runs, &in); err != nil {
			t.Fatal(err)
		}
		got[m] = [2]int{runs, in}
	}
	if got["m1"] != [2]int{1, 15} || got["conf-m"] != [2]int{1, 7} || len(got) != 2 {
		t.Fatalf("v_model_usage %v", got)
	}
}
