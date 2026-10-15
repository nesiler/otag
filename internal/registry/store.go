package registry

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nesiler/otag/internal/db"
)

// ProfileRow is an active profile as stored in the database.
type ProfileRow struct {
	Name        string   `json:"name"`
	Account     string   `json:"account"`
	Harness     string   `json:"harness"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort"`
	Mode        string   `json:"mode"`
	Tags        []string `json:"tags"`
	Fallback    []string `json:"fallback"`
	Description string   `json:"description"`
	ConfigHash  string   `json:"config_hash"`
}

// ListProfiles returns active profiles, optionally only those carrying tag.
func ListProfiles(ctx context.Context, q db.Querier, tag string) ([]ProfileRow, error) {
	rows, err := q.QueryContext(ctx, `
SELECT p.name, a.name, a.harness, p.model, p.effort, p.mode, p.tags, p.description, p.config_hash,
       coalesce((SELECT group_concat(fp.name, ',' ORDER BY f.position)
                 FROM profile_fallbacks f JOIN profiles fp ON fp.id = f.fallback_profile_id
                 WHERE f.profile_id = p.id), '')
FROM profiles p JOIN accounts a ON a.id = p.account_id
WHERE p.retired_at IS NULL
  AND (? = '' OR EXISTS (SELECT 1 FROM json_each(p.tags) WHERE json_each.value = ?))
ORDER BY p.name`, tag, tag)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ProfileRow{}
	for rows.Next() {
		var r ProfileRow
		var tags, fb string
		if err := rows.Scan(&r.Name, &r.Account, &r.Harness, &r.Model, &r.Effort, &r.Mode, &tags,
			&r.Description, &r.ConfigHash, &fb); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tags), &r.Tags); err != nil {
			return nil, err
		}
		r.Fallback = []string{}
		if fb != "" {
			r.Fallback = strings.Split(fb, ",")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProjectRow is an active project as stored in the database.
type ProjectRow struct {
	ID                 int64    `json:"-"`
	Name               string   `json:"name"`
	Path               string   `json:"path"`
	Prefix             string   `json:"prefix"`
	CoordinatorProfile string   `json:"coordinator_profile"`
	DefaultProfile     string   `json:"default_profile"`
	ReviewProfile      string   `json:"review_profile"`
	WorkspaceMode      string   `json:"workspace_mode"`
	Deny               []string `json:"deny"`
	Guard              string   `json:"guard"` // light | strict
}

// ListProjects returns active projects ordered by name.
func ListProjects(ctx context.Context, q db.Querier) ([]ProjectRow, error) {
	rows, err := q.QueryContext(ctx, `
SELECT pr.id, pr.name, pr.path, pr.prefix,
       coalesce(c.name, ''), coalesce(d.name, ''), coalesce(r.name, ''),
       pr.workspace_mode, pr.deny, pr.guard
FROM projects pr
LEFT JOIN profiles c ON c.id = pr.coordinator_profile_id
LEFT JOIN profiles d ON d.id = pr.default_profile_id
LEFT JOIN profiles r ON r.id = pr.review_profile_id
WHERE pr.retired_at IS NULL
ORDER BY pr.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ProjectRow
	for rows.Next() {
		var p ProjectRow
		var deny string
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.Prefix, &p.CoordinatorProfile, &p.DefaultProfile,
			&p.ReviewProfile, &p.WorkspaceMode, &deny, &p.Guard); err != nil {

			return nil, err
		}
		if err := json.Unmarshal([]byte(deny), &p.Deny); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
