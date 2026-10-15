// Package event appends rows to the events table.
package event

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/nesiler/otag/internal/db"
)

// Event is one row of the audit/event log.
type Event struct {
	Source     string // cli | sync | daemon | hook:<harness>
	DedupeKey  string // optional; (Source, DedupeKey) is unique
	Kind       string
	EntityType string
	EntityID   string
	FromStatus string
	ToStatus   string
	ProjectID  int64
	TaskID     string
	RunID      string
	Actor      string
	Data       any // marshalled to JSON; nil = {}
}

// Record inserts e. It returns false without error when an event with the same
// (Source, DedupeKey) already exists.
func Record(ctx context.Context, q db.Querier, e Event) (bool, error) {
	_, ok, err := Insert(ctx, q, e)
	return ok, err
}

// Insert is Record that also returns the new row id (0 when deduplicated).
func Insert(ctx context.Context, q db.Querier, e Event) (int64, bool, error) {
	data := []byte("{}")
	if e.Data != nil {
		var err error
		if data, err = json.Marshal(e.Data); err != nil {
			return 0, false, err
		}
	}
	res, err := q.ExecContext(ctx, `
INSERT INTO events (source, dedupe_key, kind, entity_type, entity_id, from_status, to_status,
                    project_id, task_id, run_id, actor, data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (source, dedupe_key) DO NOTHING`,
		e.Source, nullStr(e.DedupeKey), e.Kind, e.EntityType, e.EntityID,
		nullStr(e.FromStatus), nullStr(e.ToStatus), nullInt(e.ProjectID),
		nullStr(e.TaskID), nullStr(e.RunID), e.Actor, string(data))
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	return id, err == nil, err
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func nullInt(i int64) sql.NullInt64   { return sql.NullInt64{Int64: i, Valid: i != 0} }
