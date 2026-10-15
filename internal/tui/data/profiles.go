package data

import (
	"context"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// unavailable returns the active profiles that new runs may not use now, with why
// (run.ProfileAvailable: retired, cooldown or quota). The pickers grey them out; the
// CLI stays the authority.
func unavailable(ctx context.Context, q db.Querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name FROM profiles WHERE retired_at IS NULL`)
	if err != nil {
		return nil, err
	}
	type prof struct {
		id   int64
		name string
	}
	var ps []prof
	for rows.Next() {
		var p prof
		if err := rows.Scan(&p.id, &p.name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ps = append(ps, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range ps {
		if ok, why := run.ProfileAvailable(ctx, q, p.id); !ok {
			out[p.name] = why
		}
	}
	return out, nil
}

// Task reads one task with its dependencies (task.Get), for a form prefilled with
// its current values and version.
func Task(ctx context.Context, d *db.DB, id string) (*task.Task, error) {
	return task.Get(ctx, d, id)
}
