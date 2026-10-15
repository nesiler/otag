package run

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/guard"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

func insertOutput(ctx context.Context, tx *sql.Tx, runID, kind, value string, data any) error {
	b := []byte("{}")
	if data != nil {
		b, _ = json.Marshal(data)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO run_outputs (run_id, kind, value, data) VALUES (?, ?, ?, ?)`,
		runID, kind, value, string(b))
	return err
}

// closeWorkspace records the final HEAD; the worktree is kept for review and merge.
func closeWorkspace(ctx context.Context, tx *sql.Tx, runID, sha string) error {
	_, err := tx.ExecContext(ctx, `UPDATE workspaces SET head_sha = coalesce(?, head_sha), status = 'kept'
		WHERE run_id = ? AND status = 'active'`, nullStr(sha), runID)
	return err
}

// Me is what a child sees with `otag task me`.
type Me struct {
	Task      *task.Task `json:"task"`
	Run       *Run       `json:"run"`
	Rules     []string   `json:"rules"`
	Commands  []Command  `json:"commands"`
	Workspace *Workspace `json:"workspace,omitempty"`
}

// TaskMe returns the task, workspace and rules of a run.
func TaskMe(ctx context.Context, q db.Querier, runID string) (*Me, error) {
	r, err := Get(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	if r.TaskID == "" {
		return nil, apperr.New(apperr.NotFound, "run %s has no task", runID)
	}
	t, err := task.Get(ctx, q, r.TaskID)
	if err != nil {
		return nil, err
	}
	var deny string
	if err := q.QueryRowContext(ctx, `SELECT deny FROM projects WHERE id = ?`, r.ProjectID).Scan(&deny); err != nil {
		return nil, err
	}
	var denyList []string
	_ = json.Unmarshal([]byte(deny), &denyList)
	denyList = guard.DenyGlobs(guard.Level(r.Guard), denyList)
	me := &Me{Task: t, Run: r, Workspace: r.Workspace, Commands: ChildCommands}
	if w := r.Workspace; w != nil {
		me.Rules = Rules(w.Mode, w.Path, w.Branch, denyList)
	} else {
		me.Rules = Rules(workspace.Same, "your workspace", "", denyList)
	}
	return me, nil
}
