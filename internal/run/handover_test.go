package run

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

// gitRepo makes a repository with one commit and the project's path point at it, with
// otag's data directory (and so its worktree root) next to it.
func gitRepo(t *testing.T, m *Manager) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	m.Getenv = func(k string) string {
		if k == "OTAG_DATA_DIR" {
			return filepath.Join(dir, "data")
		}
		return ""
	}
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(repo, 0o755)
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "config", "user.email", "t@x")
	gitRun(t, repo, "config", "commit.gpgsign", "false")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	if _, err := m.DB.Exec(`UPDATE projects SET path = ?`, repo); err != nil {
		t.Fatal(err)
	}
	return repo
}

// taskWorktree prepares task's worktree in repo, records it for run, and commits n
// files on its branch.
func taskWorktree(t *testing.T, m *Manager, repo, taskID, runID string, n int) string {
	t.Helper()
	path := paths.Worktree(m.Getenv, "x", taskID)
	ws, err := workspace.Prepare(context.Background(), workspace.Spec{Mode: workspace.Worktree, ProjectPath: repo, Path: path,
		Branch: workspace.BranchFor("otag/", taskID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB.Exec(`INSERT INTO workspaces (project_id, task_id, run_id, mode, path, branch, base_ref, base_sha, status)
		VALUES (1, ?, ?, 'worktree', ?, ?, 'main', ?, 'active')`, taskID, runID, path, ws.Branch, ws.BaseSHA); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		name := string(rune('a' + i))
		_ = os.WriteFile(filepath.Join(path, name), []byte(name), 0o644)
		gitRun(t, path, "add", name)
		gitRun(t, path, "commit", "-q", "-m", name)
	}
	return path
}

func doneMessages(t *testing.T, m *Manager) []*msg.Message {
	t.Helper()
	rows, err := m.DB.Query(`SELECT id FROM messages WHERE client_msg_id LIKE 'done:%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*msg.Message
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		mm, err := msg.Get(context.Background(), m.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, mm)
	}
	return out
}

// A task that becomes done tells the coordinator (the human while none is live) in the
// same transaction: branch, commits since base and the summary; once per task.
func TestDoneNotifiesCoordinator(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	repo := gitRepo(t, m)
	taskWorktree(t, m, repo, "x-1", "x-1-r1", 2)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", "built it"); err != nil {
		t.Fatal(err)
	}
	ms := doneMessages(t, m)
	if len(ms) != 1 {
		t.Fatalf("%d done messages", len(ms))
	}
	got := ms[0]
	if got.ClientMsgID != "done:x-1" || got.Kind != msg.KindInfo || got.From.Kind != msg.SenderSystem || *got.Task != "x-1" ||
		len(got.Deliveries) != 1 || got.Deliveries[0].To.Kind != msg.ToRole || got.Deliveries[0].State != msg.Queued {
		t.Fatalf("message %+v", got)
	}
	for _, want := range []string{"Task x-1 is done and ready to deliver: t", "Branch: otag/x-1, 2 commit(s) since base",
		"Run: x-1-r1", "Summary: built it", "otag task deliver x-1"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, got.Body)
		}
	}
	// Once per task.
	if err := m.DB.Tx(ctx, func(tx *sql.Tx) error { return NotifyDone(ctx, tx, "x-1", m.Actor) }); err != nil {
		t.Fatal(err)
	}
	if n := len(doneMessages(t, m)); n != 1 {
		t.Fatalf("%d done messages after a second notice", n)
	}
}

// A huge summary never fails otag done: the notice quotes it clipped.
func TestDoneNoticeClipsSummary(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", strings.Repeat("ü", 30*1024)); err != nil {
		t.Fatal(err)
	}
	ms := doneMessages(t, m)
	if len(ms) != 1 || len(ms[0].Body) > msg.MaxBody || !strings.Contains(ms[0].Body, "bytes clipped") {
		t.Fatalf("notices %d", len(ms))
	}
}

// With a review profile the task is not done until the review accepts it: the notice
// comes then (with the verdict), and never for the review task.
func TestReviewAcceptNotifies(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	rt, rr := reviewSetup(t, m)
	if n := len(doneMessages(t, m)); n != 0 {
		t.Fatalf("notice while in review: %d", n)
	}
	if _, err := m.Review(ctx, ReviewInput{Task: rt, Verdict: Accept, Reason: "solid", RunID: rr}); err != nil {
		t.Fatal(err)
	}
	ms := doneMessages(t, m)
	if len(ms) != 1 || ms[0].ClientMsgID != "done:x-1" || !strings.Contains(ms[0].Body, "Review: accepted by "+rr+": solid") ||
		!strings.Contains(ms[0].Body, "Summary: built it") {
		t.Fatalf("notices %+v", ms)
	}
	if tk, _ := task.Get(ctx, m.DB, rt); tk.Status != task.Done {
		t.Fatalf("review task %s", tk.Status)
	}
}

// otag task deliver: refused while a run lives or the worktree is dirty; then done ->
// delivered, the worktree removed and recorded cleaned, the unmerged branch kept with
// a warning. Run again after the merge it deletes the branch.
func TestDeliverTask(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	repo := gitRepo(t, m)
	path := taskWorktree(t, m, repo, "x-1", "x-1-r1", 1)
	deliver := func(in DeliverInput) (*DeliverResult, error) {
		in.Task = "x-1"
		return m.DeliverTask(ctx, in)
	}
	if _, err := deliver(DeliverInput{}); apperr.Code(err) != apperr.InvalidTransition {
		t.Fatalf("deliver a running task: %v", err)
	}
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", "built it"); err != nil {
		t.Fatal(err)
	}
	// A run of the task that has not ended (here: put back by hand) holds it.
	if _, err := m.DB.Exec(`UPDATE runs SET status = 'idle', ended_reason = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(DeliverInput{}); apperr.Code(err) != apperr.Conflict || !strings.Contains(err.Error(), "x-1-r1") {
		t.Fatalf("deliver with a live run: %v", err)
	}
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'done'`)
	_ = os.WriteFile(filepath.Join(path, "wip"), []byte("x"), 0o644)
	_, err := deliver(DeliverInput{})
	if apperr.Code(err) != apperr.Conflict || !strings.Contains(err.Error(), "uncommitted") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("deliver a dirty worktree: %v", err)
	}
	if tk, _ := task.Get(ctx, m.DB, "x-1"); tk.Status != task.Done {
		t.Fatalf("dirty refusal changed the task: %s", tk.Status)
	}
	_ = os.Remove(filepath.Join(path, "wip"))

	res, err := deliver(DeliverInput{Note: "PR #7", DeleteBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delivered || res.TaskStatus != task.Delivered || res.DeliveredAt == "" || res.Target != "main" ||
		len(res.Worktrees) != 1 || res.Worktrees[0].Result != "removed" || len(res.Branches) != 1 ||
		res.Branches[0].Deleted || !res.Branches[0].Exists || *res.Branches[0].Unmerged != 1 {
		t.Fatalf("deliver: %+v", res)
	}
	if w := strings.Join(res.Warnings, "\n"); !strings.Contains(w, "delivered unmerged") || !strings.Contains(w, "branch otag/x-1 kept: 1 commit(s)") {
		t.Fatalf("warnings %q", w)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) || !workspace.BranchExists(ctx, repo, "otag/x-1") {
		t.Fatalf("worktree still there (%v), or branch gone", err)
	}
	var status, cleaned string
	_ = m.DB.QueryRow(`SELECT status, coalesce(cleaned_at, '') FROM workspaces WHERE task_id = 'x-1'`).Scan(&status, &cleaned)
	if status != "cleaned" || cleaned == "" {
		t.Fatalf("workspace %s %q", status, cleaned)
	}
	if tk, _ := task.Get(ctx, m.DB, "x-1"); tk.StatusReason != "PR #7" {
		t.Fatalf("note %q", tk.StatusReason)
	}

	// After the merge, a second run deletes the branch; nothing else changes.
	gitRun(t, repo, "merge", "-q", "--no-ff", "-m", "merge", "otag/x-1")
	res, err = deliver(DeliverInput{DeleteBranch: true})
	if err != nil || res.Delivered || len(res.Worktrees) != 0 || !res.Branches[0].Deleted || len(res.Warnings) != 0 {
		t.Fatalf("second deliver: %+v %v", res, err)
	}
	if workspace.BranchExists(ctx, repo, "otag/x-1") {
		t.Fatal("merged branch kept")
	}
	var kinds []string
	rows, _ := m.DB.Query(`SELECT kind FROM events WHERE task_id = 'x-1' AND kind IN
		('task.delivered', 'task.worktree_removed', 'task.branch_deleted') ORDER BY id`)
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds = append(kinds, k)
	}
	_ = rows.Close()
	if strings.Join(kinds, " ") != "task.delivered task.worktree_removed task.branch_deleted" {
		t.Fatalf("events %v", kinds)
	}
	if res, err := deliver(DeliverInput{}); err != nil || res.Delivered || len(res.Worktrees) != 0 {
		t.Fatalf("third deliver: %+v %v", res, err)
	}
}

// A worktree that cannot be removed after the commit leaves the task delivered and
// returns a retryable error; running deliver again finishes the cleanup. --force
// removes a dirty worktree.
func TestDeliverTaskRetriesCleanup(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	repo := gitRepo(t, m)
	path := taskWorktree(t, m, repo, "x-1", "x-1-r1", 0)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", "nothing to commit"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(path, "wip"), []byte("x"), 0o644)
	gitRun(t, repo, "worktree", "lock", path) // git refuses to remove a locked worktree
	res, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1", Force: true})
	if e, ok := apperr.As(err); !ok || !e.Retryable || !strings.Contains(e.Message, "again") {
		t.Fatalf("locked worktree: %v", err)
	}
	if res == nil || !res.Delivered || res.Worktrees[0].Result != "failed" || res.Worktrees[0].Error == "" {
		t.Fatalf("partial result %+v", res)
	}
	if tk, _ := task.Get(ctx, m.DB, "x-1"); tk.Status != task.Delivered {
		t.Fatalf("task %s", tk.Status)
	}
	var status string
	_ = m.DB.QueryRow(`SELECT status FROM workspaces WHERE task_id = 'x-1'`).Scan(&status)
	if status == "cleaned" {
		t.Fatal("a worktree that is still there recorded cleaned")
	}
	gitRun(t, repo, "worktree", "unlock", path)
	if _, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1"}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("re-run without --force on a dirty worktree: %v", err)
	}
	res, err = m.DeliverTask(ctx, DeliverInput{Task: "x-1", Force: true})
	if err != nil || res.Delivered || len(res.Worktrees) != 1 || res.Worktrees[0].Result != "removed" {
		t.Fatalf("re-run: %+v %v", res, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("worktree left")
	}
}

// The task's done review tasks are delivered (and their worktrees removed) with it; a
// live review run holds the delivery.
func TestDeliverTaskWithReview(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	rt, rr := reviewSetup(t, m)
	repo := gitRepo(t, m)
	rpath := taskWorktree(t, m, repo, rt, rr, 0)
	if _, err := m.Review(ctx, ReviewInput{Task: "x-1", Verdict: Accept}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("human review while the review run works: %v", err)
	}
	if _, err := m.Review(ctx, ReviewInput{Task: rt, Verdict: Accept, RunID: rr}); err != nil {
		t.Fatal(err)
	}
	res, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1", Note: "merged"})
	if err != nil || !res.Delivered || len(res.ReviewTasks) != 1 || res.ReviewTasks[0] != rt || len(res.Worktrees) != 1 ||
		res.Worktrees[0].Task != rt {
		t.Fatalf("deliver: %+v %v", res, err)
	}
	rtk, _ := task.Get(ctx, m.DB, rt)
	if rtk.Status != task.Delivered || rtk.StatusReason != "delivered with x-1" {
		t.Fatalf("review task %s %q", rtk.Status, rtk.StatusReason)
	}
	if _, err := os.Stat(rpath); !os.IsNotExist(err) {
		t.Fatal("review worktree left")
	}
	// The review task's branch never had commits: it counts as merged and stays (no
	// --delete-branch), without a warning about the reviewed task.
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings %v", res.Warnings)
	}
}

// Removal stays inside what is the task's: a worktree outside otag's root is refused
// (the task stays delivered, retryably), and a path that is another worktree now (on
// another branch) is left alone and only recorded cleaned.
func TestDeliverTaskOnlyRemovesItsOwn(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	repo := gitRepo(t, m)
	path := taskWorktree(t, m, repo, "x-1", "x-1-r1", 0)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", "done"); err != nil {
		t.Fatal(err)
	}
	// Its old path is another worktree of the repository now.
	gitRun(t, repo, "worktree", "remove", path)
	gitRun(t, repo, "worktree", "add", "-q", "-b", "feature", path)
	_ = os.WriteFile(filepath.Join(path, "theirs"), []byte("x"), 0o644)
	// And a worktree recorded outside the root.
	outside := filepath.Join(t.TempDir(), "x-1-old")
	gitRun(t, repo, "worktree", "add", "-q", outside, "otag/x-1")
	_, _ = m.DB.Exec(`INSERT INTO workspaces (project_id, task_id, run_id, mode, path, branch, status)
		VALUES (1, 'x-1', 'x-1-r1', 'worktree', ?, 'otag/x-1', 'kept')`, outside)

	res, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1", Force: true})
	if e, ok := apperr.As(err); !ok || !e.Retryable || res == nil || !res.Delivered || len(res.Worktrees) != 2 {
		t.Fatalf("deliver: %+v %v", res, err)
	}
	got := map[string]WorktreeCleanup{}
	for _, c := range res.Worktrees {
		got[c.Path] = c
	}
	if got[path].Result != workspace.Replaced || got[outside].Result != "failed" || !strings.Contains(got[outside].Error, "outside") {
		t.Fatalf("worktrees %+v", res.Worktrees)
	}
	if _, err := os.Stat(filepath.Join(path, "theirs")); err != nil {
		t.Fatal("the other worktree was touched")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("the worktree outside the root was removed")
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), path+" is another worktree now") {
		t.Fatalf("warnings %v", res.Warnings)
	}
	var cleaned int
	_ = m.DB.QueryRow(`SELECT count(*) FROM workspaces WHERE path = ? AND status = 'cleaned'`, path).Scan(&cleaned)
	if cleaned != 1 {
		t.Fatal("the replaced path not recorded cleaned")
	}
	_ = m.DB.QueryRow(`SELECT count(*) FROM workspaces WHERE path = ? AND status = 'cleaned'`, outside).Scan(&cleaned)
	if cleaned != 0 {
		t.Fatal("the refused path recorded cleaned")
	}
}

// Any run that has not ended and works in the worktree (another task's, here) holds
// the delivery; without tmux the leftovers of the task's ended runs cannot be verified
// gone, so nothing is removed; only the delivered tasks' runs are cleaned up, never
// those of a same-mode project the task's project became.
func TestDeliverTaskChecksRunsAndCleanup(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	repo := gitRepo(t, m)
	path := taskWorktree(t, m, repo, "x-1", "x-1-r1", 0)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB.Exec(`
INSERT INTO tasks (id, project_id, title, status) VALUES ('x-2', 1, 'other', 'running');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('x-2-r1', 1, 'x-2', 'child', 1, 1, 1, 'fake', 'running');
UPDATE tasks SET current_run_id = 'x-2-r1' WHERE id = 'x-2';
INSERT INTO workspaces (project_id, task_id, run_id, mode, path, status) VALUES (1, 'x-2', 'x-2-r1', 'worktree', ?, 'active')`,
		path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1"}); apperr.Code(err) != apperr.Conflict ||
		!strings.Contains(err.Error(), "x-2-r1") {
		t.Fatalf("deliver while another run works there: %v", err)
	}
	// x-2 ends in the project checkout; the project is same-mode now. Its pending
	// cleanup is not the delivery's business.
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'stopped', cleanup_pending = 1 WHERE id = 'x-2-r1';
		UPDATE workspaces SET mode = 'same', path = ? WHERE run_id = 'x-2-r1'; UPDATE projects SET workspace_mode = 'same'`, repo)

	saved := m.Tmux.Bin
	m.Tmux.Bin = "otag-test-no-tmux"
	_, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1"})
	if e, ok := apperr.As(err); !ok || e.Code != apperr.Unavailable || !e.Retryable || !strings.Contains(e.Message, "tmux") {
		t.Fatalf("deliver without tmux: %v", err)
	}
	if tk, _ := task.Get(ctx, m.DB, "x-1"); tk.Status != task.Done {
		t.Fatalf("task %s", tk.Status)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("worktree removed without tmux")
	}
	m.Tmux.Bin = saved
	_, _ = m.DB.Exec(`UPDATE runs SET cleanup_pending = 1 WHERE id = 'x-1-r1'`)
	res, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1"})
	if err != nil || !res.Delivered || res.Worktrees[0].Result != workspace.Removed {
		t.Fatalf("deliver: %+v %v", res, err)
	}
	var own, other int
	_ = m.DB.QueryRow(`SELECT cleanup_pending FROM runs WHERE id = 'x-1-r1'`).Scan(&own)
	_ = m.DB.QueryRow(`SELECT cleanup_pending FROM runs WHERE id = 'x-2-r1'`).Scan(&other)
	if own != 0 || other != 1 {
		t.Fatalf("cleanup_pending: own %d, other %d", own, other)
	}
}

// A run that works under a task's worktree holds its delivery too; and no run starts
// in a project that lives inside the worktree root (synced before sync refused it).
func TestWorktreeRootContainment(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	repo := gitRepo(t, m)
	path := taskWorktree(t, m, repo, "x-1", "x-1-r1", 0)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	if _, err := m.Done(ctx, "x-1-r1", "done"); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(path, "sub")
	_ = os.MkdirAll(sub, 0o755)
	if _, err := m.DB.Exec(`
INSERT INTO projects (id, name, path, prefix, config_hash, workspace_mode) VALUES (2, 'y', ?, 'y', 'h', 'same');
INSERT INTO tasks (id, project_id, title, status) VALUES ('y-1', 2, 'nested', 'ready');
INSERT INTO runs (id, project_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('y-coord-r1', 2, 'coordinator', 1, 1, 1, 'fake', 'queued')`, sub); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1"}); apperr.Code(err) != apperr.Conflict ||
		!strings.Contains(err.Error(), "y-coord-r1") {
		t.Fatalf("deliver while a queued coordinator would work under the worktree: %v", err)
	}
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'stopped' WHERE id = 'y-coord-r1'`)
	if _, _, err := m.create(ctx, StartInput{TaskID: "y-1", Profile: "p"}); apperr.Code(err) != apperr.Invalid ||
		!strings.Contains(err.Error(), "worktree root") {
		t.Fatalf("start in a project inside the worktree root: %v", err)
	}
	if res, err := m.DeliverTask(ctx, DeliverInput{Task: "x-1", Force: true}); err != nil || !res.Delivered {
		t.Fatalf("deliver: %+v %v", res, err)
	}
}
