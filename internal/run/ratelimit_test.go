package run

import (
	"context"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
)

// A rate_limit event cools the run's profile down until the reported reset (the fake
// harness is not account-wide), also when it arrives after the run ended.
func TestRateLimitEventSetsCooldown(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	reg := adapter.NewRegistry(fake.New())
	// Without a reset time (a 429 the harness retries) nothing cools down.
	if _, err := HandleHook(ctx, d, reg, HookRequest{Harness: "fake", Kind: adapter.RateLimit, RunID: "x-1-r1",
		Payload: []byte(`{"text":"retrying"}`)}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = d.QueryRow(`SELECT (SELECT count(*) FROM profile_state) + (SELECT count(*) FROM account_state)`).Scan(&n)
	if n != 0 {
		t.Fatal("cooldown without a reset time")
	}

	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	payload := `{"text":"limit reached","resets_at":"` + reset.Format(time.RFC3339) + `"}`
	if _, err := HandleHook(ctx, d, reg, HookRequest{Harness: "fake", Kind: adapter.RateLimit, RunID: "x-1-r1",
		Payload: []byte(payload)}); err != nil {
		t.Fatal(err)
	}
	var until, errText string
	_ = d.QueryRow(`SELECT cooldown_until FROM profile_state WHERE profile_id = 1`).Scan(&until)
	_ = d.QueryRow(`SELECT coalesce(error, '') FROM runs WHERE id = 'x-1-r1'`).Scan(&errText)
	if until != reset.Format("2006-01-02T15:04:05.000Z") || errText != "rate_limit: limit reached" {
		t.Fatalf("cooldown %q error %q", until, errText)
	}
	var snaps int
	_ = d.QueryRow(`SELECT count(*) FROM quota_snapshots WHERE source = 'rate_limit' AND profile_id = 1`).Scan(&snaps)
	if snaps != 1 {
		t.Fatalf("snapshots %d", snaps)
	}

	_, _ = d.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'failed', ended_at = '2026-10-10T00:00:00.000Z'`)
	later := reset.Add(time.Hour)
	payload = `{"text":"late","resets_at":"` + later.Format(time.RFC3339) + `"}`
	if _, err := HandleHook(ctx, d, reg, HookRequest{Harness: "fake", Kind: adapter.RateLimit, RunID: "x-1-r1",
		Payload: []byte(payload)}); err != nil {
		t.Fatal(err)
	}
	_ = d.QueryRow(`SELECT cooldown_until FROM profile_state WHERE profile_id = 1`).Scan(&until)
	if until != later.Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("late hit ignored: %q", until)
	}
}
