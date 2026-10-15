package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/guard"
	"github.com/nesiler/otag/internal/registry"
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
	var deny, push string
	if err := q.QueryRowContext(ctx, `SELECT deny, push FROM projects WHERE id = ?`, r.ProjectID).Scan(&deny, &push); err != nil {
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
	if childPushes(push, r.Role, t.Branch) {
		me.Rules = append(me.Rules[:len(me.Rules)-1], pushRule(t.ID), me.Rules[len(me.Rules)-1])
		me.Commands = append(append([]Command{}, ChildCommands...), pushCommand(t.ID))
	}
	return me, nil
}

// childPushes reports whether a run of role may push its task branch itself: the
// project's push is "child", the run is a child (not a review) and the task has a
// branch.
func childPushes(push, role, branch string) bool {
	return push == registry.PushChild && role == "child" && branch != ""
}

// pushRule is the rule of a child that may push its own branch (push = "child").
func pushRule(taskID string) string {
	return fmt.Sprintf("The project lets you push your branch: commit, then run `otag task push %s` (never git push).", taskID)
}

func pushCommand(taskID string) Command {
	return Command{"otag task push " + taskID, "push your branch to the project's remote (push = child)"}
}
