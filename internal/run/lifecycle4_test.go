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
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

func ask(t *testing.T, m *Manager, runID, body string) *msg.Message {
	t.Helper()
	var q *msg.Message
	err := m.DB.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		q, _, err = msg.Ask(context.Background(), tx, msg.AskInput{RunID: runID, Body: body}, msg.Actor{Name: "run:" + runID})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func closedReason(t *testing.T, m *Manager, id int64) string {
	t.Helper()
	q, err := msg.Get(context.Background(), m.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if q.ClosedReason == nil {
		return ""
	}
	return *q.ClosedReason
}

// otag done and otag fail close the open ask of their task: the question is moot.
func TestDoneAndFailCloseAsk(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	q := ask(t, m, "x-1-r1", "which db?")
	if tk, _ := task.Get(ctx, m.DB, "x-1"); tk.Status != task.Waiting {
		t.Fatalf("task %s", tk.Status)
	}
	res, err := m.Done(ctx, "x-1-r1", "used sqlite")
	if err != nil || res.TaskStatus != task.Done {
		t.Fatalf("done: %+v %v", res, err)
	}
	if r := closedReason(t, m, q.ID); r != msg.ClosedAskerDone {
		t.Fatalf("ask closed %q", r)
	}

	m = testManager(t)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'`)
	q = ask(t, m, "x-1-r1", "which db?")
	r, err := m.Fail(ctx, "x-1-r1", "no access")
	if err != nil || r.TaskStatus != task.Ready {
		t.Fatalf("fail: %+v %v", r, err)
	}
	if r := closedReason(t, m, q.ID); r != msg.ClosedAskerFailed {
		t.Fatalf("ask closed %q", r)
	}
}

// reviewSetup finishes x-1-r1 in a project with a review profile: x-1 goes to review
// with a linked review task, whose run (role review) is returned.
func reviewSetup(t *testing.T, m *Manager) (reviewTask, reviewRun string) {
	t.Helper()
	ctx := context.Background()
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'running'; UPDATE projects SET review_profile_id = 1, prefix = 'x'`)
	res, err := m.Done(ctx, "x-1-r1", "built it")
	if err != nil || res.TaskStatus != task.Review || res.ReviewTask == "" {
		t.Fatalf("done: %+v %v", res, err)
	}
	reviewRun = res.ReviewTask + "-r1"
	if _, err := m.DB.Exec(`
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES (?, 1, ?, 'review', 1, 1, 1, 'fake', 'running');
UPDATE tasks SET status = 'running', current_run_id = ? WHERE id = ?`, reviewRun, res.ReviewTask, reviewRun, res.ReviewTask); err != nil {
		t.Fatal(err)
	}
	return res.ReviewTask, reviewRun
}

// A review run that finishes with otag done never creates another review.
func TestReviewRunDoneDoesNotChain(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	rt, rr := reviewSetup(t, m)
	res, err := m.Done(ctx, rr, "looks fine")
	if err != nil || res.TaskStatus != task.Done || res.ReviewTask != "" {
		t.Fatalf("review run done: %+v %v", res, err)
	}
	var n int
	_ = m.DB.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n)
	if tk, _ := task.Get(ctx, m.DB, rt); n != 2 || tk.Status != task.Done {
		t.Fatalf("%d tasks, review task %s", n, tk.Status)
	}
}

// otag review from the review run: a reviews row linked to the reviewed run (and its
// profile) and the reviewer run, the reviewed task back to ready with the reason, and
// the review run finished (done) in the same transaction, without a new review task.
func TestReviewFromReviewRun(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	rt, rr := reviewSetup(t, m)
	if _, err := m.Review(ctx, ReviewInput{Task: "x-1", Verdict: Reject, RunID: rr}); apperr.Code(err) != apperr.Invalid {
		t.Fatalf("reject without reason: %v", err)
	}
	if _, err := m.Review(ctx, ReviewInput{Task: "x-1", Verdict: Accept, RunID: "x-1-r1"}); apperr.Code(err) != apperr.Forbidden {
		t.Fatalf("review by a child run: %v", err)
	}
	res, err := m.Review(ctx, ReviewInput{Task: rt, Verdict: Reject, Reason: "no tests", RunID: rr})
	if err != nil || res.Task != "x-1" || res.TaskStatus != task.Ready || res.ReviewedRun != "x-1-r1" || res.Profile != "p" ||
		res.Done == nil || res.Done.TaskStatus != task.Done || res.Done.ReviewTask != "" {
		t.Fatalf("review: %+v %v", res, err)
	}
	var reviewTask, reviewed, reviewer, verdict, reason string
	_ = m.DB.QueryRow(`SELECT review_task_id, reviewed_run_id, reviewer_run_id, verdict, reason FROM reviews`).
		Scan(&reviewTask, &reviewed, &reviewer, &verdict, &reason)
	if reviewTask != rt || reviewed != "x-1-r1" || reviewer != rr || verdict != Reject || reason != "no tests" {
		t.Fatalf("reviews row %s %s %s %s %s", reviewTask, reviewed, reviewer, verdict, reason)
	}
	tk, _ := task.Get(ctx, m.DB, "x-1")
	if !strings.Contains(tk.StatusReason, "review rejected: no tests") {
		t.Fatalf("status reason %q", tk.StatusReason)
	}
	var rate float64
	var n int
	_ = m.DB.QueryRow(`SELECT reject_rate FROM v_profile_stats WHERE profile = 'p'`).Scan(&rate)
	_ = m.DB.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n)
	if rate != 1 || n != 2 {
		t.Fatalf("reject rate %v, %d tasks", rate, n)
	}
	// The next attempt of x-1 sees the rejection.
	p, _, err := m.create(ctx, StartInput{TaskID: "x-1", Profile: "p"})
	if err != nil || p.prior == nil || !strings.Contains(p.prior.String(), "Review: rejected: no tests") {
		t.Fatalf("retry prior %+v %v", p, err)
	}
}

// The human reviews a task in review: unstarted review tasks are cancelled; one that a
// run works on is a conflict.
func TestReviewByHuman(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	rt, rr := reviewSetup(t, m)
	if _, err := m.Review(ctx, ReviewInput{Task: "x-1", Verdict: Accept}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("review while the review run works: %v", err)
	}
	if err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, _, err := msg.Ask(ctx, tx, msg.AskInput{RunID: rr, Body: "unsure?"}, msg.Actor{Name: "run:" + rr})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'stopped' WHERE id = ?`, rr)
	_, _ = m.DB.Exec(`UPDATE tasks SET status = 'ready', current_run_id = NULL WHERE id = ?`, rt)
	res, err := m.Review(ctx, ReviewInput{Task: "x-1", Verdict: Accept, Reason: "ok"})
	if open, _ := msg.HasOpenAsk(ctx, m.DB, rt); open {
		t.Fatal("the cancelled review task keeps an open ask")
	}
	if err != nil || res.TaskStatus != task.Done || len(res.Cancelled) != 1 || res.Cancelled[0] != rt {
		t.Fatalf("human review: %+v %v", res, err)
	}
	var actor string
	_ = m.DB.QueryRow(`SELECT reviewer_actor FROM reviews`).Scan(&actor)
	if actor != "user:test" {
		t.Fatalf("reviewer %q", actor)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A retry gets a short summary of the previous attempt: how it ended, its error and
// summary, and the commits and uncommitted changes it left; and the messages queued
// for the task, with their markers (claimed for the launch prompt).
func TestPriorAttemptSummary(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	_, _ = m.DB.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'quota', error = 'rate_limit: usage limit reached';
		INSERT INTO run_outputs (run_id, kind, value) VALUES ('x-1-r1', 'summary', 'half of it');
		UPDATE tasks SET status = 'ready', current_run_id = NULL`)
	var info *msg.Message
	err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		info, _, err = msg.Send(ctx, tx, msg.SendInput{ProjectID: 1, From: msg.Sender{Kind: msg.SenderHuman, Actor: "user:test"},
			TaskID: "x-1", Body: "use the v2 API", To: []msg.Recipient{{Kind: msg.ToTask, Ref: "x-1"}}}, msg.Actor{Name: "user:test"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := m.create(ctx, StartInput{TaskID: "x-1", Profile: "p"})
	if err != nil || p.prior == nil || p.messages == nil || len(p.messages.Items) != 1 {
		t.Fatalf("create: %+v %v", p, err)
	}

	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "--allow-empty", "-m", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	for _, msg := range []string{"one", "two"} {
		gitRun(t, repo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "--allow-empty", "-m", msg)
	}
	_ = os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("x"), 0o644)
	ws := workspace.Result{Mode: workspace.Worktree, Path: repo, Branch: "otag/x-1", BaseSHA: base}
	p.prior.readWorkspace(ctx, ws)
	prompt, instructions := prompts(p, ws)
	for _, want := range []string{
		`Previous attempt: run x-1-r1 (profile p) ended "quota": rate_limit: usage limit reached`,
		"Its summary: half of it",
		"Branch otag/x-1: 2 commit(s) since base " + base[:10] + "; uncommitted changes: yes",
		"use the v2 API", msg.Marker(info.Deliveries[0].ID),
		`otag ask "<question>"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(instructions, "otag child agent") || len(strings.Split(instructions, "\n")) > 8 {
		t.Errorf("instructions:\n%s", instructions)
	}
}

// One coordinator per project; Claude only (v1); its checkout keeps same-mode children
// out while it runs; it is never parked.
func TestCoordinator(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	if _, _, err := m.createCoordinator(ctx, CoordInput{Project: "x", Profile: "p"}); apperr.Code(err) != apperr.Invalid {
		t.Fatalf("coordinator on fake: %v", err)
	}
	old := CoordinatorHarnesses
	CoordinatorHarnesses = []string{"fake"}
	t.Cleanup(func() { CoordinatorHarnesses = old })
	p, _, err := m.createCoordinator(ctx, CoordInput{Project: "x", Profile: "p"})
	if err != nil || p.runID != "x-coord-r1" || p.role != "coordinator" || p.mode != workspace.Same || p.window != "coord" {
		t.Fatalf("coordinator: %+v %v", p, err)
	}
	prompt, instructions := prompts(p, workspace.Result{Mode: workspace.Same, Path: "/x"})
	if !strings.Contains(instructions, "coordinator of project x") || !strings.Contains(instructions, "otag task add") ||
		!strings.Contains(prompt, "otag needs") {
		t.Fatalf("coordinator prompts:\n%s\n%s", instructions, prompt)
	}
	if _, _, err := m.createCoordinator(ctx, CoordInput{Project: "x", Profile: "p"}); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("second coordinator: %v", err)
	}
	if live, _ := LiveCoordinator(ctx, m.DB, 1); live != "x-coord-r1" {
		t.Fatalf("live coordinator %q", live)
	}
	if _, err := m.Park(ctx, "x-coord-r1", ParkOptions{}); apperr.Code(err) != apperr.Invalid {
		t.Fatalf("park a coordinator: %v", err)
	}
	// A same-mode child would share the coordinator's checkout.
	_, _ = m.DB.Exec(`UPDATE projects SET workspace_mode = 'same'; UPDATE runs SET status = 'ended', ended_reason = 'done'
		WHERE id = 'x-1-r1'; UPDATE tasks SET status = 'ready', current_run_id = NULL`)
	_, _, err = m.create(ctx, StartInput{TaskID: "x-1", Profile: "p"})
	if apperr.Code(err) != apperr.Conflict || !strings.Contains(err.Error(), "coordinator") {
		t.Fatalf("same-mode child with a coordinator: %v", err)
	}
}
