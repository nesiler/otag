package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/event"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

// Delivery (phase 5b): otag never merges. A done task is ready to deliver; how it is
// delivered (merge, PR, patch) is the project's business. NotifyDone tells the
// coordinator when a task becomes done; DeliverTask (`otag task deliver`) records the
// delivery and removes the task's worktrees. The branch stays unless asked.

// NotifyDone tells the project's coordinator (the human while none is live) that task
// taskID is done and ready to deliver: an info message with its branch, the commits
// since its base and its run's summary. It runs in the caller's transaction, the one
// that moved the task to done, and sends at most one message per task (client_msg_id
// done:<task>). A review task gets none: the task it reviews is the one to deliver.
func NotifyDone(ctx context.Context, tx *sql.Tx, taskID string, a Actor) error {
	t, err := task.Get(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if t.Status != task.Done {
		return nil
	}
	for _, d := range t.Deps {
		if d.Kind == "review_of" {
			return nil
		}
	}
	clientID := "done:" + t.ID
	var sent int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE client_msg_id = ?`, clientID).Scan(&sent); err != nil {
		return err
	}
	if sent > 0 {
		return nil
	}
	body, err := doneBody(ctx, tx, t)
	if err != nil {
		return err
	}
	_, _, err = msg.Send(ctx, tx, msg.SendInput{ProjectID: t.ProjectID, ClientMsgID: clientID, Kind: msg.KindInfo,
		From: msg.Sender{Kind: msg.SenderSystem, Actor: "otag"}, TaskID: t.ID, Body: body,
		To: []msg.Recipient{{Kind: msg.ToRole, Ref: msg.RoleCoordinator}}}, msgActor(a))
	return err
}

// maxNoticeSummary bounds the summary a done notice quotes (a message is at most
// msg.MaxBody; `otag done` takes any summary).
const maxNoticeSummary = 8 * 1024

// doneBody is the text of NotifyDone: what the task's latest done run recorded (its
// summary, and the commit output with the branch and the commits since the base), else
// its latest workspace.
func doneBody(ctx context.Context, q db.Querier, t *task.Task) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s is done and ready to deliver: %s\n", t.ID, t.Title)
	var runID, summary, head, data string
	err := q.QueryRowContext(ctx, `
SELECT r.id,
       coalesce((SELECT value FROM run_outputs WHERE run_id = r.id AND kind = 'summary' ORDER BY id DESC LIMIT 1), ''),
       coalesce((SELECT value FROM run_outputs WHERE run_id = r.id AND kind = 'commit' ORDER BY id DESC LIMIT 1), ''),
       coalesce((SELECT data FROM run_outputs WHERE run_id = r.id AND kind = 'commit' ORDER BY id DESC LIMIT 1), '{}')
FROM runs r WHERE r.task_id = ? AND r.status = 'ended' AND r.ended_reason = 'done'
ORDER BY r.attempt DESC LIMIT 1`, t.ID).Scan(&runID, &summary, &head, &data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var commit struct {
		Branch       string `json:"branch"`
		BaseSHA      string `json:"base_sha"`
		CommitsAhead *int   `json:"commits_ahead"`
		Dirty        bool   `json:"dirty"`
	}
	_ = json.Unmarshal([]byte(data), &commit)
	var mode, path, branch, base string
	err = q.QueryRowContext(ctx, `SELECT mode, path, coalesce(branch, ''), coalesce(base_sha, '')
		FROM workspaces WHERE task_id = ? ORDER BY id DESC LIMIT 1`, t.ID).Scan(&mode, &path, &branch, &base)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if commit.Branch != "" {
		branch = commit.Branch
	}
	if commit.BaseSHA != "" {
		base = commit.BaseSHA
	}
	switch {
	case branch != "":
		fmt.Fprintf(&b, "Branch: %s", branch)
		if commit.CommitsAhead != nil {
			fmt.Fprintf(&b, ", %d commit(s) since base %s", *commit.CommitsAhead, short(base))
		}
		if head != "" {
			fmt.Fprintf(&b, " (head %s)", short(head))
		}
		fmt.Fprintf(&b, "\nWorktree: %s\n", path)
	case mode == workspace.Same:
		fmt.Fprintf(&b, "Workspace: the project checkout %s", path)
		if head != "" {
			fmt.Fprintf(&b, " (head %s)", short(head))
		}
		b.WriteString("\n")
	}
	if commit.Dirty {
		b.WriteString("Note: uncommitted changes were left in the workspace.\n")
	}
	if runID != "" {
		fmt.Fprintf(&b, "Run: %s\nSummary: %s\n", runID, clip(summary, maxNoticeSummary))
	}
	var reviewer, verdict, reason string
	err = q.QueryRowContext(ctx, `SELECT coalesce(reviewer_run_id, reviewer_actor), verdict, reason FROM reviews
		WHERE reviewed_task_id = ? ORDER BY id DESC LIMIT 1`, t.ID).Scan(&reviewer, &verdict, &reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if verdict == Accept {
		fmt.Fprintf(&b, "Review: accepted by %s", reviewer)
		if reason != "" {
			fmt.Fprintf(&b, ": %s", clip(reason, maxNoticeSummary))
		}
		b.WriteString("\n")
	}
	if branch != "" {
		fmt.Fprintf(&b, "Once it is delivered (merged), run: otag task deliver %s [--delete-branch]\n", t.ID)
	} else {
		fmt.Fprintf(&b, "Once it is delivered, run: otag task deliver %s\n", t.ID)
	}
	return clip(b.String(), msg.MaxBody-64), nil // a long title must not fail the done
}

// DeliverInput is `otag task deliver`.
type DeliverInput struct {
	Task string
	Note string // kept as the status reason and on the task.delivered event
	// DeleteBranch deletes the task's branch when the default branch contains it
	// (`git branch -d` semantics); an unmerged branch is kept with a warning.
	DeleteBranch bool
	Force        bool // remove worktrees even with uncommitted changes (they are lost)
}

// DeliverResult reports `otag task deliver`.
type DeliverResult struct {
	Task        string `json:"task"`
	TaskStatus  string `json:"task_status"`
	DeliveredAt string `json:"delivered_at"`
	// Delivered: this call moved the task from done (false when it was delivered before
	// and the call only finished the cleanup).
	Delivered bool `json:"delivered"`
	// ReviewTasks are the task's done review tasks, delivered (and cleaned up) with it.
	ReviewTasks []string          `json:"review_tasks"`
	Target      string            `json:"target_branch"` // the default branch merges are checked against ("" = none)
	Worktrees   []WorktreeCleanup `json:"worktrees"`
	Branches    []BranchState     `json:"branches"`
	Warnings    []string          `json:"warnings"`
}

// WorktreeCleanup is one worktree DeliverTask removed (or tried to).
type WorktreeCleanup struct {
	Task   string `json:"task"`
	Path   string `json:"path"`
	Result string `json:"result"` // removed | gone (deleted before; pruned) | failed
	Error  string `json:"error,omitempty"`
}

// BranchState is a task branch after DeliverTask.
type BranchState struct {
	Task     string `json:"task"`
	Branch   string `json:"branch"`
	Exists   bool   `json:"exists"`
	Unmerged *int   `json:"unmerged"` // commits the target branch does not contain (nil = unknown)
	Deleted  bool   `json:"deleted"`
}

// worktreeRow is a workspace of a delivered task that is not cleaned yet.
type worktreeRow struct{ task, path, branch string }

// DeliverTask delivers a done task: in one transaction it moves the task (and its done
// review tasks) done -> delivered with delivered_at and a task.delivered event, then
// removes their worktrees (`git worktree remove`) and records them cleaned in
// workspaces. Branches stay; with DeleteBranch a branch the default branch contains is
// deleted, an unmerged one is kept with a warning. Delivering an unmerged branch is a
// warning, not an error.
//
// It refuses (changing nothing) while a run of the task or of a review task has not
// ended, while any run that has not ended (any project, any role) works in one of their
// worktrees, when a worktree has uncommitted changes unless Force, and when what is
// left of their ended runs cannot be cleaned up (or tmux cannot be asked). A worktree
// is removed only inside otag's worktree root of the project and only while git lists
// it on the task's branch (workspace.Remove); a path that is another worktree now is
// left alone and only recorded cleaned. Called again on a delivered task it finishes
// what is left (worktrees, branches): a removal that failed after the commit returns
// the result with a retryable error.
func (m *Manager) DeliverTask(ctx context.Context, in DeliverInput) (*DeliverResult, error) {
	in.Note = strings.TrimSpace(in.Note)
	t, err := task.Get(ctx, m.DB, in.Task)
	if err != nil {
		return nil, err
	}
	if t.Status != task.Done && t.Status != task.Delivered {
		return nil, apperr.New(apperr.InvalidTransition, "task %s is %s; only a done task can be delivered", t.ID, t.Status)
	}
	res := &DeliverResult{Task: t.ID, ReviewTasks: []string{}, Worktrees: []WorktreeCleanup{}, Branches: []BranchState{},
		Warnings: []string{}}
	ids := []string{t.ID}
	for _, d := range t.Dependents {
		if d.Kind == "review_of" && (d.Status == task.Done || d.Status == task.Delivered) {
			ids = append(ids, d.TaskID)
			res.ReviewTasks = append(res.ReviewTasks, d.TaskID)
		}
	}
	var project, projectPath string
	if err := m.DB.QueryRowContext(ctx, `SELECT name, path FROM projects WHERE id = ?`, t.ProjectID).
		Scan(&project, &projectPath); err != nil {
		return nil, err
	}
	wts, err := openWorktrees(ctx, m.DB, ids)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(paths.DataDir(m.Getenv), "worktrees", project)
	spec := func(w worktreeRow) workspace.RemoveSpec {
		return workspace.RemoveSpec{ProjectPath: projectPath, Path: w.path, Branch: workspace.BranchFor(w.task), Root: root,
			Force: in.Force}
	}
	if err := m.worktreesFree(ctx, m.DB, ids, wts); err != nil {
		return nil, err
	}
	if !in.Force {
		for _, w := range wts {
			if st, err := workspace.Uncommitted(ctx, spec(w)); err == nil && st != "" {
				return nil, apperr.New(apperr.Conflict, "task %s: %v; commit or discard them, or deliver with --force "+
					"(which discards them)", w.task, &workspace.DirtyError{Path: w.path, Status: st})
			}
		}
	}
	// The branches: the task's own (and its review tasks', which only matter for
	// --delete-branch) against the default branch.
	branches, err := taskBranches(ctx, m.DB, ids)
	if err != nil {
		return nil, err
	}
	if len(branches) > 0 {
		res.Target = workspace.DefaultBranch(ctx, projectPath)
	}
	for _, b := range branches {
		bs := BranchState{Task: b.task, Branch: b.branch, Exists: workspace.BranchExists(ctx, projectPath, b.branch)}
		// A checkout on the task branch itself proves nothing about the merge.
		if bs.Exists && res.Target != "" && res.Target != b.branch {
			if n, err := workspace.Unmerged(ctx, projectPath, b.branch, res.Target); err == nil {
				bs.Unmerged = &n
			}
		}
		if b.task == t.ID && bs.Exists {
			switch {
			case res.Target == "":
				res.Warnings = append(res.Warnings, fmt.Sprintf("the project checkout has no default branch (detached HEAD); "+
					"cannot tell whether %s is merged", b.branch))
			case bs.Unmerged == nil:
				res.Warnings = append(res.Warnings, fmt.Sprintf("cannot tell whether %s is merged into %s", b.branch, res.Target))
			case *bs.Unmerged > 0:
				res.Warnings = append(res.Warnings, fmt.Sprintf("branch %s has %d commit(s) that %s does not contain: "+
					"delivered unmerged", b.branch, *bs.Unmerged, res.Target))
			}
		}
		res.Branches = append(res.Branches, bs)
	}
	// Nothing of the tasks' ended runs may still work in a worktree that is about to go.
	if len(wts) > 0 {
		if err := m.clearTaskRuns(ctx, ids); err != nil {
			return nil, err
		}
	}

	// The check and the move commit together. Afterwards no run can take the worktrees
	// before they are removed: a worktree path belongs to its task, Start takes only a
	// ready or claimed task, never a delivered one, and no project (so no coordinator or
	// same-mode run) may live inside the worktree root (sync and Start refuse it).
	err = m.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := m.worktreesFree(ctx, tx, ids, wts); err != nil {
			return err
		}
		for _, id := range ids {
			cur, err := task.Get(ctx, tx, id)
			if err != nil {
				return err
			}
			if cur.Status == task.Delivered {
				continue
			}
			note := in.Note
			if id != t.ID {
				note = "delivered with " + t.ID
			}
			if _, err := task.Deliver(ctx, tx, id, note, 0, m.Actor.task("")); err != nil {
				return err
			}
			if id == t.ID {
				res.Delivered = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var failed []string
	removed := map[string]bool{} // task -> every worktree of it is gone
	for _, id := range ids {
		removed[id] = true
	}
	for _, w := range wts {
		c := WorktreeCleanup{Task: w.task, Path: w.path}
		got, err := workspace.Remove(ctx, spec(w))
		if err != nil {
			c.Result, c.Error = "failed", oneLine(err.Error())
			removed[w.task] = false
			failed = append(failed, fmt.Sprintf("%s (%s)", w.path, c.Error))
		} else {
			c.Result = got
			if got == workspace.Replaced {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s is another worktree now (not on %s); left alone",
					w.path, workspace.BranchFor(w.task)))
			}
			if err := m.recordCleaned(ctx, t.ProjectID, w, in.Force, got); err != nil {
				return nil, err
			}
		}
		res.Worktrees = append(res.Worktrees, c)
	}
	if in.DeleteBranch {
		for i := range res.Branches {
			b := &res.Branches[i]
			switch {
			case !b.Exists:
				continue
			case !removed[b.Task]:
				res.Warnings = append(res.Warnings, fmt.Sprintf("branch %s kept: its worktree is still there", b.Branch))
				continue
			case res.Target == "" || b.Unmerged == nil:
				res.Warnings = append(res.Warnings, fmt.Sprintf("branch %s kept: no default branch to check it is merged", b.Branch))
				continue
			case *b.Unmerged > 0:
				res.Warnings = append(res.Warnings, fmt.Sprintf("branch %s kept: %d commit(s) not merged into %s",
					b.Branch, *b.Unmerged, res.Target))
				continue
			}
			n, err := workspace.DeleteBranch(ctx, projectPath, b.Branch, res.Target)
			switch {
			case err != nil:
				res.Warnings = append(res.Warnings, fmt.Sprintf("branch %s kept: %s", b.Branch, oneLine(err.Error())))
			case n > 0: // a commit arrived after the check
				b.Unmerged = &n
				res.Warnings = append(res.Warnings, fmt.Sprintf("branch %s kept: %d commit(s) not merged into %s",
					b.Branch, n, res.Target))
			default:
				b.Deleted, b.Exists = true, false
				if err := m.recordEvent(ctx, t.ProjectID, b.Task, "task.branch_deleted",
					map[string]any{"branch": b.Branch, "target": res.Target}); err != nil {
					return nil, err
				}
			}
		}
	}
	cur, err := task.Get(ctx, m.DB, t.ID)
	if err != nil {
		return nil, err
	}
	res.TaskStatus, res.DeliveredAt = cur.Status, cur.DeliveredAt
	if len(failed) > 0 {
		return res, apperr.Retryable(apperr.Unavailable, "task %s is delivered, but its worktree could not be removed: %s; "+
			"fix it and run `otag task deliver %s` again", t.ID, strings.Join(failed, "; "), t.ID)
	}
	return res, nil
}

// worktreesFree refuses while a run of one of the tasks has not ended, or while a run
// that has not ended (any project, any role, queued ones too) works in one of the
// worktrees wts or under it. A run's directory is its latest workspace; one not recorded yet (a
// queued run) is where it will work: the project checkout for a coordinator or a
// same-mode project, else its task's worktree path.
func (m *Manager) worktreesFree(ctx context.Context, q db.Querier, ids []string, wts []worktreeRow) error {
	rows, err := q.QueryContext(ctx, `
SELECT r.id, coalesce(r.task_id, ''), r.role, p.name, p.path, p.workspace_mode, coalesce(w.path, '')
FROM runs r JOIN projects p ON p.id = r.project_id
LEFT JOIN workspaces w ON w.id = (SELECT max(id) FROM workspaces WHERE run_id = r.id)
WHERE r.status <> 'ended' ORDER BY r.id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var runID, taskID, role, project, projectPath, mode, dir string
		if err := rows.Scan(&runID, &taskID, &role, &project, &projectPath, &mode, &dir); err != nil {
			return err
		}
		if slices.Contains(ids, taskID) {
			return apperr.New(apperr.Conflict, "task %s has run %s that has not ended; stop it first (otag run stop %s)",
				taskID, runID, runID)
		}
		if dir == "" {
			switch {
			case role == "coordinator" || mode == workspace.Same:
				dir = projectPath
			case taskID != "":
				dir = paths.Worktree(m.Getenv, project, taskID)
			}
		}
		for _, w := range wts {
			if paths.Within(dir, w.path) {
				return apperr.New(apperr.Conflict, "run %s works in %s, the worktree of task %s", runID, w.path, w.task)
			}
		}
	}
	return rows.Err()
}

// clearTaskRuns kills what is left of the ended runs of the tasks (pending cleanups and
// their orphan windows), and only of them, before their worktrees are removed. Without
// tmux nothing can be verified: the removal is refused (retryable).
func (m *Manager) clearTaskRuns(ctx context.Context, ids []string) error {
	rows, err := m.DB.QueryContext(ctx, `SELECT id, cleanup_pending FROM runs WHERE task_id IN (`+marks(len(ids))+`)
		AND status = 'ended' ORDER BY id`, anys(ids)...)
	if err != nil {
		return err
	}
	pending := map[string]bool{}
	var runs []string
	for rows.Next() {
		var id string
		var p bool
		if err := rows.Scan(&id, &p); err != nil {
			_ = rows.Close()
			return err
		}
		runs = append(runs, id)
		pending[id] = p
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(runs) == 0 {
		return err
	}
	if m.Tmux == nil || !m.Tmux.Available() {
		return apperr.Retryable(apperr.Unavailable, "tmux is not available: cannot verify that nothing of runs %s "+
			"still works in their worktrees", strings.Join(runs, ", "))
	}
	orphans, err := m.Orphans(ctx)
	if err != nil {
		return apperr.Retryable(apperr.Unavailable, "list windows: %v", err)
	}
	byRun := map[string][]Orphan{}
	for _, o := range orphans {
		if o.RunStatus == Ended {
			byRun[o.Run] = append(byRun[o.Run], o)
		}
	}
	var errs []error
	for _, id := range runs {
		if !pending[id] && len(byRun[id]) == 0 {
			continue
		}
		if _, err := m.Cleanup(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
		}
		for _, o := range byRun[id] {
			if err := m.KillOrphan(ctx, o); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return apperr.Retryable(apperr.Unavailable, "an ended run still works in the worktree: %v", err)
	}
	return nil
}

// openWorktrees lists the worktrees of the tasks that are not cleaned yet, one row per
// path (the task's runs share it).
func openWorktrees(ctx context.Context, q db.Querier, ids []string) ([]worktreeRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT task_id, path, coalesce(max(branch), '') FROM workspaces
		WHERE task_id IN (`+marks(len(ids))+`) AND mode = 'worktree' AND status <> 'cleaned'
		GROUP BY task_id, path ORDER BY min(id)`, anys(ids)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []worktreeRow
	for rows.Next() {
		var w worktreeRow
		if err := rows.Scan(&w.task, &w.path, &w.branch); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// taskBranches lists the branches the tasks' workspaces were on, the first task's first.
func taskBranches(ctx context.Context, q db.Querier, ids []string) ([]worktreeRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT task_id, branch FROM workspaces
		WHERE task_id IN (`+marks(len(ids))+`) AND mode = 'worktree' AND branch IS NOT NULL
		GROUP BY task_id, branch ORDER BY task_id <> ?, min(id)`, append(anys(ids), ids[0])...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []worktreeRow
	for rows.Next() {
		var w worktreeRow
		if err := rows.Scan(&w.task, &w.branch); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// recordCleaned marks the workspaces of a removed worktree cleaned, with an event.
func (m *Manager) recordCleaned(ctx context.Context, projectID int64, w worktreeRow, force bool, result string) error {
	return m.DB.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = 'cleaned', cleaned_at = ?
			WHERE task_id = ? AND path = ? AND mode = 'worktree' AND status <> 'cleaned'`, db.Now(), w.task, w.path)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return recordTaskEvent(ctx, tx, m.Actor, projectID, w.task, "task.worktree_removed",
			map[string]any{"path": w.path, "result": result, "force": force})
	})
}

func (m *Manager) recordEvent(ctx context.Context, projectID int64, taskID, kind string, data any) error {
	return m.DB.Tx(ctx, func(tx *sql.Tx) error { return recordTaskEvent(ctx, tx, m.Actor, projectID, taskID, kind, data) })
}

func recordTaskEvent(ctx context.Context, tx *sql.Tx, a Actor, projectID int64, taskID, kind string, data any) error {
	_, err := event.Record(ctx, tx, event.Event{Source: a.source(), Kind: kind, EntityType: "task", EntityID: taskID,
		ProjectID: projectID, TaskID: taskID, Actor: a.Name, Data: data})
	return err
}

func marks(n int) string { return "?" + strings.Repeat(", ?", n-1) }

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
