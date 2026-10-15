package run

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

// addTask inserts a ready task of project 1.
func addTask(t *testing.T, m *Manager, id string, extra string, args ...any) {
	t.Helper()
	if _, err := m.DB.Exec(`INSERT INTO tasks (id, project_id, title, status) VALUES (?, 1, 't', 'ready')`, id); err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		if _, err := m.DB.Exec(extra, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// A new task branch is <branch_prefix><task>; a task that has a branch keeps it,
// whatever the prefix says now; the task's base overrides the project's.
func TestPlanBranchAndBase(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	if _, err := m.DB.Exec(`UPDATE projects SET branch_prefix = 'team/', base = 'origin/dev'`); err != nil {
		t.Fatal(err)
	}
	addTask(t, m, "x-2", "")
	addTask(t, m, "x-3", `UPDATE tasks SET branch = 'otag/x-3', base = 'main' WHERE id = 'x-3'`)
	addTask(t, m, "x-4", `UPDATE tasks SET base = 'release' WHERE id = 'x-4'`)
	for _, c := range []struct {
		task, branch, base string
		known              bool
	}{
		{"x-2", "team/x-2", "origin/dev", false},
		{"x-3", "otag/x-3", "main", true},
		{"x-4", "team/x-4", "release", false},
	} {
		p, _, err := m.create(ctx, StartInput{TaskID: c.task, Profile: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if p.branch != c.branch || p.base != c.base || p.knownBranch != c.known || p.reviewHead != "" {
			t.Errorf("%s: branch %q base %q known %v", c.task, p.branch, p.base, p.knownBranch)
		}
	}
	// Same mode: no branch at all.
	if _, err := m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'stopped'; UPDATE projects SET workspace_mode = 'same'`); err != nil {
		t.Fatal(err)
	}
	addTask(t, m, "x-5", "")
	if p, _, err := m.create(ctx, StartInput{TaskID: "x-5", Profile: "p"}); err != nil || p.branch != "" || p.base != "" {
		t.Fatalf("same mode: %+v %v", p, err)
	}
}

// A review run is planned detached at the reviewed task's head (its latest done run's
// commit), with the reviewed base; a review task that already has a branch (made before
// review checkouts were detached) keeps it.
func TestPlanReviewDetached(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	if _, err := m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'done';
UPDATE tasks SET status = 'review', branch = 'feature/x-1';
INSERT INTO run_outputs (run_id, kind, value, data) VALUES ('x-1-r1', 'commit', 'abc123', '{"base_sha":"base99","branch":"feature/x-1"}')`); err != nil {
		t.Fatal(err)
	}
	addTask(t, m, "x-9", `INSERT INTO task_deps (task_id, depends_on_id, kind) VALUES ('x-9', 'x-1', 'review_of')`)
	p, _, err := m.create(ctx, StartInput{TaskID: "x-9", Profile: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if p.role != "review" || p.branch != "" || p.reviewHead != "abc123" || p.reviewBase != "base99" || p.reviewBranch != "feature/x-1" {
		t.Fatalf("review plan: role %s branch %q head %q base %q reviewed %q", p.role, p.branch, p.reviewHead, p.reviewBase,
			p.reviewBranch)
	}
	ws := workspace.Result{Mode: workspace.Worktree, Path: "/w/x-9", BaseSHA: "abc123"}
	prompt, instructions := prompts(p, ws)
	for _, want := range []string{"detached checkout of the commit under review (head abc123)", "git diff base99...abc123",
		"otag review x-1 --accept"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt lacks %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(instructions, "detached checkout of the commit under review; do not create, move or delete branches") {
		t.Errorf("review rules:\n%s", instructions)
	}
	// A legacy review task with its own branch keeps working on it.
	addTask(t, m, "x-8", `INSERT INTO task_deps (task_id, depends_on_id, kind) VALUES ('x-8', 'x-1', 'review_of');
		UPDATE tasks SET branch = 'otag/x-8' WHERE id = 'x-8'`)
	if p, _, err := m.create(ctx, StartInput{TaskID: "x-8", Profile: "p"}); err != nil || p.branch != "otag/x-8" || p.reviewHead != "" {
		t.Fatalf("legacy review task: %+v %v", p, err)
	}
	// Nothing to review: refused before anything runs.
	addTask(t, m, "x-7", "")
	addTask(t, m, "x-6", `INSERT INTO task_deps (task_id, depends_on_id, kind) VALUES ('x-6', 'x-7', 'review_of')`)
	if _, _, err := m.create(ctx, StartInput{TaskID: "x-6", Profile: "p"}); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("review of a task without a head: %v", err)
	}
}

// --review overrides the project's review_profile: none finishes without a review,
// a profile reviews with that profile, a retired one falls back to the project's.
func TestReviewOverride(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, setup, want string // want: the review task's profile, "" = no review
	}{
		{"project", ``, "p"},
		{"none", `UPDATE tasks SET review_none = 1`, ""},
		{"profile", `UPDATE tasks SET review_profile_id = 2`, "q"},
		{"retired", `UPDATE tasks SET review_profile_id = 2; UPDATE profiles SET retired_at = '2026-01-01T00:00:00.000Z' WHERE id = 2`, "p"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := testManager(t)
			if _, err := m.DB.Exec(`INSERT INTO profiles (id, name, account_id, config_hash) VALUES (2, 'q', 1, 'h');
UPDATE projects SET review_profile_id = 1; UPDATE runs SET status = 'running'`); err != nil {
				t.Fatal(err)
			}
			if c.setup != "" {
				if _, err := m.DB.Exec(c.setup); err != nil {
					t.Fatal(err)
				}
			}
			res, err := m.Done(ctx, "x-1-r1", "built it")
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if res.TaskStatus != task.Done || res.ReviewTask != "" {
					t.Fatalf("no review: %+v", res)
				}
				return
			}
			rt, err := task.Get(ctx, m.DB, res.ReviewTask)
			if err != nil || res.TaskStatus != task.Review || rt.Profile != c.want {
				t.Fatalf("review task %+v (want profile %s): %v", rt, c.want, err)
			}
		})
	}
}

// Reopen: done -> ready with the reason; the accepted review goes stale, the done
// notice is retracted (queued deliveries expire, its notifications are read), the next
// run's prior-attempt summary carries the reason, the next done is reviewed again and
// notified anew. A review task, a task that is not done and a delivered task are
// refused.
func TestReopen(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	rt, rr := reviewSetup(t, m)
	if _, err := m.Review(ctx, ReviewInput{Task: rt, Verdict: Accept, Reason: "solid", RunID: rr}); err != nil {
		t.Fatal(err)
	}
	notices := doneMessages(t, m)
	if len(notices) != 1 {
		t.Fatalf("notices %d", len(notices))
	}
	// One notification the daemon has not shown yet, one it has.
	if _, err := m.DB.Exec(`INSERT INTO notifications (project_id, task_id, message_id, kind, title) VALUES (1, 'x-1', ?, 'msg.human', 'done');
INSERT INTO notifications (project_id, task_id, message_id, kind, title, sent_at, channel)
VALUES (1, 'x-1', ?, 'msg.human', 'done again', '2026-10-15T00:00:00.000Z', 'osascript')`,
		notices[0].ID, notices[0].ID); err != nil {
		t.Fatal(err)
	}
	if pending, _ := notify.Pending(ctx, m.DB, 10); len(pending) != 1 {
		t.Fatalf("fixture: pending %+v", pending)
	}
	if _, err := m.Reopen(ctx, ReopenInput{Task: rt, Reason: "x"}); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("reopen a review task: %v", err)
	}
	if _, err := m.Reopen(ctx, ReopenInput{Task: "x-1", Reason: "  "}); apperr.Code(err) != apperr.Invalid {
		t.Fatalf("reopen without a reason: %v", err)
	}
	if _, err := m.Reopen(ctx, ReopenInput{Task: "x-1", Reason: "x", ExpectVersion: 1}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("reopen at a stale version: %v", err)
	}
	res, err := m.Reopen(ctx, ReopenInput{Task: "x-1", Reason: "CI is red"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Task.Status != task.Ready || res.Task.StatusReason != "reopened: CI is red" || res.Task.ClosedAt != "" ||
		res.Task.CurrentRunID != "" || res.StaleReviews != 1 || res.RetractedNotices != 1 {
		t.Fatalf("reopen: %+v task %+v", res, res.Task)
	}
	if d := delivery(t, m.DB, notices[0].Deliveries[0].ID); d.State != msg.Expired {
		t.Fatalf("notice delivery %s", d.State)
	}
	var unread int
	_ = m.DB.QueryRow(`SELECT count(*) FROM notifications WHERE read_at IS NULL`).Scan(&unread)
	if unread != 0 {
		t.Fatal("the done notice's notification is still unread")
	}
	// The daemon never announces the reopened task as ready to deliver.
	if pending, err := notify.Pending(ctx, m.DB, 10); err != nil || len(pending) != 0 {
		t.Fatalf("pending after the reopen: %+v %v", pending, err)
	}
	var channels []string
	rows, _ := m.DB.Query(`SELECT channel FROM notifications ORDER BY id`)
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		channels = append(channels, c)
	}
	_ = rows.Close()
	if strings.Join(channels, ",") != "retracted,osascript" {
		t.Fatalf("channels %v", channels)
	}
	var data string
	_ = m.DB.QueryRow(`SELECT data FROM events WHERE kind = 'task.reopened' AND task_id = 'x-1'`).Scan(&data)
	if !strings.Contains(data, `"run":"x-1-r1"`) || !strings.Contains(data, `"reason":"CI is red"`) {
		t.Fatalf("event data %s", data)
	}
	if _, err := m.Reopen(ctx, ReopenInput{Task: "x-1", Reason: "again"}); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("reopen a ready task: %v", err)
	}

	// The next run gets the reason; when it is done the task is reviewed again.
	p, _, err := m.create(ctx, StartInput{TaskID: "x-1", Profile: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if p.prior == nil || p.prior.reopened != "CI is red" ||
		!strings.Contains(p.prior.String(), "The task was reopened after it was done: CI is red") {
		t.Fatalf("prior %+v", p.prior)
	}
	if _, err := m.DB.Exec(`UPDATE runs SET status = 'running' WHERE id = ?; UPDATE tasks SET status = 'running' WHERE id = 'x-1'`,
		p.runID); err != nil {
		t.Fatal(err)
	}
	done, err := m.Done(ctx, p.runID, "fixed lint")
	if err != nil || done.TaskStatus != task.Review || done.ReviewTask == "" || done.ReviewTask == rt {
		t.Fatalf("done after the reopen: %+v %v", done, err)
	}
	if _, err := m.Review(ctx, ReviewInput{Task: "x-1", Verdict: Accept}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, n := range doneMessages(t, m) {
		ids[n.ClientMsgID] = true
	}
	if !ids["done:x-1"] || !ids["done:x-1:2"] || len(ids) != 2 {
		t.Fatalf("done notices %v", ids)
	}

	// Delivered: never reopened.
	if _, err := m.DB.Exec(`UPDATE tasks SET status = 'delivered', delivered_at = '2026-10-15T00:00:00.000Z' WHERE id = 'x-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reopen(ctx, ReopenInput{Task: "x-1", Reason: "late"}); apperr.Code(err) != apperr.InvalidTransition ||
		!strings.Contains(err.Error(), "delivered") {
		t.Fatalf("reopen a delivered task: %v", err)
	}
}

// A task reopened before it ever ran (done by hand) gives its first run the reason.
func TestReopenWithoutRuns(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	addTask(t, m, "x-2", `UPDATE tasks SET status = 'done', closed_at = '2026-10-15T00:00:00.000Z' WHERE id = 'x-2'`)
	if _, err := m.Reopen(ctx, ReopenInput{Task: "x-2", Reason: "not really done"}); err != nil {
		t.Fatal(err)
	}
	p, _, err := m.create(ctx, StartInput{TaskID: "x-2", Profile: "p"})
	if err != nil || p.prior != nil || p.reopened != "not really done" {
		t.Fatalf("plan %+v %v", p, err)
	}
	prompt, _ := prompts(p, workspace.Result{Mode: workspace.Worktree, Path: "/w", Branch: p.branch})
	if !strings.Contains(prompt, "The task was reopened: not really done") {
		t.Fatalf("prompt:\n%s", prompt)
	}
}

// --base is frozen once the task has its worktree (branch); --review can change.
func TestEditBaseFrozen(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	edit := func(in task.EditInput) error {
		return m.DB.Tx(ctx, func(tx *sql.Tx) error {
			_, err := task.Edit(ctx, tx, "x-1", in, task.Actor{Name: "user:test"})
			return err
		})
	}
	base, none, bad := "origin/dev", "none", "-x"
	if err := edit(task.EditInput{Base: &base, Review: &none}); err != nil {
		t.Fatal(err)
	}
	if err := edit(task.EditInput{Base: &bad}); apperr.Code(err) != apperr.Invalid {
		t.Fatalf("bad base: %v", err)
	}
	if _, err := m.DB.Exec(`UPDATE tasks SET branch = 'feature/x-1'`); err != nil {
		t.Fatal(err)
	}
	other := "main"
	if err := edit(task.EditInput{Base: &other}); apperr.Code(err) != apperr.InvalidTransition || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("base after the worktree: %v", err)
	}
	if err := edit(task.EditInput{Base: &base}); err != nil {
		t.Fatalf("the same base again: %v", err)
	}
	p := "p"
	if err := edit(task.EditInput{Review: &p}); err != nil {
		t.Fatal(err)
	}
	tk, _ := task.Get(ctx, m.DB, "x-1")
	if tk.Base != "origin/dev" || tk.Review != "p" {
		t.Fatalf("task %+v", tk)
	}
}

// Delivery uses the stored branch (not the current prefix) and removes the review
// task's detached worktree with the task's.
func TestDeliverStoredBranchAndDetachedReview(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	rt, rr := reviewSetup(t, m)
	repo := gitRepo(t, m)
	if _, err := m.DB.Exec(`UPDATE projects SET branch_prefix = 'feature/'`); err != nil {
		t.Fatal(err)
	}
	path := paths.Worktree(m.Getenv, "x", "x-1")
	ws, err := workspace.Prepare(ctx, workspace.Spec{Mode: workspace.Worktree, ProjectPath: repo, Path: path, Branch: "custom/x-1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(path, "a"), []byte("a"), 0o644)
	gitRun(t, path, "add", "a")
	gitRun(t, path, "commit", "-q", "-m", "a")
	head := gitRun(t, path, "rev-parse", "HEAD")
	rpath := paths.Worktree(m.Getenv, "x", rt)
	if _, err := workspace.Prepare(ctx, workspace.Spec{Mode: workspace.Worktree, ProjectPath: repo, Path: rpath, Detach: head}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB.Exec(`
INSERT INTO workspaces (project_id, task_id, run_id, mode, path, branch, base_ref, base_sha, status)
VALUES (1, 'x-1', 'x-1-r1', 'worktree', ?, 'custom/x-1', 'main', ?, 'kept'),
       (1, ?, ?, 'worktree', ?, NULL, NULL, ?, 'active');
UPDATE tasks SET branch = 'custom/x-1' WHERE id = 'x-1'`, path, ws.BaseSHA, rt, rr, rpath, head); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Review(ctx, ReviewInput{Task: rt, Verdict: Accept, RunID: rr}); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "merge", "-q", "--ff-only", "custom/x-1")
	res, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1", DeleteBranch: true})
	if err != nil {
		t.Fatalf("deliver: %+v %v", res, err)
	}
	if len(res.Branches) != 1 || res.Branches[0].Branch != "custom/x-1" || !res.Branches[0].Deleted {
		t.Fatalf("branches %+v (warnings %v)", res.Branches, res.Warnings)
	}
	got := map[string]string{}
	for _, w := range res.Worktrees {
		got[w.Task] = w.Result
	}
	if got["x-1"] != workspace.Removed || got[rt] != workspace.Removed {
		t.Fatalf("worktrees %+v", res.Worktrees)
	}
	for _, p := range []string{path, rpath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left", p)
		}
	}
}

// A squash merge never contains the task branch: --delete-branch keeps it with a
// warning, --delete-branch --force deletes it (git branch -D).
func TestDeliverSquashMergedBranch(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	repo := gitRepo(t, m)
	path := taskWorktree(t, m, repo, "x-1", "x-1-r1", 2)
	if _, err := m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'done';
		UPDATE tasks SET status = 'done', branch = 'otag/x-1'`); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "merge", "-q", "--squash", "otag/x-1")
	gitRun(t, repo, "commit", "-q", "-m", "squashed")
	res, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1", DeleteBranch: true})
	if err != nil || !res.Branches[0].Exists || res.Branches[0].Deleted ||
		!strings.Contains(strings.Join(res.Warnings, "\n"), "--force deletes it anyway") {
		t.Fatalf("deliver: %+v %v", res, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("worktree left")
	}
	res, err = m.DeliverTask(ctx, DeliverInput{Task: "x-1", DeleteBranch: true, Force: true})
	if err != nil || !res.Branches[0].Deleted || workspace.BranchExists(ctx, repo, "otag/x-1") {
		t.Fatalf("deliver --force: %+v %v", res, err)
	}
	var data string
	_ = m.DB.QueryRow(`SELECT data FROM events WHERE kind = 'task.branch_deleted'`).Scan(&data)
	if !strings.Contains(data, `"forced":true`) {
		t.Fatalf("event %s", data)
	}
}
