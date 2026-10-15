package quota

import (
	"context"
	"database/sql"
	"time"

	"github.com/nesiler/otag/internal/db"
)

// Row is the latest snapshot of one (account, profile, source, window).
type Row struct {
	Account     string     `json:"account"`
	Harness     string     `json:"harness"`
	Profile     string     `json:"profile,omitempty"`
	Window      string     `json:"window"`
	Source      string     `json:"source"`
	UsedPercent *float64   `json:"used_percent"`
	Used        *float64   `json:"used,omitempty"`
	Limit       *float64   `json:"limit,omitempty"`
	ResetsAt    *time.Time `json:"resets_at"`
	ObservedAt  time.Time  `json:"observed_at"`
	// Derived at read time.
	AgeSeconds      int64  `json:"age_s"`
	ResetsInSeconds *int64 `json:"resets_in_s"`
	Stale           bool   `json:"stale"`        // older than StaleAfter
	ResetPassed     bool   `json:"reset_passed"` // the window has rolled over since
}

// Effective is the used percent that still applies now: 0 once the window has reset,
// nil when unknown.
func (r Row) Effective() *float64 {
	if r.ResetPassed {
		return ptr(0)
	}
	return r.UsedPercent
}

// Cooldown is an active account or profile cooldown.
type Cooldown struct {
	Account string    `json:"account,omitempty"`
	Profile string    `json:"profile,omitempty"`
	Until   time.Time `json:"until"`
	Reason  string    `json:"reason"`
}

// Filter narrows List.
type Filter struct {
	Account string
	// MaxAge hides snapshots older than this (0 = all).
	MaxAge time.Duration
}

// List returns the latest snapshot of every (account, profile, source, window) of the
// active accounts, ordered by account, profile, window and source.
func List(ctx context.Context, q db.Querier, f Filter, now time.Time) ([]Row, error) {
	since := ""
	if f.MaxAge > 0 {
		since = fmtTime(now.Add(-f.MaxAge))
	}
	rows, err := q.QueryContext(ctx, `
SELECT a.name, a.harness, coalesce(p.name, ''), s.quota_window, s.source, s.used_percent, s.used, s.limit_value,
       s.resets_at, s.observed_at
FROM quota_snapshots s
JOIN accounts a ON a.id = s.account_id
LEFT JOIN profiles p ON p.id = s.profile_id
WHERE a.retired_at IS NULL AND (? = '' OR a.name = ?) AND s.observed_at >= ?
  AND s.id = (SELECT s2.id FROM quota_snapshots s2
              WHERE s2.account_id = s.account_id AND s2.profile_id IS s.profile_id
                AND s2.source = s.source AND s2.quota_window = s.quota_window
              ORDER BY s2.observed_at DESC, s2.id DESC LIMIT 1)
ORDER BY a.name, coalesce(p.name, ''), s.quota_window, s.source`, f.Account, f.Account, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Row{}
	for rows.Next() {
		var r Row
		var used, usedAbs, limit sql.NullFloat64
		var resets sql.NullString
		var observed string
		if err := rows.Scan(&r.Account, &r.Harness, &r.Profile, &r.Window, &r.Source, &used, &usedAbs, &limit,
			&resets, &observed); err != nil {
			return nil, err
		}
		r.UsedPercent, r.Used, r.Limit = nf(used), nf(usedAbs), nf(limit)
		r.ObservedAt, _ = time.Parse(db.TimeFormat, observed)
		r.AgeSeconds = int64(now.Sub(r.ObservedAt).Seconds())
		r.Stale = now.Sub(r.ObservedAt) > StaleAfter
		if resets.Valid {
			if t, err := time.Parse(db.TimeFormat, resets.String); err == nil {
				r.ResetsAt = &t
				in := int64(t.Sub(now).Seconds())
				r.ResetsInSeconds = &in
				r.ResetPassed = !t.After(now)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Cooldowns returns account and profile cooldowns that last beyond now.
func Cooldowns(ctx context.Context, q db.Querier, now time.Time) ([]Cooldown, error) {
	rows, err := q.QueryContext(ctx, `
SELECT a.name, '', s.cooldown_until, coalesce(s.cooldown_reason, '')
FROM account_state s JOIN accounts a ON a.id = s.account_id
WHERE s.cooldown_until > ? AND a.retired_at IS NULL
UNION ALL
SELECT a.name, p.name, s.cooldown_until, coalesce(s.cooldown_reason, '')
FROM profile_state s JOIN profiles p ON p.id = s.profile_id JOIN accounts a ON a.id = p.account_id
WHERE s.cooldown_until > ? AND p.retired_at IS NULL
ORDER BY 1, 2`, fmtTime(now), fmtTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Cooldown{}
	for rows.Next() {
		var c Cooldown
		var until string
		if err := rows.Scan(&c.Account, &c.Profile, &until, &c.Reason); err != nil {
			return nil, err
		}
		c.Until, _ = time.Parse(db.TimeFormat, until)
		out = append(out, c)
	}
	return out, rows.Err()
}

func nf(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}
