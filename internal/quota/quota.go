// Package quota records how much of each account's provider quota is used.
//
// Observations come from passive, local sources first (Claude Code statusLine
// payloads, Codex rollout files), then from cheap official endpoints (LiteLLM
// /key/info) and, only when enabled, the undocumented Claude OAuth usage endpoint. No
// collector ever refreshes a token or writes into a harness's config directory.
//
// Everything ends up in quota_snapshots (one row per account, source and window, touched
// while the values stay the same) and, when a limit is hit, in the cooldown of
// account_state or profile_state. See docs/QUOTA.md.
package quota

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Collector is one periodic quota source; the daemon calls Collect on its schedule.
type Collector interface {
	Collect(ctx context.Context) error
}

// Sources stored in quota_snapshots.source.
const (
	SourceClaudeStatusline = "claude_statusline"
	SourceClaudeOAuth      = "claude_oauth"
	SourceCodexRollout     = "codex_rollout"
	SourceLiteLLM          = "litellm"
	SourceRateLimit        = "rate_limit" // a normalized rate_limit event of a run
)

// DefaultCooldown is used when a window is reported reached without a reset time
// (same as run.DefaultCooldown).
const DefaultCooldown = 30 * time.Minute

// StaleAfter is the age after which a snapshot is shown as stale. A stale snapshot is
// still the last thing known; its resets_at tells whether the window has rolled over.
const StaleAfter = time.Hour

// Window is one quota window as observed.
type Window struct {
	Name        string   `json:"window"` // 5h, 7d, 7d-opus, spend-monthly, budget-30d, gemini-weekly ...
	UsedPercent *float64 `json:"used_percent,omitempty"`
	Used        *float64 `json:"used,omitempty"`  // absolute amount when known (USD spend)
	Limit       *float64 `json:"limit,omitempty"` // absolute limit when known
	ResetsAt    time.Time
	// Reached is set when the provider itself says the limit is hit.
	Reached bool `json:"reached,omitempty"`
	// Raw is a small extract stored in quota_snapshots.raw (never secrets).
	Raw map[string]any `json:"-"`
}

// Exhausted reports whether the window is at or over its limit.
func (w Window) Exhausted() bool {
	return w.Reached || (w.UsedPercent != nil && *w.UsedPercent >= 100)
}

// Observation is a set of windows of one account (and optionally one profile) seen
// by one source at one time.
type Observation struct {
	AccountID  int64
	ProfileID  int64 // 0 = the whole account
	Source     string
	ObservedAt time.Time
	Windows    []Window
	// NoCooldown records the windows only; the caller handles the cooldown.
	NoCooldown bool
}

// WindowName names a window by its length: 300 min → 5h, 10080 min → 7d.
func WindowName(minutes int64) string {
	switch {
	case minutes <= 0:
		return "window"
	case minutes%1440 == 0:
		return fmt.Sprintf("%dd", minutes/1440)
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

func ptr(f float64) *float64 { return &f }

// round2 keeps two decimals, so float noise does not defeat the touch-if-equal logic.
func round2(f float64) float64 { return math.Round(f*100) / 100 }

func epoch(sec float64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), 0).UTC()
}
