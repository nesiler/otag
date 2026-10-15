// Package task implements tasks, their status machine and dependencies.
//
// Mutating functions take a *sql.Tx opened with BEGIN IMMEDIATE (db.DB.Tx); every
// status change is a compare-and-swap on tasks.version plus an events row in that tx.
package task

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/event"
)

// Task is a unit of work.
type Task struct {
	ID              string          `json:"id"`
	Project         string          `json:"project"`
	ProjectID       int64           `json:"-"`
	Title           string          `json:"title"`
	Body            string          `json:"body"`
	Status          string          `json:"status"`
	StatusReason    string          `json:"status_reason"`
	Priority        int             `json:"priority"`
	Profile         string          `json:"profile"`
	CurrentRunID    string          `json:"current_run_id"`
	Version         int64           `json:"version"`
	ExternalRef     string          `json:"external_ref"`
	Metadata        json.RawMessage `json:"metadata"`
	ExtraPrompt     string          `json:"extra_prompt"`
	ClaimedBy       string          `json:"claimed_by"`
	ClaimedAt       string          `json:"claimed_at"`
	StatusChangedAt string          `json:"status_changed_at"`
	ClosedAt        string          `json:"closed_at"`
	DeliveredAt     string          `json:"delivered_at"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	// Deps are edges from this task; Dependents are edges pointing at it. Filled by Get.
	Deps       []Dep `json:"deps,omitempty"`
	Dependents []Dep `json:"dependents,omitempty"`
}

// Dep is one dependency edge as seen from a task.
type Dep struct {
	Kind   string `json:"kind"`
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// DepKinds are the allowed dependency kinds.
var DepKinds = []string{"blocks", "review_of", "parent", "related"}

// Actor identifies who performs a mutation; recorded on events.
type Actor struct {
	Name   string // e.g. "user:enes" or "run:<id>"
	RunID  string
	Source string // events.source; default "cli"
}

func (a Actor) source() string {
	if a.Source == "" {
		return "cli"
	}
	return a.Source
}

const selectTask = `
SELECT t.id, p.name, t.project_id, t.title, t.body, t.status, t.status_reason, t.priority,
       coalesce(pf.name, ''), coalesce(t.current_run_id, ''), t.version, coalesce(t.external_ref, ''),
       t.metadata, t.extra_prompt, coalesce(t.claimed_by, ''), coalesce(t.claimed_at, ''),
       t.status_changed_at, coalesce(t.closed_at, ''), coalesce(t.delivered_at, ''), t.created_by, t.created_at,
       t.updated_at
FROM %s t
JOIN projects p ON p.id = t.project_id
LEFT JOIN profiles pf ON pf.id = t.profile_id`

type scanner interface{ Scan(...any) error }

func scanTask(s scanner) (*Task, error) {
	var t Task
	var meta string
	err := s.Scan(&t.ID, &t.Project, &t.ProjectID, &t.Title, &t.Body, &t.Status, &t.StatusReason, &t.Priority,
		&t.Profile, &t.CurrentRunID, &t.Version, &t.ExternalRef, &meta, &t.ExtraPrompt, &t.ClaimedBy,
		&t.ClaimedAt, &t.StatusChangedAt, &t.ClosedAt, &t.DeliveredAt, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	t.Metadata = json.RawMessage(meta)
	return &t, err
}

// Get returns a task with its dependency edges.
func Get(ctx context.Context, q db.Querier, id string) (*Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx, fmt.Sprintf(selectTask, "tasks")+" WHERE t.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.NotFound, "task %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if t.Deps, err = deps(ctx, q, `
SELECT d.kind, d.depends_on_id, o.title, o.status FROM task_deps d JOIN tasks o ON o.id = d.depends_on_id
WHERE d.task_id = ? ORDER BY d.kind, d.depends_on_id`, id); err != nil {
		return nil, err
	}
	if t.Dependents, err = deps(ctx, q, `
SELECT d.kind, d.task_id, o.title, o.status FROM task_deps d JOIN tasks o ON o.id = d.task_id
WHERE d.depends_on_id = ? ORDER BY d.kind, d.task_id`, id); err != nil {
		return nil, err
	}
	return t, nil
}

func deps(ctx context.Context, q db.Querier, query, id string) ([]Dep, error) {
	rows, err := q.QueryContext(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Dep
	for rows.Next() {
		var d Dep
		if err := rows.Scan(&d.Kind, &d.TaskID, &d.Title, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Filter selects tasks for List.
type Filter struct {
	ProjectID int64 // 0 = all projects
	Statuses  []string
	ReadyOnly bool // use the ready_tasks view (ready and unblocked)
	Open      bool // exclude done, delivered and cancelled
	// Deliverable: done tasks waiting for delivery, without review tasks (they are
	// delivered with the task they review).
	Deliverable bool
	// Recent orders by the last status change, newest first (instead of priority), so a
	// Limit keeps the latest.
	Recent bool
	Limit  int
}

// List returns tasks ordered by priority then age (Filter.Recent: newest status change first).
func List(ctx context.Context, q db.Querier, f Filter) ([]Task, error) {
	from := "tasks"
	if f.ReadyOnly {
		from = "ready_tasks"
	}
	query := fmt.Sprintf(selectTask, from) + " WHERE 1 = 1"
	var args []any
	if f.ProjectID != 0 {
		query += " AND t.project_id = ?"
		args = append(args, f.ProjectID)
	}
	if len(f.Statuses) > 0 {
		query += " AND t.status IN (?" + strings.Repeat(", ?", len(f.Statuses)-1) + ")"
		for _, s := range f.Statuses {
			args = append(args, s)
		}
	}
	if f.Open {
		query += " AND t.status NOT IN ('done', 'delivered', 'cancelled')"
	}
	if f.Deliverable {
		query += ` AND t.status = 'done'
  AND NOT EXISTS (SELECT 1 FROM task_deps d WHERE d.task_id = t.id AND d.kind = 'review_of')`
	}
	if f.Recent {
		query += " ORDER BY t.status_changed_at DESC, t.id"
	} else {
		query += " ORDER BY t.priority DESC, t.created_at, t.id"
	}
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// DepRef names a dependency edge from a new or edited task.
type DepRef struct {
	Kind string
	On   string // task id
}

// AddInput describes a new task.
type AddInput struct {
	ProjectID   int64
	Prefix      string
	Title       string
	Body        string
	Priority    int
	Profile     string
	Draft       bool
	ExternalRef string
	Metadata    json.RawMessage
	ExtraPrompt string
	Deps        []DepRef
}

// Add creates a task in status ready (or draft) and records task.created.
func Add(ctx context.Context, tx *sql.Tx, in AddInput, actor Actor) (*Task, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, apperr.New(apperr.Invalid, "title is required")
	}
	if err := requireActiveProject(ctx, tx, in.ProjectID); err != nil {
		return nil, err
	}
	meta, err := normalizeMeta(in.Metadata)
	if err != nil {
		return nil, err
	}
	profileID, err := profileID(ctx, tx, in.Profile)
	if err != nil {
		return nil, err
	}
	id, err := newID(ctx, tx, in.Prefix)
	if err != nil {
		return nil, err
	}
	status := Ready
	if in.Draft {
		status = Draft
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO tasks (id, project_id, title, body, status, priority, profile_id, external_ref, metadata,
                   extra_prompt, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.ProjectID, in.Title, in.Body, status, in.Priority, profileID, nullStr(in.ExternalRef),
		meta, in.ExtraPrompt, actor.Name)
	if err != nil {
		return nil, err
	}
	if err := record(ctx, tx, actor, "task.created", id, in.ProjectID, "", status, map[string]any{"title": in.Title}); err != nil {
		return nil, err
	}
	for _, d := range in.Deps {
		if err := addDep(ctx, tx, id, d.Kind, d.On, 0, false, actor); err != nil {
			return nil, err
		}
	}
	return Get(ctx, tx, id)
}

// EditInput lists optional changes; nil pointers are left untouched.
type EditInput struct {
	Title         *string
	Body          *string
	Priority      *int
	Profile       *string
	ExternalRef   *string
	ExtraPrompt   *string
	Metadata      json.RawMessage   // replaces the whole object
	MetaSet       map[string]string // merged keys; values parsed as JSON when valid
	AddDeps       []DepRef
	RemoveDeps    []DepRef
	ExpectVersion int64 // 0 = no check
}

// Edit updates task fields (not status) with a version check.
func Edit(ctx context.Context, tx *sql.Tx, id string, in EditInput, actor Actor) (*Task, error) {
	t, err := Get(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(t, in.ExpectVersion); err != nil {
		return nil, err
	}
	sets := []string{}
	args := []any{}
	changed := []string{}
	set := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
		changed = append(changed, col)
	}
	if in.Title != nil {
		if strings.TrimSpace(*in.Title) == "" {
			return nil, apperr.New(apperr.Invalid, "title cannot be empty")
		}
		set("title", *in.Title)
	}
	if in.Body != nil {
		set("body", *in.Body)
	}
	if in.Priority != nil {
		set("priority", *in.Priority)
	}
	if in.Profile != nil {
		pid, err := profileID(ctx, tx, *in.Profile)
		if err != nil {
			return nil, err
		}
		set("profile_id", pid)
	}
	if in.ExternalRef != nil {
		set("external_ref", nullStr(*in.ExternalRef))
	}
	if in.ExtraPrompt != nil {
		set("extra_prompt", *in.ExtraPrompt)
	}
	if in.Metadata != nil || len(in.MetaSet) > 0 {
		base := t.Metadata
		if in.Metadata != nil {
			base = in.Metadata
		}
		meta, err := mergeMeta(base, in.MetaSet)
		if err != nil {
			return nil, err
		}
		set("metadata", meta)
	}
	if len(sets) > 0 {
		args = append(args, id, t.Version)
		res, err := tx.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(sets, ", ")+
			", version = version + 1 WHERE id = ? AND version = ?", args...)
		if err := casResult(res, err, id); err != nil {
			return nil, err
		}
		if err := record(ctx, tx, actor, "task.updated", id, t.ProjectID, "", "", map[string]any{"fields": changed}); err != nil {
			return nil, err
		}
	}
	for _, d := range in.RemoveDeps {
		if err := RemoveDep(ctx, tx, id, d.Kind, d.On, 0, actor); err != nil {
			return nil, err
		}
	}
	for _, d := range in.AddDeps {
		if err := AddDep(ctx, tx, id, d.Kind, d.On, 0, actor); err != nil {
			return nil, err
		}
	}
	return Get(ctx, tx, id)
}

// Transition moves a task to status `to` if the status machine allows it.
func Transition(ctx context.Context, tx *sql.Tx, id, to, reason string, expectVersion int64, actor Actor) (*Task, error) {
	if !ValidStatus(to) {
		return nil, apperr.New(apperr.Invalid, "unknown status %q (want one of %s)", to, strings.Join(Statuses, "|"))
	}
	t, err := Get(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if to == Delivered {
		// Delivery also removes the task's worktree: only Deliver (`otag task deliver`) does it.
		return nil, apperr.New(apperr.InvalidTransition, "task %s: deliver it with `otag task deliver %s`", id, id)
	}
	if to == Claimed {
		if t.Status == Waiting {
			return ClaimWaiting(ctx, tx, id, expectVersion, actor)
		}
		return Claim(ctx, tx, id, expectVersion, actor)
	}
	if err := checkVersion(t, expectVersion); err != nil {
		return nil, err
	}
	if !CanTransition(t.Status, to) {
		return nil, apperr.New(apperr.InvalidTransition, "task %s: cannot go from %s to %s", id, t.Status, to)
	}
	now := db.Now()
	q := `UPDATE tasks SET status = ?, status_reason = ?, status_changed_at = ?, version = version + 1`
	args := []any{to, reason, now}
	switch {
	case Terminal(to):
		q += ", closed_at = ?"
		args = append(args, now)
	case to == Ready || to == Draft:
		// Back in the queue: the previous claim/run no longer owns the task.
		q += ", claimed_by = NULL, claimed_at = NULL, current_run_id = NULL"
	}
	q += " WHERE id = ? AND version = ? AND status = ?"
	args = append(args, id, t.Version, t.Status)
	res, err := tx.ExecContext(ctx, q, args...)
	if err := casResult(res, err, id); err != nil {
		return nil, err
	}
	var data map[string]any
	if reason != "" {
		data = map[string]any{"reason": reason}
	}
	if err := record(ctx, tx, actor, "task.status", id, t.ProjectID, t.Status, to, data); err != nil {
		return nil, err
	}
	return Get(ctx, tx, id)
}

// Deliver moves a done task to delivered (compare-and-swap) with delivered_at and a
// task.delivered event; note is kept as the status reason. The caller (run.Manager.
// DeliverTask) removes the task's worktrees after the commit.
func Deliver(ctx context.Context, tx *sql.Tx, id, note string, expectVersion int64, actor Actor) (*Task, error) {
	t, err := Get(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(t, expectVersion); err != nil {
		return nil, err
	}
	if !CanTransition(t.Status, Delivered) {
		return nil, apperr.New(apperr.InvalidTransition, "task %s is %s; only a done task can be delivered", id, t.Status)
	}
	now := db.Now()
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'delivered', status_reason = ?, status_changed_at = ?,
		delivered_at = ?, version = version + 1 WHERE id = ? AND version = ? AND status = 'done'`, note, now, now, id, t.Version)
	if err := casResult(res, err, id); err != nil {
		return nil, err
	}
	var data map[string]any
	if note != "" {
		data = map[string]any{"note": note}
	}
	if err := record(ctx, tx, actor, "task.delivered", id, t.ProjectID, Done, Delivered, data); err != nil {
		return nil, err
	}
	return Get(ctx, tx, id)
}

// Claim atomically takes a ready, unblocked task. With id == "" it takes the
// highest-priority ready task of projectID. Exactly one concurrent caller wins;
// the others get CONFLICT.
func Claim(ctx context.Context, tx *sql.Tx, id string, expectVersion int64, actor Actor) (*Task, error) {
	return claim(ctx, tx, id, 0, expectVersion, actor)
}

// ClaimWaiting takes over a waiting task whose run has ended (parked while its
// question is open, or ended without releasing the task) for the task's next run:
// waiting -> claimed. While a run of the task has not ended it is CONFLICT: that run
// owns the task. An open question stays open; its answer follows the task.
// `otag task claim` (Claim) still takes only ready tasks.
func ClaimWaiting(ctx context.Context, tx *sql.Tx, id string, expectVersion int64, actor Actor) (*Task, error) {
	t, err := Get(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(t, expectVersion); err != nil {
		return nil, err
	}
	if t.Status != Waiting {
		return nil, apperr.New(apperr.InvalidTransition, "task %s is %s, not waiting", id, t.Status)
	}
	if err := requireActiveProject(ctx, tx, t.ProjectID); err != nil {
		return nil, err
	}
	var live string
	// A parked run is not live: ended with ended_reason parked (P4), or the reserved
	// run status parked.
	err = tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE task_id = ? AND status NOT IN ('ended', 'parked') LIMIT 1`, id).Scan(&live)
	if err == nil {
		return nil, apperr.New(apperr.Conflict, "task %s is waiting in run %s, which has not ended", id, live)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	now := db.Now()
	res, err := tx.ExecContext(ctx, `
UPDATE tasks SET status = 'claimed', claimed_by = ?, claimed_at = ?, status_reason = '',
                 status_changed_at = ?, version = version + 1
WHERE id = ? AND version = ? AND status = 'waiting'`, actor.Name, now, now, id, t.Version)
	if err := casResult(res, err, id); err != nil {
		return nil, err
	}
	if err := record(ctx, tx, actor, "task.claimed", id, t.ProjectID, Waiting, Claimed, nil); err != nil {
		return nil, err
	}
	return Get(ctx, tx, id)
}

// ClaimNext claims the next ready task of a project.
func ClaimNext(ctx context.Context, tx *sql.Tx, projectID int64, actor Actor) (*Task, error) {
	return claim(ctx, tx, "", projectID, 0, actor)
}

func claim(ctx context.Context, tx *sql.Tx, id string, projectID, expectVersion int64, actor Actor) (*Task, error) {
	if id == "" {
		err := tx.QueryRowContext(ctx, `SELECT id FROM ready_tasks WHERE project_id = ?
			ORDER BY priority DESC, created_at, id LIMIT 1`, projectID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.NotFound, "no ready task")
		}
		if err != nil {
			return nil, err
		}
	}
	t, err := Get(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(t, expectVersion); err != nil {
		return nil, err
	}
	switch {
	case t.Status == Claimed || t.Status == Running || t.Status == Waiting || t.Status == Review:
		return nil, apperr.New(apperr.Conflict, "task %s already %s by %s", id, t.Status, t.ClaimedBy)
	case t.Status != Ready:
		return nil, apperr.New(apperr.InvalidTransition, "task %s is %s, only ready tasks can be claimed", id, t.Status)
	}
	if err := requireActiveProject(ctx, tx, t.ProjectID); err != nil {
		return nil, err
	}
	if open := openBlockers(t); len(open) > 0 {
		return nil, apperr.New(apperr.InvalidTransition, "task %s is blocked by open tasks: %s", id, strings.Join(open, ", "))
	}
	now := db.Now()
	res, err := tx.ExecContext(ctx, `
UPDATE tasks SET status = 'claimed', claimed_by = ?, claimed_at = ?, status_reason = '',
                 status_changed_at = ?, version = version + 1
WHERE id = ? AND version = ? AND status = 'ready'
  AND EXISTS (SELECT 1 FROM projects p WHERE p.id = tasks.project_id AND p.retired_at IS NULL)`,
		actor.Name, now, now, id, t.Version)
	if err := casResult(res, err, id); err != nil {
		return nil, err
	}
	if err := record(ctx, tx, actor, "task.claimed", id, t.ProjectID, Ready, Claimed, nil); err != nil {
		return nil, err
	}
	return Get(ctx, tx, id)
}

// SetCurrentRun points a claimed task at the run working on it (CAS on version).
func SetCurrentRun(ctx context.Context, tx *sql.Tx, id, runID string) error {
	t, err := Get(ctx, tx, id)
	if err != nil {
		return err
	}
	if t.Status != Claimed {
		return apperr.New(apperr.InvalidTransition, "task %s is %s, want claimed", id, t.Status)
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET current_run_id = ?, version = version + 1
		WHERE id = ? AND version = ?`, runID, id, t.Version)
	return casResult(res, err, id)
}

func requireActiveProject(ctx context.Context, q db.Querier, projectID int64) error {
	var name string
	var retired sql.NullString
	err := q.QueryRowContext(ctx, `SELECT name, retired_at FROM projects WHERE id = ?`, projectID).Scan(&name, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.NotFound, "project %d not found", projectID)
	}
	if err != nil {
		return err
	}
	if retired.Valid {
		return apperr.New(apperr.InvalidTransition, "project %s is retired", name)
	}
	return nil
}

func openBlockers(t *Task) []string {
	var open []string
	for _, d := range t.Deps {
		if d.Kind == "blocks" && !Terminal(d.Status) {
			open = append(open, d.TaskID)
		}
	}
	return open
}

// AddDep adds edge taskID -> on of kind. For blocks, parent and review_of it
// rejects edges that would close a cycle. Adding an edge bumps the task's version
// (CAS against expectVersion when non-zero); an existing edge is a no-op.
func AddDep(ctx context.Context, tx *sql.Tx, taskID, kind, on string, expectVersion int64, actor Actor) error {
	return addDep(ctx, tx, taskID, kind, on, expectVersion, true, actor)
}

func addDep(ctx context.Context, tx *sql.Tx, taskID, kind, on string, expectVersion int64, bump bool, actor Actor) error {
	if !validKind(kind) {
		return apperr.New(apperr.Invalid, "unknown dependency kind %q (want %s)", kind, strings.Join(DepKinds, "|"))
	}
	if taskID == on {
		return apperr.New(apperr.Cycle, "task %s cannot depend on itself", taskID)
	}
	t, err := Get(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if err := checkVersion(t, expectVersion); err != nil {
		return err
	}
	if _, err := Get(ctx, tx, on); err != nil {
		return err
	}
	if kind != "related" {
		var hit int
		err := tx.QueryRowContext(ctx, `
WITH RECURSIVE reach(id) AS (
    SELECT ?
    UNION
    SELECT d.depends_on_id FROM task_deps d JOIN reach r ON d.task_id = r.id WHERE d.kind = ?
)
SELECT count(*) FROM reach WHERE id = ?`, on, kind, taskID).Scan(&hit)
		if err != nil {
			return err
		}
		if hit > 0 {
			return apperr.New(apperr.Cycle, "adding %s %s %s would create a cycle", taskID, kind, on)
		}
	}
	if kind == "parent" {
		for _, d := range t.Deps {
			if d.Kind == "parent" && d.TaskID != on {
				return apperr.New(apperr.Invalid, "task %s already has parent %s", taskID, d.TaskID)
			}
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on_id, kind) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, taskID, on, kind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if bump {
		if err := bumpVersion(ctx, tx, t); err != nil {
			return err
		}
	}
	return record(ctx, tx, actor, "task.dep_added", taskID, t.ProjectID, "", "", map[string]any{"kind": kind, "on": on})
}

// RemoveDep deletes edge taskID -> on of kind and bumps the task's version (CAS against
// expectVersion when non-zero); a missing edge is a no-op.
func RemoveDep(ctx context.Context, tx *sql.Tx, taskID, kind, on string, expectVersion int64, actor Actor) error {
	t, err := Get(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if err := checkVersion(t, expectVersion); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM task_deps WHERE task_id = ? AND depends_on_id = ? AND kind = ?`,
		taskID, on, kind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err := bumpVersion(ctx, tx, t); err != nil {
		return err
	}
	return record(ctx, tx, actor, "task.dep_removed", taskID, t.ProjectID, "", "", map[string]any{"kind": kind, "on": on})
}

func bumpVersion(ctx context.Context, tx *sql.Tx, t *Task) error {
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET version = version + 1 WHERE id = ? AND version = ?`, t.ID, t.Version)
	return casResult(res, err, t.ID)
}

func validKind(k string) bool { return slices.Contains(DepKinds, k) }

func checkVersion(t *Task, expect int64) error {
	if expect != 0 && t.Version != expect {
		return apperr.New(apperr.Conflict, "task %s is at version %d, expected %d", t.ID, t.Version, expect)
	}
	return nil
}

func casResult(res sql.Result, err error, id string) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return apperr.New(apperr.Conflict, "task %s was modified concurrently", id)
	}
	return nil
}

func record(ctx context.Context, tx *sql.Tx, actor Actor, kind, id string, projectID int64, from, to string, data any) error {
	_, err := event.Record(ctx, tx, event.Event{
		Source: actor.source(), Kind: kind, EntityType: "task", EntityID: id,
		FromStatus: from, ToStatus: to, ProjectID: projectID, TaskID: id, RunID: actor.RunID,
		Actor: actor.Name, Data: data,
	})
	return err
}

func profileID(ctx context.Context, q db.Querier, name string) (sql.NullInt64, error) {
	if name == "" {
		return sql.NullInt64{}, nil
	}
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM profiles WHERE name = ? AND retired_at IS NULL`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, apperr.New(apperr.NotFound, "profile %s not found", name)
	}
	return sql.NullInt64{Int64: id, Valid: true}, err
}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// newID returns an unused "<prefix>-<base36>" id: 3 chars while the space is
// sparse, 4 chars after repeated collisions.
func newID(ctx context.Context, q db.Querier, prefix string) (string, error) {
	return newIDWith(ctx, q, prefix, func() (int, error) {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(base36))))
		if err != nil {
			return 0, err
		}
		return int(i.Int64()), nil
	})
}

func newIDWith(ctx context.Context, q db.Querier, prefix string, randIndex func() (int, error)) (string, error) {
	for attempt := range 24 {
		n := 3
		if attempt >= 8 {
			n = 4
		}
		var sb strings.Builder
		sb.WriteString(prefix + "-")
		for range n {
			i, err := randIndex()
			if err != nil {
				return "", err
			}
			sb.WriteByte(base36[i])
		}
		id := sb.String()
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id = ?`, id).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			return id, nil
		}
	}
	return "", apperr.Retryable(apperr.Conflict, "could not allocate a task id for prefix %s", prefix)
}

func normalizeMeta(m json.RawMessage) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	return mergeMeta(m, nil)
}

func mergeMeta(base json.RawMessage, set map[string]string) (string, error) {
	obj := map[string]any{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &obj); err != nil || obj == nil {
			return "", apperr.New(apperr.Invalid, "metadata must be a JSON object")
		}
	}
	for k, v := range set {
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			obj[k] = parsed
		} else {
			obj[k] = v
		}
	}
	b, err := json.Marshal(obj)
	return string(b), err
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
