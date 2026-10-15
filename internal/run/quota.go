package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/quota"
)

// DefaultCooldown is how long an account (Claude, Codex) or a profile (other
// harnesses) rests after a turn failed on a rate limit whose reset time is unknown.
const DefaultCooldown = 30 * time.Minute

// DefaultQuotaThreshold is the used_percent at or above which a quota window makes a
// profile unavailable.
const DefaultQuotaThreshold = 90.0

// turnRateLimited reports whether the current turn of run saw a rate_limit event that
// the harness does not retry (data.retry unset): from the last turn_start/turn_end
// before event evID (the turn_end being applied) up to it. It returns the limit's text.
func turnRateLimited(ctx context.Context, tx *sql.Tx, runID string, evID int64) (string, bool, error) {
	var text string
	err := tx.QueryRowContext(ctx, `
SELECT coalesce(json_extract(data, '$.text'), '') FROM events
WHERE run_id = ? AND kind = 'hook.rate_limit' AND id < ?
  AND coalesce(json_extract(data, '$.retry'), 0) = 0
  AND id > coalesce((SELECT max(id) FROM events WHERE run_id = ? AND id < ?
                     AND kind IN ('hook.turn_start', 'hook.turn_end')), 0)
ORDER BY id DESC LIMIT 1`, runID, evID, runID, evID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return text, err == nil, err
}

// quotaEnded ends a live run whose turn failed on a usage limit: sessions closed,
// workspace kept, run ended (quota), task back to ready, and the limit's target cooling
// down for DefaultCooldown unless a known reset already cooled it. The target follows
// quota.HitTarget: the account for Claude and Codex (subscription windows), the profile
// elsewhere (a LiteLLM route is per model). The hook bridge
// closes the run's windows after the commit; the daemon may then start a fallback run.
func quotaEnded(ctx context.Context, tx *sql.Tx, r *row, a Actor, text string) error {
	if err := r.endSessions(ctx, tx, ReasonQuota); err != nil {
		return err
	}
	if err := closeWorkspace(ctx, tx, r.ID, ""); err != nil {
		return err
	}
	msg := oneLine("rate_limit: " + text)
	if err := r.end(ctx, tx, a, ReasonQuota, msg); err != nil {
		return err
	}
	t, err := quota.HitTarget(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	until, err := quota.CooldownUntil(ctx, tx, t)
	if err != nil {
		return err
	}
	if until <= db.Now() {
		if err := SetCooldown(ctx, tx, t.AccountID, t.ProfileID, time.Now().Add(DefaultCooldown), msg); err != nil {
			return err
		}
	}
	return r.releaseTask(ctx, tx, a, "run "+r.ID+" hit a usage limit")
}

// SetCooldown makes the account (when accountID != 0) and the profile (when
// profileID != 0) unavailable until `until`. An existing later cooldown is kept:
// cooldowns only grow. It also records reason as the last error. Quota collectors
// use the same function (quota.SetCooldown).
func SetCooldown(ctx context.Context, q db.Querier, accountID, profileID int64, until time.Time, reason string) error {
	return quota.SetCooldown(ctx, q, accountID, profileID, until, reason)
}

// Availability decides whether a profile may take a new run. The zero value uses the
// defaults.
type Availability struct {
	QuotaThreshold float64          // used_percent that blocks; default DefaultQuotaThreshold
	SnapshotMaxAge time.Duration    // a snapshot without resets_at older than this is ignored; default 6h
	Now            func() time.Time // default time.Now
}

// ProfileAvailable reports whether new runs may use the profile now, with the default
// policy. reason explains a false.
func ProfileAvailable(ctx context.Context, q db.Querier, profileID int64) (ok bool, reason string) {
	return Availability{}.Check(ctx, q, profileID)
}

// Check reports whether new runs may use the profile now. It is unavailable when:
//   - the profile or its account is retired (or unknown);
//   - profile_state.cooldown_until or account_state.cooldown_until is in the future;
//   - the latest quota_snapshots row of some (source, quota_window) of its account,
//     account-wide (profile_id NULL) or for this profile, has used_percent >= the
//     threshold and has not reset yet (resets_at in the future, or no resets_at and
//     observed within SnapshotMaxAge).
//
// A database error counts as unavailable (the reason says why).
func (av Availability) Check(ctx context.Context, q db.Querier, profileID int64) (bool, string) {
	threshold := av.QuotaThreshold
	if threshold <= 0 {
		threshold = DefaultQuotaThreshold
	}
	maxAge := av.SnapshotMaxAge
	if maxAge <= 0 {
		maxAge = 6 * time.Hour
	}
	nowT := time.Now()
	if av.Now != nil {
		nowT = av.Now()
	}
	now := nowT.UTC().Format(db.TimeFormat)

	var name, account string
	var accountID int64
	var retired, accRetired bool
	var pUntil, pReason, aUntil, aReason sql.NullString
	err := q.QueryRowContext(ctx, `
SELECT p.name, p.retired_at IS NOT NULL, a.id, a.name, a.retired_at IS NOT NULL,
       ps.cooldown_until, ps.cooldown_reason, s.cooldown_until, s.cooldown_reason
FROM profiles p JOIN accounts a ON a.id = p.account_id
LEFT JOIN profile_state ps ON ps.profile_id = p.id
LEFT JOIN account_state s ON s.account_id = a.id
WHERE p.id = ?`, profileID).Scan(&name, &retired, &accountID, &account, &accRetired, &pUntil, &pReason, &aUntil, &aReason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, fmt.Sprintf("profile %d not found", profileID)
	case err != nil:
		return false, "availability check failed: " + err.Error()
	case retired:
		return false, "profile " + name + " is retired"
	case accRetired:
		return false, "account " + account + " is retired"
	case pUntil.Valid && pUntil.String > now:
		return false, fmt.Sprintf("profile %s cooling down until %s (%s)", name, pUntil.String, pReason.String)
	case aUntil.Valid && aUntil.String > now:
		return false, fmt.Sprintf("account %s cooling down until %s (%s)", account, aUntil.String, aReason.String)
	}

	var source, window, resets string
	var used float64
	err = q.QueryRowContext(ctx, `
SELECT s.source, s.quota_window, s.used_percent, coalesce(s.resets_at, '')
FROM quota_snapshots s
WHERE s.account_id = ? AND (s.profile_id IS NULL OR s.profile_id = ?)
  AND s.id = (SELECT s2.id FROM quota_snapshots s2
              WHERE s2.account_id = s.account_id AND s2.source = s.source AND s2.quota_window = s.quota_window
                AND coalesce(s2.profile_id, 0) = coalesce(s.profile_id, 0)
              ORDER BY s2.observed_at DESC, s2.id DESC LIMIT 1)
  AND s.used_percent >= ?
  AND (s.resets_at > ? OR (s.resets_at IS NULL AND s.observed_at > ?))
ORDER BY s.used_percent DESC LIMIT 1`, accountID, profileID, threshold, now,
		nowT.Add(-maxAge).UTC().Format(db.TimeFormat)).Scan(&source, &window, &used, &resets)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true, ""
	case err != nil:
		return false, "availability check failed: " + err.Error()
	}
	why := fmt.Sprintf("account %s quota %s %s at %.0f%% (threshold %.0f%%)", account, source, window, used, threshold)
	if resets != "" {
		why += ", resets " + resets
	}
	return false, why
}
