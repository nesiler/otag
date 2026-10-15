package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/event"
)

// Counts summarizes what sync did to one table.
type Counts struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Retired   int `json:"retired"`
}

func (c Counts) changed() bool { return c.Added+c.Updated+c.Retired > 0 }

// SyncResult is returned by Sync.
type SyncResult struct {
	Accounts  Counts   `json:"accounts"`
	Profiles  Counts   `json:"profiles"`
	Projects  Counts   `json:"projects"`
	Fallbacks int      `json:"fallback_lists_changed"`
	Warnings  []string `json:"warnings"`
}

// Sync upserts cfg into the database inside tx. Rows missing from cfg are retired, never
// deleted, so historic foreign keys stay valid. Running it twice is a no-op.
func Sync(ctx context.Context, tx *sql.Tx, cfg *Config, actor string) (SyncResult, error) {
	res := SyncResult{Warnings: cfg.Warnings}
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	var err error

	accountIDs := map[string]int64{}
	for _, name := range sortedKeys(cfg.Accounts) {
		a := cfg.Accounts[name]
		conf, hash, err := hashJSON(a)
		if err != nil {
			return res, err
		}
		env, _ := json.Marshal(nonNilMap(a.Env))
		id, err := upsert(ctx, tx, &res.Accounts, "accounts", name, hash,
			[]string{"harness", "config_dir", "command", "env", "config"},
			[]any{a.Harness, a.ConfigDir, a.Command, string(env), conf})
		if err != nil {
			return res, fmt.Errorf("account %s: %w", name, err)
		}
		accountIDs[name] = id
	}

	profileIDs := map[string]int64{}
	for _, name := range sortedKeys(cfg.Profiles) {
		p := cfg.Profiles[name]
		// The revision covers the account too: a run must know exactly what it launched with.
		conf, hash, err := hashJSON(map[string]any{"name": name, "profile": p, "account_name": p.Account, "account": cfg.Accounts[p.Account]})
		if err != nil {
			return res, err
		}
		tags, _ := json.Marshal(nonNilSlice(p.Tags))
		env, _ := json.Marshal(nonNilMap(p.Env))
		args, _ := json.Marshal(nonNilSlice(p.Args))
		id, err := upsert(ctx, tx, &res.Profiles, "profiles", name, hash,
			[]string{"account_id", "model", "effort", "mode", "tags", "env", "args", "description", "config"},
			[]any{accountIDs[p.Account], p.Model, p.Effort, p.Mode, string(tags), string(env), string(args), p.Description, conf})
		if err != nil {
			return res, fmt.Errorf("profile %s: %w", name, err)
		}
		profileIDs[name] = id
		if _, err := tx.ExecContext(ctx, `
INSERT INTO profile_revisions (profile_id, account_id, harness, model, effort, config_hash, config)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (profile_id, config_hash) DO NOTHING`,
			id, accountIDs[p.Account], cfg.Accounts[p.Account].Harness, p.Model, p.Effort, hash, conf); err != nil {
			return res, err
		}
	}
	for _, name := range sortedKeys(cfg.Profiles) {
		changed, err := syncFallbacks(ctx, tx, profileIDs[name], cfg.Profiles[name].Fallback, profileIDs)
		if err != nil {
			return res, fmt.Errorf("profile %s fallbacks: %w", name, err)
		}
		if changed {
			res.Fallbacks++
		}
	}

	var projNames []string
	for _, p := range cfg.Projects {
		projNames = append(projNames, p.Name)
	}
	// Retire projects first so a prefix can move from a removed project to a new one.
	if res.Projects.Retired, err = retireMissing(ctx, tx, "projects", projNames); err != nil {
		return res, err
	}
	// Release prefixes that are about to change, so swaps between active projects
	// do not trip the active-prefix unique index mid-transaction.
	for _, p := range cfg.Projects {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET prefix = '#' || id
			WHERE name = ? AND retired_at IS NULL AND prefix <> ?`, p.Name, p.Prefix); err != nil {
			return res, err
		}
	}
	for _, p := range cfg.Projects {
		conf, hash, err := hashJSON(p.Raw)
		if err != nil {
			return res, err
		}
		deny, _ := json.Marshal(nonNilSlice(p.Deny))
		_, err = upsert(ctx, tx, &res.Projects, "projects", p.Name, hash,
			[]string{"path", "prefix", "coordinator_profile_id", "default_profile_id", "review_profile_id",
				"workspace_mode", "deny", "guard", "config"},
			[]any{p.Path, p.Prefix, refID(profileIDs, p.CoordinatorProfile), refID(profileIDs, p.DefaultProfile),
				refID(profileIDs, p.ReviewProfile), p.Workspace, string(deny), p.Guard, conf})

		if err != nil {
			return res, fmt.Errorf("project %s: %w", p.Name, err)
		}
	}
	if res.Profiles.Retired, err = retireMissing(ctx, tx, "profiles", sortedKeys(cfg.Profiles)); err != nil {
		return res, err
	}
	if res.Accounts.Retired, err = retireMissing(ctx, tx, "accounts", sortedKeys(cfg.Accounts)); err != nil {
		return res, err
	}

	if res.Accounts.changed() || res.Profiles.changed() || res.Projects.changed() || res.Fallbacks > 0 {
		_, err := event.Record(ctx, tx, event.Event{
			Source: "sync", Kind: "registry.synced", EntityType: "registry", EntityID: cfg.Dir,
			Actor: actor, Data: res,
		})
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// upsert inserts or updates the row named name in table (accounts/profiles/projects).
// Only rows whose config_hash changed, or that were retired, are written.
func upsert(ctx context.Context, tx *sql.Tx, c *Counts, table, name, hash string, cols []string, vals []any) (int64, error) {
	var (
		id      int64
		oldHash string
		retired sql.NullString
	)
	err := tx.QueryRowContext(ctx, "SELECT id, config_hash, retired_at FROM "+table+" WHERE name = ?", name).
		Scan(&id, &oldHash, &retired)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		q := "INSERT INTO " + table + " (name, config_hash"
		ph := "?, ?"
		for _, col := range cols {
			q += ", " + col
			ph += ", ?"
		}
		q += ") VALUES (" + ph + ") RETURNING id"
		if err := tx.QueryRowContext(ctx, q, append([]any{name, hash}, vals...)...).Scan(&id); err != nil {
			return 0, err
		}
		c.Added++
		return id, nil
	case err != nil:
		return 0, err
	case oldHash == hash && !retired.Valid:
		c.Unchanged++
		return id, nil
	}
	q := "UPDATE " + table + " SET config_hash = ?, retired_at = NULL"
	for _, col := range cols {
		q += ", " + col + " = ?"
	}
	q += " WHERE id = ?"
	if _, err := tx.ExecContext(ctx, q, append(append([]any{hash}, vals...), id)...); err != nil {
		return 0, err
	}
	c.Updated++
	return id, nil
}

func retireMissing(ctx context.Context, tx *sql.Tx, table string, keep []string) (int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, name FROM "+table+" WHERE retired_at IS NULL")
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !slices.Contains(keep, name) {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := db.Now()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET retired_at = ? WHERE id = ?", now, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// syncFallbacks replaces the fallback list of profileID if it differs from want.
func syncFallbacks(ctx context.Context, tx *sql.Tx, profileID int64, want []string, ids map[string]int64) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT fallback_profile_id FROM profile_fallbacks WHERE profile_id = ? ORDER BY position", profileID)
	if err != nil {
		return false, err
	}
	var have []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return false, err
		}
		have = append(have, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	wantIDs := make([]int64, len(want))
	for i, n := range want {
		wantIDs[i] = ids[n]
	}
	if slices.Equal(have, wantIDs) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM profile_fallbacks WHERE profile_id = ?", profileID); err != nil {
		return false, err
	}
	for i, id := range wantIDs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO profile_fallbacks (profile_id, position, fallback_profile_id) VALUES (?, ?, ?)",
			profileID, i+1, id); err != nil {
			return false, err
		}
	}
	return true, nil
}

func refID(ids map[string]int64, name string) sql.NullInt64 {
	id, ok := ids[name]
	return sql.NullInt64{Int64: id, Valid: ok && name != ""}
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
