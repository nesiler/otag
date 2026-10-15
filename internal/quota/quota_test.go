package quota

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (1, 'c1', 'claude', 'h'), (2, 'oc', 'opencode', 'h'),
       (3, 'cx', 'codex', 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (1, 'opus', 1, 'h'), (2, 'flash', 2, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config)
VALUES (1, 1, 1, 'claude', 'h', '{}'), (2, 2, 2, 'opencode', 'h', '{}');
INSERT INTO projects (id, name, path, prefix, config_hash) VALUES (1, 'x', '/x', 'x', 'h');
INSERT INTO tasks (id, project_id, title, status) VALUES ('x-1', 1, 't', 'running'), ('x-2', 1, 't', 'running');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('x-1-r1', 1, 'x-1', 'child', 1, 1, 1, 'claude', 'running'),
       ('x-2-r1', 1, 'x-2', 'child', 2, 2, 2, 'opencode', 'running');`)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func record(t *testing.T, d *db.DB, o Observation) int {
	t.Helper()
	var n int
	err := d.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		n, err = Record(context.Background(), tx, o)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func count(t *testing.T, d *db.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func cooldown(t *testing.T, d *db.DB, table, key string, id int64) string {
	t.Helper()
	var s sql.NullString
	err := d.QueryRow(`SELECT cooldown_until FROM `+table+` WHERE `+key+` = ?`, id).Scan(&s)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return s.String
}

func TestRecordTouchesUnchangedAndDropsOlder(t *testing.T) {
	d := testDB(t)
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(3 * time.Hour)
	obs := func(at time.Time, used float64) Observation {
		return Observation{AccountID: 1, Source: SourceClaudeStatusline, ObservedAt: at,
			Windows: []Window{{Name: "5h", UsedPercent: ptr(used), ResetsAt: reset}}}
	}
	record(t, d, obs(t0, 20))
	record(t, d, obs(t0.Add(time.Minute), 20.001)) // same after rounding: touched
	if n := count(t, d, `SELECT count(*) FROM quota_snapshots`); n != 1 {
		t.Fatalf("unchanged values inserted a row: %d rows", n)
	}
	var at string
	_ = d.QueryRow(`SELECT observed_at FROM quota_snapshots`).Scan(&at)
	if at != fmtTime(t0.Add(time.Minute)) {
		t.Fatalf("not touched: %s", at)
	}
	record(t, d, obs(t0.Add(2*time.Minute), 25))
	if n := record(t, d, obs(t0.Add(30*time.Second), 99)); n != 0 {
		t.Fatal("an observation older than the latest was recorded")
	}
	if n := count(t, d, `SELECT count(*) FROM quota_snapshots`); n != 2 {
		t.Fatalf("rows %d", n)
	}
	if c := cooldown(t, d, "account_state", "account_id", 1); c != "" {
		t.Fatalf("cooldown below 100%%: %s", c)
	}
}

func TestExhaustedWindowCoolsDownAccountNeverShorter(t *testing.T) {
	d := testDB(t)
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	late, early := t0.Add(5*time.Hour), t0.Add(time.Hour)
	record(t, d, Observation{AccountID: 3, Source: SourceCodexRollout, ObservedAt: t0,
		Windows: []Window{{Name: "5h", UsedPercent: ptr(100), ResetsAt: late}}})
	if c := cooldown(t, d, "account_state", "account_id", 3); c != fmtTime(late) {
		t.Fatalf("cooldown %q", c)
	}
	record(t, d, Observation{AccountID: 3, Source: SourceCodexRollout, ObservedAt: t0.Add(time.Minute),
		Windows: []Window{{Name: "7d", UsedPercent: ptr(100), ResetsAt: early}}})
	if c := cooldown(t, d, "account_state", "account_id", 3); c != fmtTime(late) {
		t.Fatalf("cooldown shortened to %q", c)
	}
	// Profile-level observations cool the profile, not the account.
	record(t, d, Observation{AccountID: 2, ProfileID: 2, Source: SourceLiteLLM, ObservedAt: t0,
		Windows: []Window{{Name: "budget", Used: ptr(5), Reached: true}}})
	if c := cooldown(t, d, "profile_state", "profile_id", 2); c != fmtTime(t0.Add(DefaultCooldown)) {
		t.Fatalf("profile cooldown %q", c)
	}
	if c := cooldown(t, d, "account_state", "account_id", 2); c != "" {
		t.Fatalf("account cooled by a profile observation: %q", c)
	}
}

func TestRecordHit(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	hit := func(h Hit) Target {
		t.Helper()
		var tg Target
		if err := d.Tx(ctx, func(tx *sql.Tx) (err error) { tg, err = RecordHit(ctx, tx, h); return err }); err != nil {
			t.Fatal(err)
		}
		return tg
	}
	// No future reset (a transient 429 the harness retries): nothing.
	if tg := hit(Hit{RunID: "x-1-r1", Text: "rate_limit", At: t0}); tg != (Target{}) {
		t.Fatalf("target %+v", tg)
	}
	hit(Hit{RunID: "x-1-r1", ResetsAt: t0.Add(-time.Minute), At: t0})
	if count(t, d, `SELECT count(*) FROM quota_snapshots`) != 0 {
		t.Fatal("hit without reset recorded")
	}
	// Claude: the account is the target; the snapshot does not set a cooldown itself.
	reset := t0.Add(2 * time.Hour)
	if tg := hit(Hit{RunID: "x-1-r1", ResetsAt: reset, At: t0}); tg != (Target{AccountID: 1}) {
		t.Fatalf("claude target %+v", tg)
	}
	if count(t, d, `SELECT count(*) FROM quota_snapshots WHERE source = 'rate_limit' AND profile_id IS NULL
		AND resets_at = ?`, fmtTime(reset)) != 1 {
		t.Fatal("claude hit snapshot")
	}
	if c := cooldown(t, d, "account_state", "account_id", 1); c != "" {
		t.Fatalf("RecordHit wrote a cooldown: %q", c)
	}
	// OpenCode: the profile is the target, and the snapshot is the profile's.
	r2 := t0.Add(10 * time.Minute)
	if tg := hit(Hit{RunID: "x-2-r1", ResetsAt: r2, Text: "provider.rate-limit", At: t0}); tg != (Target{ProfileID: 2}) {
		t.Fatalf("opencode target %+v", tg)
	}
	if n := count(t, d, `SELECT count(*) FROM quota_snapshots WHERE source = 'rate_limit' AND profile_id = 2
		AND used_percent = 100 AND json_extract(raw, '$.reached') AND resets_at = ?`, fmtTime(r2)); n != 1 {
		t.Fatalf("hit snapshot rows %d", n)
	}
}

func TestListStaleAndResetPassed(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	record(t, d, Observation{AccountID: 1, Source: SourceClaudeStatusline, ObservedAt: now.Add(-3 * time.Hour),
		Windows: []Window{
			{Name: "5h", UsedPercent: ptr(95), ResetsAt: now.Add(-time.Hour)},     // rolled over since
			{Name: "7d", UsedPercent: ptr(50), ResetsAt: now.Add(48 * time.Hour)}, // still valid, but old
		}})
	record(t, d, Observation{AccountID: 2, Source: SourceLiteLLM, ObservedAt: now.Add(-time.Minute),
		Windows: []Window{{Name: "budget-30d", UsedPercent: ptr(16), Used: ptr(16), Limit: ptr(100)}}})
	record(t, d, Observation{AccountID: 3, Source: SourceCodexRollout, ObservedAt: now.Add(-10 * 24 * time.Hour),
		Windows: []Window{{Name: "5h", UsedPercent: ptr(10)}}})
	rows, err := List(ctx, d, Filter{MaxAge: 7 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	five, week, budget := rows[0], rows[1], rows[2]
	if five.Window != "5h" || !five.Stale || !five.ResetPassed || *five.Effective() != 0 || *five.UsedPercent != 95 {
		t.Fatalf("5h %+v", five)
	}
	if week.ResetPassed || !week.Stale || *week.Effective() != 50 || *week.ResetsInSeconds != 48*3600 {
		t.Fatalf("7d %+v", week)
	}
	if budget.Stale || budget.AgeSeconds != 60 || budget.ResetsAt != nil || *budget.Limit != 100 {
		t.Fatalf("budget %+v", budget)
	}
	all, _ := List(ctx, d, Filter{}, now)
	if len(all) != 4 {
		t.Fatalf("all %d", len(all))
	}
}

func TestPruneKeepsLatestPerWindow(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		record(t, d, Observation{AccountID: 1, Source: SourceClaudeStatusline, ObservedAt: old.Add(time.Duration(i) * time.Hour),
			Windows: []Window{{Name: "5h", UsedPercent: ptr(float64(i))}, {Name: "7d", UsedPercent: ptr(float64(i))}}})
	}
	n, err := Prune(ctx, d, old.Add(30*24*time.Hour))
	if err != nil || n != 4 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if c := count(t, d, `SELECT count(*) FROM quota_snapshots WHERE used_percent = 2`); c != 2 {
		t.Fatalf("latest rows %d", c)
	}
}

func TestWindowName(t *testing.T) {
	for m, want := range map[int64]string{300: "5h", 10080: "7d", 1440: "1d", 90: "90m", 0: "window", 60: "1h"} {
		if got := WindowName(m); got != want {
			t.Errorf("%d: %s", m, got)
		}
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
