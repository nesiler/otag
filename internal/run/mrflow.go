package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

// Merge/pull request flow primitives (docs/DAEMON.md "MR/PR flow"): otag pushes a task
// branch when the project allows it (PushTask) and reopens a done task whose merge
// request needs more work (Reopen). Opening the merge request, CI and merging stay
// outside otag: the coordinator or the human does them with the forge's own CLI.

// ReopenInput is `otag task reopen`.
type ReopenInput struct {
	Task          string
	Reason        string
	ExpectVersion int64 // 0 = no check
}

// ReopenResult reports `otag task reopen`.
type ReopenResult struct {
	Task *task.Task `json:"task"`
	// StaleReviews are the accepted reviews that no longer count: the task is reviewed
	// again when it is done again (when review applies).
	StaleReviews int `json:"stale_reviews"`
	// RetractedNotices counts the deliveries of its done notice that were still queued
	// (they expire: the task is not ready to deliver any more).
	RetractedNotices int `json:"retracted_notices"`
}

// Reopen moves a done task back to ready in one transaction (task.Reopen): the same
// worktree and branch get the next run, whose prior-attempt summary carries the
// reason. The task's accepted reviews become stale (they no longer satisfy the review
// gate, and a new review task is created when the task is done again), and its done
// notice is retracted: queued deliveries expire and its notifications are
// acknowledged. A delivered task, a review task and a task with a live run are
// refused.
func (m *Manager) Reopen(ctx context.Context, in ReopenInput) (*ReopenResult, error) {
	var res *ReopenResult
	err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		res, err = ReopenTask(ctx, tx, in, m.Actor)
		return err
	})
	return res, err
}

// ReopenTask is Reopen inside the caller's write transaction.
func ReopenTask(ctx context.Context, tx *sql.Tx, in ReopenInput, a Actor) (*ReopenResult, error) {
	res := &ReopenResult{}
	t, err := task.Reopen(ctx, tx, in.Task, in.Reason, in.ExpectVersion, a.task(""))
	if err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE reviews SET stale_at = ? WHERE reviewed_task_id = ? AND verdict = 'accept'
		AND stale_at IS NULL`, db.Now(), t.ID)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	res.StaleReviews = int(n)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM messages WHERE client_msg_id = ? OR client_msg_id LIKE ?`,
		"done:"+t.ID, "done:"+t.ID+":%")
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		k, err := msg.Retract(ctx, tx, id, "task "+t.ID+" was reopened", msgActor(a))
		if err != nil {
			return nil, err
		}
		res.RetractedNotices += k
	}
	res.Task = t
	return res, nil
}

// Push callers (PushInput.Caller).
const (
	CallerHuman       = "human"
	CallerCoordinator = "coordinator"
	CallerChild       = "child"
)

// PushInput is `otag task push`.
type PushInput struct {
	Task  string
	Force bool // --force-with-lease against the last push otag recorded
	// Caller is who pushes (CallerHuman, CallerCoordinator, CallerChild); the CLI
	// verified a run caller's token. CallerRun is the caller's run (coordinator or
	// child).
	Caller    string
	CallerRun *Run
}

// PushResult reports `otag task push`.
type PushResult struct {
	Task        string   `json:"task"`
	Remote      string   `json:"remote"`
	Branch      string   `json:"branch"`
	SHA         string   `json:"sha"`
	PreviousSHA string   `json:"previous_sha,omitempty"` // the last push otag recorded, if any
	Forced      bool     `json:"forced"`
	At          string   `json:"at"`
	Warnings    []string `json:"warnings"`
	Output      string   `json:"output"` // git's porcelain report
}

// PushTask pushes the task's branch (its stored branch) to the project's remote:
// `git push <remote> <sha>:refs/heads/<branch>`, the same branch name on the remote,
// never another ref and no tags. Who may push is the project's push setting: none
// (nobody), coordinator (the project's coordinator and the human; default) or child
// (also the task's own child run). With Force it is --force-with-lease against the
// commit otag recorded at its last push of that branch to that remote; when otag never
// pushed it, only a remote without the branch is accepted. Uncommitted changes in the
// task's worktree are not pushed (a warning says so). The push is recorded on the task
// (remote, branch, commit, time) with a task.pushed event.
func (m *Manager) PushTask(ctx context.Context, in PushInput) (*PushResult, error) {
	t, err := task.Get(ctx, m.DB, in.Task)
	if err != nil {
		return nil, err
	}
	var projectPath, projectBase, remote, policy string
	if err := m.DB.QueryRowContext(ctx, `SELECT path, base, remote, push FROM projects WHERE id = ?`, t.ProjectID).
		Scan(&projectPath, &projectBase, &remote, &policy); err != nil {
		return nil, err
	}
	if err := pushAllowed(t, policy, in); err != nil {
		return nil, err
	}
	if t.Branch == "" {
		return nil, apperr.New(apperr.InvalidTransition, "task %s has no branch yet: it gets one with its first run "+
			"(a project with workspace = \"same\" has none)", t.ID)
	}
	if remote == "" {
		base := t.Base
		if base == "" {
			base = projectBase
		}
		remote = "origin"
		if r, _, ok := workspace.RemoteTracking(ctx, projectPath, base); ok && base != "" {
			remote = r
		}
	}
	if !workspace.HasRemote(ctx, projectPath, remote) {
		return nil, apperr.New(apperr.Invalid, "remote %s is not configured in %s (set remote in the project's TOML)",
			remote, projectPath)
	}
	res := &PushResult{Task: t.ID, Remote: remote, Branch: t.Branch, Forced: in.Force, Warnings: []string{}}
	if t.Push != nil {
		res.PreviousSHA = t.Push.SHA
	}
	if dir := m.taskWorktree(ctx, t.ID); dir != "" {
		if st, err := workspace.Changes(ctx, dir); err == nil && st != "" {
			res.Warnings = append(res.Warnings, fmt.Sprintf("the worktree %s has uncommitted changes; only commits are pushed",
				dir))
		}
	}
	spec := workspace.PushSpec{ProjectPath: projectPath, Remote: remote, Branch: t.Branch}
	if in.Force {
		lease := ""
		if p := t.Push; p != nil && p.Remote == remote && p.Branch == t.Branch {
			lease = p.SHA
		} else {
			cur, err := workspace.RemoteBranch(ctx, projectPath, remote, t.Branch)
			if err != nil {
				return nil, apperr.Retryable(apperr.Unavailable, "task %s: %v", t.ID, err)
			}
			if cur != "" {
				return nil, apperr.New(apperr.Conflict, "task %s: otag never pushed %s to %s, and the remote branch exists "+
					"(at %s): --force refuses to overwrite what otag did not push; push without --force, or delete the "+
					"remote branch first", t.ID, t.Branch, remote, short(cur))
			}
		}
		spec.Lease = &lease
	}
	out, err := workspace.Push(ctx, spec)
	if err != nil {
		var rej *workspace.PushRejected
		if errors.As(err, &rej) {
			hint := "the remote branch has commits the task branch does not: fetch and look, or push with --force"
			if in.Force {
				hint = "the remote branch moved since otag last pushed it: --force refuses to overwrite it"
			}
			return nil, apperr.New(apperr.Conflict, "task %s: %v (%s)", t.ID, err, hint)
		}
		return nil, apperr.Retryable(apperr.Unavailable, "task %s: %v", t.ID, err)
	}
	res.SHA, res.Output = out.SHA, out.Output
	res.At = time.Now().UTC().Format(db.TimeFormat)
	rec := task.Push{Remote: remote, Branch: t.Branch, SHA: out.SHA, At: res.At}
	actor := m.Actor.task("")
	if in.CallerRun != nil {
		actor = m.Actor.task(in.CallerRun.ID)
	}
	err = m.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := task.RecordPush(ctx, tx, t.ID, rec, t.Push, in.Force, actor)
		return err
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// pushAllowed applies the project's push setting to the caller.
func pushAllowed(t *task.Task, policy string, in PushInput) error {
	if policy == registry.PushNone {
		return apperr.New(apperr.Forbidden, "project %s has push = \"none\": otag does not push its branches", t.Project)
	}
	switch in.Caller {
	case CallerHuman:
		return nil
	case CallerCoordinator:
		if in.CallerRun == nil || in.CallerRun.ProjectID != t.ProjectID {
			return apperr.New(apperr.Forbidden, "only the coordinator of project %s pushes its tasks", t.Project)
		}
		return nil
	case CallerChild:
		r := in.CallerRun
		switch {
		case r == nil:
		case policy != registry.PushChild:
			return apperr.New(apperr.Forbidden, "project %s has push = %q: the coordinator or the human pushes; a child "+
				"commits and runs `otag done`", t.Project, policy)
		case r.Role != "child" || r.TaskID != t.ID:
			return apperr.New(apperr.Forbidden, "run %s may push only its own task's branch (push = \"child\")", r.ID)
		default:
			return nil
		}
	}
	return apperr.New(apperr.Forbidden, "who pushes is not known")
}

// taskWorktree is the task's latest worktree when it still exists ("" otherwise).
func (m *Manager) taskWorktree(ctx context.Context, taskID string) string {
	var dir string
	if err := m.DB.QueryRowContext(ctx, `SELECT path FROM workspaces WHERE task_id = ? AND mode = 'worktree'
		AND status <> 'cleaned' ORDER BY id DESC LIMIT 1`, taskID).Scan(&dir); err != nil {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return strings.TrimSpace(dir)
}
