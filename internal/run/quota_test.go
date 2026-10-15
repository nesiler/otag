package run

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/db"
)

func hook(t *testing.T, d *db.DB, kind, payload string) {
	t.Helper()
	req := HookRequest{Harness: "fake", Kind: kind, RunID: "x-1-r1", Payload: []byte(payload)}
	if _, err := HandleHook(context.Background(), d, adapter.NewRegistry(fake.New()), req); err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
}

func runState(t *testing.T, d *db.DB) (status, reason, taskStatus string) {
	t.Helper()
	_ = d.QueryRow(`SELECT r.status, coalesce(r.ended_reason, ''), t.status FROM runs r JOIN tasks t ON t.id = r.task_id
		WHERE r.id = 'x-1-r1'`).Scan(&status, &reason, &taskStatus)
	return
}

func cooldowns(t *testing.T, d *db.DB) (account, profile string) {
	t.Helper()
	_ = d.QueryRow(`SELECT coalesce((SELECT cooldown_until FROM account_state WHERE account_id = 1), ''),
		coalesce((SELECT cooldown_until FROM profile_state WHERE profile_id = 1), '')`).Scan(&account, &profile)
	return
}

// A rate limit the harness retries does not end the run, even if the turn later fails
// for another reason; a turn that fails right after a limit ends it as quota.
func TestRateLimitedTurnEndsRunAsQuota(t *testing.T) {
	d := setupRun(t)
	hook(t, d, "session_start", `{"session_id":"s"}`)
	hook(t, d, "turn_start", `{}`)
	hook(t, d, "rate_limit", `{"text":"429","retry":true}`)
	hook(t, d, "turn_end", `{"reason":"error","text":"transport"}`)
	if s, _, ts := runState(t, d); s != Idle || ts != "running" {
		t.Fatalf("retried limit ended the run: %s %s", s, ts)
	}
	if a, _ := cooldowns(t, d); a != "" {
		t.Fatalf("transient limit without reset set a cooldown: %s", a)
	}

	hook(t, d, "turn_start", `{}`)
	hook(t, d, "rate_limit", `{"text":"usage limit reached"}`)
	if s, _, _ := runState(t, d); s != Running {
		t.Fatalf("a limit alone ended the turn: %s", s)
	}
	hook(t, d, "turn_end", `{"reason":"error"}`)
	s, reason, ts := runState(t, d)
	if s != Ended || reason != ReasonQuota || ts != "ready" {
		t.Fatalf("after failed turn: %s %s task %s", s, reason, ts)
	}
	var errText, sess string
	_ = d.QueryRow(`SELECT error, (SELECT status FROM sessions WHERE run_id = 'x-1-r1' ORDER BY id DESC LIMIT 1)
		FROM runs WHERE id = 'x-1-r1'`).Scan(&errText, &sess)
	if !strings.Contains(errText, "usage limit reached") || sess != "ended" {
		t.Fatalf("error %q session %s", errText, sess)
	}
	// The fake harness's limits are per model (quota.HitTarget): only the profile
	// cools down, so other profiles of the account stay available.
	a, p := cooldowns(t, d)
	until, err := time.Parse(db.TimeFormat, p)
	if err != nil || a != "" || until.Before(time.Now().Add(DefaultCooldown-time.Minute)) {
		t.Fatalf("cooldown account %q profile %q", a, p)
	}
	if ok, why := ProfileAvailable(context.Background(), d, 1); ok || !strings.Contains(why, "profile p cooling down") {
		t.Fatalf("available during cooldown: %v %s", ok, why)
	}
}

// accountLike is the fake harness under the name of another harness.
type accountLike struct {
	*fake.Adapter
	name string
}

func (a accountLike) Harness() string { return a.name }

// The quota end cools the same target as quota.RecordHit, and only an existing
// cooldown of that target replaces the default.
func TestQuotaEndCooldownTarget(t *testing.T) {
	ctx := context.Background()
	future := time.Now().Add(2 * time.Hour).UTC().Format(db.TimeFormat)
	for _, tc := range []struct {
		harness                  string
		preAccount, preProfile   string // existing cooldowns
		wantAccount, wantProfile string // "default" = about now + DefaultCooldown
	}{
		{harness: "claude", wantAccount: "default"},
		{harness: "codex", preAccount: future, wantAccount: future},
		{harness: "codex", preProfile: future, wantAccount: "default", wantProfile: future},
		{harness: "fake", wantProfile: "default"},
		// Another route of the same account cooled the account: this profile still
		// gets its own cooldown, and the account's is left alone.
		{harness: "fake", preAccount: future, wantAccount: future, wantProfile: "default"},
		{harness: "fake", preProfile: future, wantProfile: future},
	} {
		d := setupRun(t)
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`PRAGMA defer_foreign_keys = ON;
				UPDATE accounts SET harness = ?1; UPDATE profile_revisions SET harness = ?1;
				UPDATE runs SET harness = ?1; UPDATE sessions SET harness = ?1`, tc.harness)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if tc.preAccount != "" {
			_, _ = d.Exec(`INSERT INTO account_state (account_id, cooldown_until) VALUES (1, ?)`, tc.preAccount)
		}
		if tc.preProfile != "" {
			_, _ = d.Exec(`INSERT INTO profile_state (profile_id, cooldown_until) VALUES (1, ?)`, tc.preProfile)
		}
		reg := adapter.NewRegistry(accountLike{fake.New(), tc.harness})
		for _, ev := range [][2]string{{"session_start", `{"session_id":"s"}`}, {"turn_start", `{}`},
			{"rate_limit", `{"text":"limit"}`}, {"turn_end", `{"reason":"error"}`}} {
			req := HookRequest{Harness: tc.harness, Kind: ev[0], RunID: "x-1-r1", Payload: []byte(ev[1])}
			if _, err := HandleHook(ctx, d, reg, req); err != nil {
				t.Fatal(err)
			}
		}
		if _, reason, _ := runState(t, d); reason != ReasonQuota {
			t.Fatalf("%s: ended %q", tc.harness, reason)
		}
		a, p := cooldowns(t, d)
		check := func(what, got, want string) {
			t.Helper()
			if want == "default" {
				u, err := time.Parse(db.TimeFormat, got)
				if err != nil || u.Before(time.Now().Add(DefaultCooldown-time.Minute)) || u.After(time.Now().Add(DefaultCooldown)) {
					t.Fatalf("%+v: %s cooldown %q, want the default", tc, what, got)
				}
				return
			}
			if got != want {
				t.Fatalf("%+v: %s cooldown %q, want %q", tc, what, got, want)
			}
		}
		check("account", a, tc.wantAccount)
		check("profile", p, tc.wantProfile)
	}
}

// A known reset time is the cooldown; quota does not stretch it to the default.
func TestRateLimitResetIsCooldown(t *testing.T) {
	d := setupRun(t)
	reset := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	hook(t, d, "session_start", `{"session_id":"s"}`)
	hook(t, d, "turn_start", `{}`)
	hook(t, d, "rate_limit", `{"text":"limit","resets_at":"`+reset.Format(time.RFC3339)+`"}`)
	hook(t, d, "turn_end", `{"reason":"error"}`)
	if _, reason, _ := runState(t, d); reason != ReasonQuota {
		t.Fatalf("reason %s", reason)
	}
	// The fake harness's limits are per model (quota.RecordHit): only the profile
	// cools down, and the quota end does not add the default to the account.
	a, p := cooldowns(t, d)
	if want := reset.Format(db.TimeFormat); a != "" || p != want {
		t.Fatalf("cooldown %q/%q, want profile %s", a, p, want)
	}
	// Cooldowns only grow.
	if err := SetCooldown(context.Background(), d, 0, 1, time.Now(), "earlier"); err != nil {
		t.Fatal(err)
	}
	if _, p2 := cooldowns(t, d); p2 != p {
		t.Fatalf("cooldown shrank to %s", p2)
	}
}

func TestProfileAvailable(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(d).UTC().Format(db.TimeFormat) }
	check := func(want bool, contains string) {
		t.Helper()
		ok, why := Availability{Now: func() time.Time { return now }}.Check(ctx, d, 1)
		if ok != want || !strings.Contains(why, contains) {
			t.Fatalf("available %v (%s), want %v containing %q", ok, why, want, contains)
		}
	}
	check(true, "")
	if ok, why := ProfileAvailable(ctx, d, 99); ok || !strings.Contains(why, "not found") {
		t.Fatalf("unknown profile: %v %s", ok, why)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Expired cooldowns do not count.
	exec(`INSERT INTO account_state (account_id, cooldown_until, cooldown_reason) VALUES (1, ?, 'old')`, ts(-time.Minute))
	exec(`INSERT INTO profile_state (profile_id, cooldown_until, cooldown_reason) VALUES (1, ?, 'old')`, ts(-time.Minute))
	check(true, "")
	exec(`UPDATE account_state SET cooldown_until = ?, cooldown_reason = '429'`, ts(time.Hour))
	check(false, "account f cooling down")
	exec(`UPDATE account_state SET cooldown_until = NULL; UPDATE profile_state SET cooldown_until = ?`, ts(time.Hour))
	check(false, "profile p cooling down")
	exec(`UPDATE profile_state SET cooldown_until = NULL`)

	// The latest snapshot of a window decides; a reset window or an old one without a
	// reset time does not block.
	snap := func(window string, used float64, resets any, observed string) {
		t.Helper()
		exec(`INSERT INTO quota_snapshots (account_id, source, quota_window, used_percent, resets_at, observed_at)
			VALUES (1, 'test', ?, ?, ?, ?)`, window, used, resets, observed)
	}
	snap("5h", 95, ts(time.Hour), ts(-2*time.Minute))
	check(false, "quota test 5h at 95%")
	snap("5h", 40, ts(time.Hour), ts(-time.Minute))
	check(true, "")
	snap("7d", 92, ts(-time.Minute), ts(-time.Hour))
	check(true, "")
	snap("month", 99, nil, ts(-7*time.Hour))
	check(true, "")
	snap("day", 90, nil, ts(-time.Minute))
	check(false, "day at 90%")
	if ok, _ := (Availability{QuotaThreshold: 95, Now: func() time.Time { return now }}).Check(ctx, d, 1); !ok {
		t.Fatal("threshold not configurable")
	}

	exec(`DELETE FROM quota_snapshots; UPDATE accounts SET retired_at = ?`, ts(0))
	check(false, "account f is retired")
	exec(`UPDATE profiles SET retired_at = ?`, ts(0))
	check(false, "profile p is retired")
}
