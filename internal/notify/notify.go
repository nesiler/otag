// Package notify stores notifications for the human: rows in `notifications`, listed
// with `otag notify list` and acknowledged with `otag notify ack`. The daemon also shows
// new rows as macOS notifications (sent_at marks a row as handled by it).
package notify

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/nesiler/otag/internal/db"
)

// Severities.
const (
	Info  = "info"
	Warn  = "warn"
	Error = "error"
)

// Notification is a notifications row.
type Notification struct {
	ID        int64  `json:"id"`
	Project   string `json:"project,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Kind      string `json:"kind"`
	Severity  string `json:"severity"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	SentAt    string `json:"sent_at,omitempty"`
	ReadAt    string `json:"read_at,omitempty"`
	CreatedAt string `json:"created_at"`
}

// New describes a notification to add. ProjectID is required when TaskID or RunID is
// set.
type New struct {
	ProjectID int64
	TaskID    string
	RunID     string
	Kind      string // e.g. run.crashed, run.stalled, run.unanswered, run.fallback, task.blocked
	Severity  string // default info
	Title     string
	Body      string
}

// Add inserts a notification and returns its id.
func Add(ctx context.Context, q db.Querier, n New) (int64, error) {
	if n.Severity == "" {
		n.Severity = Info
	}
	var id int64
	err := q.QueryRowContext(ctx, `INSERT INTO notifications (project_id, task_id, run_id, kind, severity, title, body)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`, nullInt(n.ProjectID), nullStr(n.TaskID), nullStr(n.RunID),
		n.Kind, n.Severity, n.Title, n.Body).Scan(&id)
	return id, err
}

// Filter selects notifications for List.
type Filter struct {
	All   bool // include acknowledged ones
	Limit int  // 0 = 50
}

// List returns notifications, newest first.
func List(ctx context.Context, q db.Querier, f Filter) ([]Notification, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	query := `
SELECT n.id, coalesce(p.name, ''), coalesce(n.task_id, ''), coalesce(n.run_id, ''), n.kind, n.severity, n.title,
       n.body, coalesce(n.sent_at, ''), coalesce(n.read_at, ''), n.created_at
FROM notifications n LEFT JOIN projects p ON p.id = n.project_id`
	if !f.All {
		query += " WHERE n.read_at IS NULL"
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY n.id DESC LIMIT ?", f.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Project, &n.TaskID, &n.RunID, &n.Kind, &n.Severity, &n.Title, &n.Body,
			&n.SentAt, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Unread counts notifications not acknowledged yet.
func Unread(ctx context.Context, q db.Querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE read_at IS NULL`).Scan(&n)
	return n, err
}

// Ack marks notifications read; no ids = every unread one. It returns how many changed.
func Ack(ctx context.Context, q db.Querier, ids []int64) (int64, error) {
	query := `UPDATE notifications SET read_at = ? WHERE read_at IS NULL`
	args := []any{db.Now()}
	if len(ids) > 0 {
		query += " AND id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Pending returns up to limit notifications the daemon has not handled (sent_at NULL),
// oldest first.
func Pending(ctx context.Context, q db.Querier, limit int) ([]Notification, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, kind, severity, title, body, created_at FROM notifications
		WHERE sent_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Severity, &n.Title, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkSent records that the daemon handled a notification; channel says how
// (desktop | none).
func MarkSent(ctx context.Context, q db.Querier, id int64, channel string) error {
	res, err := q.ExecContext(ctx, `UPDATE notifications SET sent_at = ?, channel = ? WHERE id = ? AND sent_at IS NULL`,
		db.Now(), channel, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("notification %d already sent", id)
	}
	return nil
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func nullInt(i int64) sql.NullInt64   { return sql.NullInt64{Int64: i, Valid: i != 0} }
