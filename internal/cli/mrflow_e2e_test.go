package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// withEnv runs fn with extra variables in the harness environment.
func (h *runHarness) withEnv(extra map[string]string, fn func()) {
	saved := map[string]string{}
	for k, v := range extra {
		saved[k] = h.env[k]
		h.env[k] = v
	}
	defer func() {
		for k, v := range saved {
			if v == "" {
				delete(h.env, k)
			} else {
				h.env[k] = v
			}
		}
	}()
	fn()
}

// The MR/PR flow end to end with real runs (fake harness, private tmux socket, local
// bare remote): a project with base = "origin/dev" and branch_prefix = "team/" starts
// its task from the freshly fetched remote branch on team/<task>; the review run works
// in a detached worktree at the reviewed head where the guard refuses ref moves and
// pushes; the human pushes the task branch, reopens the task with a reason after "CI"
// failed, and the next run continues on the same branch with the reason in its prompt
// and gets a new review.
func TestMRFlowEndToEnd(t *testing.T) {
	h := newRunHarness(t)
	reg, err := os.ReadFile(filepath.Join(h.cfg, "registry.toml"))
	if err != nil {
		t.Fatal(err)
	}
	h.write("registry.toml", string(reg)+`
[profiles.worker]
account = "f"
args = ["start", "busy", "commit", "done:did it", "wait"]
`)
	// The remote has dev one commit ahead of what the project has fetched.
	src := filepath.Join(h.root, "src")
	initRepo(t, src)
	bare := filepath.Join(h.root, "delta.git")
	gitIn(t, h.root, "clone", "-q", "--bare", src, bare)
	repo := filepath.Join(h.root, "delta")
	gitIn(t, h.root, "clone", "-q", bare, repo)
	for _, kv := range [][2]string{{"user.name", "tester"}, {"user.email", "tester@example.com"}, {"commit.gpgsign", "false"}} {
		gitIn(t, repo, "config", kv[0], kv[1])
	}
	gitIn(t, src, "checkout", "-q", "-b", "dev")
	_ = os.WriteFile(filepath.Join(src, "dev.txt"), []byte("dev\n"), 0o644)
	gitIn(t, src, "add", "dev.txt")
	gitIn(t, src, "commit", "-q", "-m", "dev")
	gitIn(t, src, "push", "-q", bare, "dev")
	devSHA := gitIn(t, src, "rev-parse", "HEAD")
	h.write("projects/delta.toml", "path = \""+repo+"\"\nprefix = \"de\"\ndefault_profile = \"worker\"\n"+
		"review_profile = \"review\"\nbase = \"origin/dev\"\nbranch_prefix = \"team/\"\n")
	h.ok("sync")

	id := h.addTask("delta", "Add a line")
	h.start(id)
	r := h.waitRun(id+"-r1", "run done", func(r run.Run) bool { return r.Status == run.Ended && r.TaskStatus == task.Review })
	if r.Workspace == nil || r.Workspace.Branch != "team/"+id || r.Workspace.BaseSHA != devSHA || r.Workspace.BaseRef != "origin/dev" {
		t.Fatalf("workspace %+v (dev %s)", r.Workspace, devSHA)
	}
	head := gitIn(t, r.Workspace.Path, "rev-parse", "HEAD")
	if gitIn(t, r.Workspace.Path, "rev-parse", "HEAD~1") != devSHA {
		t.Fatal("the task branch does not start at the fetched origin/dev")
	}
	var tk task.Task
	_, e := h.json("task", "show", id)
	_ = json.Unmarshal(e.Data, &tk)
	var reviewTask string
	for _, d := range tk.Dependents {
		if d.Kind == "review_of" {
			reviewTask = d.TaskID
		}
	}
	if tk.Branch != "team/"+id || reviewTask == "" {
		t.Fatalf("task %+v", tk)
	}

	// The review run: detached at the reviewed head, no branch of its own.
	h.start(reviewTask)
	rv := h.waitRun(reviewTask+"-r1", "review idle", func(r run.Run) bool { return r.Status == run.Idle })
	// The project has no guard key: light, no native sandbox, so the hook guard alone
	// keeps the detached checkout detached.
	if rv.Guard != "light" || rv.Role != "review" || rv.Workspace == nil || rv.Workspace.Branch != "" || rv.Workspace.BaseSHA != head {
		t.Fatalf("review run %s workspace %+v (head %s)", rv.Role, rv.Workspace, head)
	}
	if got := gitIn(t, rv.Workspace.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("review checkout at %s, want %s", got, head)
	}
	if _, err := runGit(rv.Workspace.Path, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Fatal("the review checkout is on a branch")
	}
	if n := h.count(`SELECT count(*) FROM tasks WHERE id = ? AND branch IS NULL`, reviewTask); n != 1 {
		t.Fatal("the review task got a branch")
	}
	prompt, _ := os.ReadFile(filepath.Join(h.env["OTAG_DATA_DIR"], "runs", rv.ID, "prompt.md"))
	if !strings.Contains(string(prompt), "detached checkout of the commit under review (head "+head+")") ||
		!strings.Contains(string(prompt), "git diff "+devSHA+"..."+head) {
		t.Fatalf("review prompt:\n%s", prompt)
	}
	reviewEnv := map[string]string{"OTAG_RUN_ID": rv.ID, run.TokenEnv: h.token(rv.ID)}
	pre := func(cmd string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"command": cmd})
		_, out, _ := h.hook(reviewEnv, string(b), "fake", "pre_tool")
		return out
	}
	for _, cmd := range []string{"git push origin HEAD", "git branch -f main HEAD", "git branch -D team/" + id,
		"git checkout -B team/" + id, "git update-ref refs/heads/main HEAD", "git switch main", "git checkout main",
		"git checkout -b scratch", "git switch -c scratch", "git branch scratch", "git tag v1"} {
		if out := pre(cmd); !strings.Contains(out, `"decision":"deny"`) {
			t.Errorf("review guard allowed %q: %s", cmd, out)
		}
	}
	if out := pre("git push origin HEAD"); !strings.Contains(out, "otag task push") {
		t.Errorf("push denial does not point to otag task push: %s", out)
	}
	for _, cmd := range []string{"git log --oneline", "git diff " + devSHA + "..." + head, "go test ./...",
		"git switch --detach " + devSHA, "git checkout -- dev.txt", "git commit -m scratch"} {
		if out := pre(cmd); out != `{"decision":"allow"}` {
			t.Errorf("review guard refused %q: %s", cmd, out)
		}
	}
	h.withEnv(reviewEnv, func() {
		if code, e := h.json("prime"); code != 0 || !strings.Contains(string(e.Data), "detached checkout") {
			t.Errorf("review prime: %d %s", code, e.Data)
		}
		if code, e := h.json("review", id, "--accept", "looks right"); code != 0 {
			t.Fatalf("review accept: %+v", e.Error)
		}
	})
	if st := h.taskStatus(id); st != task.Done {
		t.Fatalf("after the review: %s", st)
	}

	// The coordinator's MR flow: push, then CI fails: reopen with the reason.
	if code, e := h.json("task", "push", id); code != 0 {
		t.Fatalf("push: %+v", e.Error)
	}
	if got := gitIn(t, bare, "rev-parse", "refs/heads/team/"+id); got != head {
		t.Fatalf("remote team/%s at %s, want %s", id, got, head)
	}
	if code, e := h.json("task", "reopen", id, "CI is red: lint"); code != 0 {
		t.Fatalf("reopen: %+v", e.Error)
	}
	if n := h.count(`SELECT count(*) FROM reviews WHERE reviewed_task_id = ? AND stale_at IS NOT NULL`, id); n != 1 {
		t.Fatalf("stale reviews %d", n)
	}
	h.start(id)
	r2 := h.waitRun(id+"-r2", "second run done", func(r run.Run) bool { return r.Status == run.Ended && r.TaskStatus == task.Review })
	if r2.Workspace == nil || r2.Workspace.Branch != "team/"+id || r2.Workspace.Path != r.Workspace.Path ||
		r2.Workspace.BaseSHA != devSHA {
		t.Fatalf("second workspace %+v", r2.Workspace)
	}
	prompt, _ = os.ReadFile(filepath.Join(h.env["OTAG_DATA_DIR"], "runs", r2.ID, "prompt.md"))
	if !strings.Contains(string(prompt), "The task was reopened after it was done: CI is red: lint") {
		t.Fatalf("second prompt:\n%s", prompt)
	}
	if gitIn(t, r2.Workspace.Path, "rev-parse", "HEAD~1") != head {
		t.Fatal("the second run did not continue on the pushed head")
	}
	if n := h.count(`SELECT count(*) FROM task_deps WHERE depends_on_id = ? AND kind = 'review_of'`, id); n != 2 {
		t.Fatalf("review tasks %d, want a new one after the reopen", n)
	}
	// Now a plain push fast-forwards the remote branch.
	if code, e := h.json("task", "push", id); code != 0 {
		t.Fatalf("second push: %+v", e.Error)
	}
	if got := gitIn(t, bare, "rev-parse", "refs/heads/team/"+id); got != gitIn(t, r2.Workspace.Path, "rev-parse", "HEAD") {
		t.Fatalf("remote after the second push at %s", got)
	}

	// The remote cannot be reached: a new task's start fails with the fetch error,
	// and the task is ready again (no silent start from a stale origin/dev).
	gitIn(t, repo, "remote", "set-url", "origin", filepath.Join(h.root, "gone.git"))
	id2 := h.addTask("delta", "Another")
	code, e := h.json("run", "start", id2, "--no-attach")
	if code == 0 || e.Error == nil || !strings.Contains(e.Error.Message, "fetch origin dev failed") {
		t.Fatalf("start with an unreachable remote: %d %+v", code, e.Error)
	}
	if st := h.taskStatus(id2); st != task.Ready {
		t.Fatalf("task after the failed start: %s", st)
	}
	if _, err := os.Stat(filepath.Join(h.env["OTAG_DATA_DIR"], "worktrees", "delta", id2)); err == nil {
		t.Fatal("a worktree was created after the failed fetch")
	}
}
