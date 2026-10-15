package quota

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/nesiler/otag/internal/db"
)

// Record writes the observation: per window, the latest row with the same account,
// profile, source and window is touched (observed_at moves forward) when its values are
// unchanged, otherwise a new row is inserted. An observation older than the latest row
// of its window is dropped. An exhausted window with a future reset also extends the
// cooldown (of the profile when ProfileID is set, else of the account). It returns the
// number of rows inserted or touched.
func Record(ctx context.Context, q db.Querier, o Observation) (int, error) {
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now()
	}
	at := fmtTime(o.ObservedAt)
	var profile any
	if o.ProfileID != 0 {
		profile = o.ProfileID
	}
	n := 0
	for _, w := range o.Windows {
		resets := nullTime(w.ResetsAt)
		used, usedAbs, limit := nullFloat(w.UsedPercent), nullFloat(w.Used), nullFloat(w.Limit)
		raw := "{}"
		if len(w.Raw) > 0 || w.Reached {
			m := map[string]any{}
			for k, v := range w.Raw {
				m[k] = v
			}
			if w.Reached {
				m["reached"] = true
			}
			b, err := json.Marshal(m)
			if err != nil {
				return n, err
			}
			raw = string(b)
		}
		var id int64
		var lastAt string
		var same bool
		err := q.QueryRowContext(ctx, `
SELECT id, observed_at,
       used_percent IS ? AND used IS ? AND limit_value IS ? AND resets_at IS ? AND raw = ?
FROM quota_snapshots
WHERE account_id = ? AND profile_id IS ? AND source = ? AND quota_window = ?
ORDER BY observed_at DESC, id DESC LIMIT 1`, used, usedAbs, limit, resets, raw,
			o.AccountID, profile, o.Source, w.Name).Scan(&id, &lastAt, &same)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return n, err
		case lastAt > at:
			continue // older than what we know
		case same:
			if lastAt < at {
				if _, err := q.ExecContext(ctx, `UPDATE quota_snapshots SET observed_at = ? WHERE id = ?`, at, id); err != nil {
					return n, err
				}
				n++
			}
			if err := cooldownFor(ctx, q, o, w); err != nil {
				return n, err
			}
			continue
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO quota_snapshots (account_id, profile_id, source, quota_window, used_percent, used, limit_value,
                             resets_at, raw, observed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.AccountID, profile, o.Source, w.Name, used, usedAbs, limit, resets, raw, at); err != nil {
			return n, err
		}
		n++
		if err := cooldownFor(ctx, q, o, w); err != nil {
			return n, err
		}
	}
	return n, nil
}

// cooldownFor puts the account (or profile) in cooldown until an exhausted window
// resets. A cooldown that ends in the past is harmless: readers compare with now.
func cooldownFor(ctx context.Context, q db.Querier, o Observation, w Window) error {
	if o.NoCooldown || !w.Exhausted() {
		return nil
	}
	until := w.ResetsAt
	if until.IsZero() {
		if !w.Reached {
			return nil
		}
		until = o.ObservedAt.Add(DefaultCooldown)
	}
	if !until.After(o.ObservedAt) {
		return nil // a past cooldown would change nothing
	}
	reason := fmt.Sprintf("quota %s exhausted (%s)", w.Name, o.Source)
	if o.ProfileID != 0 {
		return SetCooldown(ctx, q, 0, o.ProfileID, until, reason)
	}
	return SetCooldown(ctx, q, o.AccountID, 0, until, reason)
}

// SetCooldown makes the account (when accountID != 0) and the profile (when
// profileID != 0) unavailable until `until`. An existing later cooldown is kept:
// cooldowns only grow. It also records reason as the last error. run.SetCooldown is
// this function; collectors and the hook bridge share it.
func SetCooldown(ctx context.Context, q db.Querier, accountID, profileID int64, until time.Time, reason string) error {
	u, now := fmtTime(until), db.Now()
	upsert := func(table, key string, id int64) error {
		_, err := q.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %[1]s (%[2]s, cooldown_until, cooldown_reason, last_error, last_error_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (%[2]s) DO UPDATE SET
  cooldown_reason = CASE WHEN excluded.cooldown_until > coalesce(cooldown_until, '') THEN excluded.cooldown_reason
                         ELSE cooldown_reason END,
  cooldown_until = max(coalesce(cooldown_until, ''), excluded.cooldown_until),
  last_error = excluded.last_error, last_error_at = excluded.last_error_at`, table, key),
			id, u, reason, reason, now)
		return err
	}
	if accountID != 0 {
		if err := upsert("account_state", "account_id", accountID); err != nil {
			return err
		}
	}
	if profileID == 0 {
		return nil
	}
	return upsert("profile_state", "profile_id", profileID)
}

// Hit is a limit reported by a running harness (a normalized rate_limit event).
type Hit struct {
	RunID    string
	ResetsAt time.Time // zero = unknown
	Text     string
	At       time.Time // zero = now
}

// accountWide harnesses have limits that cover every model of the account (Claude and
// Codex subscription windows). Elsewhere (LiteLLM routes) a limit
// belongs to the model, so the profile cools down instead.
var accountWide = map[string]bool{"claude": true, "codex": true}

// Target is what a limit hit cools down: the account (ProfileID 0) or the profile
// (AccountID 0).
type Target struct{ AccountID, ProfileID int64 }

// HitTarget is what a limit hit of the run cools down: its account for Claude and
// Codex, else its profile. RecordHit and the run state machine's quota end both use it,
// so a limit of one LiteLLM route never blocks the other routes of the same account.
func HitTarget(ctx context.Context, q db.Querier, runID string) (Target, error) {
	t, _, err := hitTarget(ctx, q, runID)
	return t, err
}

func hitTarget(ctx context.Context, q db.Querier, runID string) (Target, int64, error) {
	var accountID, profileID int64
	var harness string
	err := q.QueryRowContext(ctx, `SELECT account_id, profile_id, harness FROM runs WHERE id = ?`, runID).
		Scan(&accountID, &profileID, &harness)
	if err != nil {
		return Target{}, 0, fmt.Errorf("quota hit of run %s: %w", runID, err)
	}
	if accountWide[harness] {
		return Target{AccountID: accountID}, accountID, nil
	}
	return Target{ProfileID: profileID}, accountID, nil
}

// CooldownUntil returns the current cooldown_until of the target ("" = none).
func CooldownUntil(ctx context.Context, q db.Querier, t Target) (string, error) {
	var until sql.NullString
	var err error
	if t.AccountID != 0 {
		err = q.QueryRowContext(ctx, `SELECT cooldown_until FROM account_state WHERE account_id = ?`, t.AccountID).Scan(&until)
	} else {
		err = q.QueryRowContext(ctx, `SELECT cooldown_until FROM profile_state WHERE profile_id = ?`, t.ProfileID).Scan(&until)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return until.String, err
}

// RecordHit records a run's rate_limit event that names a future reset time as a
// snapshot (source rate_limit, window "limit", 100 %, reached, resets_at = the reset)
// of the run's account (Claude, Codex) or profile (HitTarget), and returns that target;
// the caller sets the cooldown (run.SetCooldown), so one event writes one cooldown. A
// hit without a future reset records nothing and returns the zero Target: harnesses
// retry transient 429s themselves, and a turn that really fails on a limit is the run
// state machine's.
func RecordHit(ctx context.Context, q db.Querier, h Hit) (Target, error) {
	at := h.At
	if at.IsZero() {
		at = time.Now()
	}
	if !h.ResetsAt.After(at) {
		return Target{}, nil
	}
	t, accountID, err := hitTarget(ctx, q, h.RunID)
	if err != nil {
		return Target{}, err
	}
	o := Observation{AccountID: accountID, Source: SourceRateLimit, ObservedAt: at, NoCooldown: true, ProfileID: t.ProfileID}
	raw := map[string]any{"run": h.RunID}
	if h.Text != "" {
		raw["text"] = truncate(h.Text, 300)
	}
	o.Windows = []Window{{Name: "limit", UsedPercent: ptr(100), ResetsAt: h.ResetsAt, Reached: true, Raw: raw}}
	_, err = Record(ctx, q, o)
	return t, err
}

// RecordStatusline records a statusLine observation of an account. Claude windows
// (5h, 7d, spend) cover the whole account.
func RecordStatusline(ctx context.Context, q db.Querier, accountID int64, source string, at time.Time, ws []Window) (int, error) {
	return Record(ctx, q, Observation{AccountID: accountID, Source: source, ObservedAt: at, Windows: ws})
}

// Prune deletes snapshots observed before `before`, keeping the latest row of every
// (account, profile, source, window).
func Prune(ctx context.Context, q db.Querier, before time.Time) (int64, error) {
	res, err := q.ExecContext(ctx, `
DELETE FROM quota_snapshots
WHERE observed_at < ?
  AND id <> (SELECT s2.id FROM quota_snapshots s2
             WHERE s2.account_id = quota_snapshots.account_id AND s2.profile_id IS quota_snapshots.profile_id
               AND s2.source = quota_snapshots.source AND s2.quota_window = quota_snapshots.quota_window
             ORDER BY s2.observed_at DESC, s2.id DESC LIMIT 1)`, fmtTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LatestObserved returns when the account was last observed by source (zero if never).
func LatestObserved(ctx context.Context, q db.Querier, accountID int64, source string) (time.Time, error) {
	var s sql.NullString
	err := q.QueryRowContext(ctx, `SELECT max(observed_at) FROM quota_snapshots WHERE account_id = ? AND source = ?`,
		accountID, source).Scan(&s)
	if err != nil || !s.Valid {
		return time.Time{}, err
	}
	return time.Parse(db.TimeFormat, s.String)
}

// AccountID returns the id of an active account.
func AccountID(ctx context.Context, q db.Querier, name string) (int64, string, error) {
	var id int64
	var harness string
	err := q.QueryRowContext(ctx, `SELECT id, harness FROM accounts WHERE name = ? AND retired_at IS NULL`, name).
		Scan(&id, &harness)
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("account %q not found (run `otag sync`?)", name)
	}
	return id, harness, err
}

func fmtTime(t time.Time) string { return t.UTC().Format(db.TimeFormat) }

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return fmtTime(t)
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return round2(*f)
}

// truncate shortens s to at most n bytes plus "…", cutting at a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
