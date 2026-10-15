package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	"strings"
	"testing"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// pushHarness is a msgHarness whose project alpha is a git repository with a local bare
// remote (origin; no network), and whose task has its branch feature/<task> with one
// commit. It also has a beta coordinator, a child of a second alpha task and a review
// run, all with tokens.
type pushHarness struct {
	*msgHarness
	bare, branch, task2 string
	betaCoordEnv        map[string]string
	child2Env           map[string]string
	reviewEnv           map[string]string
}

func newPushHarness(t *testing.T) *pushHarness {
	t.Helper()
	h := &pushHarness{msgHarness: newMsgHarness(t)}
	initRepo(t, h.alpha)
	h.bare = filepath.Join(h.root, "remote.git")
	gitIn(t, h.root, "init", "-q", "--bare", h.bare)
	gitIn(t, h.alpha, "remote", "add", "origin", h.bare)
	gitIn(t, h.alpha, "push", "-q", "origin", "main")
	h.branch = "feature/" + h.task
	gitIn(t, h.alpha, "checkout", "-q", "-b", h.branch)
	_ = os.WriteFile(filepath.Join(h.alpha, "work.txt"), []byte("work\n"), 0o644)
	gitIn(t, h.alpha, "add", "work.txt")
	gitIn(t, h.alpha, "commit", "-q", "-m", "work")
	gitIn(t, h.alpha, "checkout", "-q", "main")
	_, e := h.json("task", "add", "Second")
	var tk struct{ ID string }
	_ = json.Unmarshal(e.Data, &tk)
	h.task2 = tk.ID
	_, e = h.json("--project", "beta", "task", "add", "Beta")
	_ = json.Unmarshal(e.Data, &tk)
	betaTask := tk.ID

	d, err := db.Open(context.Background(), h.env["OTAG_DB"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	hash := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	insert := `INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status, token_hash)
SELECT ?, pr.id, ?, ?, p.id, pv.id, pv.account_id, pv.harness, 'idle', ?
FROM projects pr, profiles p JOIN profile_revisions pv ON pv.profile_id = p.id AND pv.config_hash = p.config_hash
WHERE pr.name = ? AND p.name = 'sonnet'`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE tasks SET branch = ? WHERE id = ?`, []any{h.branch, h.task}},
		{insert, []any{"be-coord-r1", nil, "coordinator", hash("beta-coord"), "beta"}},
		{insert, []any{h.task2 + "-r1", h.task2, "child", hash("child2"), "alpha"}},
		{insert, []any{betaTask + "-r1", betaTask, "review", hash("review"), "beta"}},
	} {
		if _, err := d.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	h.betaCoordEnv = map[string]string{"OTAG_RUN_ID": "be-coord-r1", run.TokenEnv: "beta-coord"}
	h.child2Env = map[string]string{"OTAG_RUN_ID": h.task2 + "-r1", run.TokenEnv: "child2"}
	h.reviewEnv = map[string]string{"OTAG_RUN_ID": betaTask + "-r1", run.TokenEnv: "review"}
	return h
}

func (h *pushHarness) setPush(policy string) {
	h.t.Helper()
	h.write("projects/alpha.toml", "path = \""+h.alpha+"\"\nprefix = \"al\"\ndefault_profile = \"sonnet\"\npush = \""+policy+"\"\n")
	h.ok("sync")
}

func (h *pushHarness) remoteSHA() string {
	h.t.Helper()
	out, err := runGit(h.bare, "rev-parse", "--verify", "-q", "refs/heads/"+h.branch)
	if err != nil {
		return ""
	}
	return out
}

func runGit(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func (h *pushHarness) db() *db.DB {
	h.t.Helper()
	d, err := db.Open(context.Background(), h.env["OTAG_DB"])
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = d.Close() })
	return d
}

// Who may run `otag task push`, per the project's push setting: coordinator (default)
// lets the project's coordinator and the human push; child also the task's own child
// run; none nobody. Raw git push stays denied to every agent (guard); a run is never
// taken from flags, and a caller with run residue is not the human.
func TestTaskPushAuthorization(t *testing.T) {
	h := newPushHarness(t)
	pushAs := func(extra map[string]string, args ...string) (int, env) {
		h.t.Helper()
		return h.as(extra, append([]string{"task", "push"}, args...)...)
	}
	// push = coordinator (the default).
	code, e := pushAs(h.childEnv, h.task)
	wantCode(t, "child pushes under push = coordinator", code, e, "FORBIDDEN")
	code, e = pushAs(h.betaCoordEnv, h.task)
	wantCode(t, "another project's coordinator pushes", code, e, "FORBIDDEN")
	code, e = pushAs(map[string]string{run.TokenEnv: "child-token"}, h.task)
	wantCode(t, "run residue pushes", code, e, "FORBIDDEN")
	code, e = pushAs(nil, h.task, "--run", h.child)
	wantCode(t, "--run outside a run", code, e, "USAGE")
	code, e = pushAs(map[string]string{"OTAG_RUN_ID": h.coord, run.TokenEnv: "wrong"}, h.task)
	wantCode(t, "coordinator with a wrong token", code, e, "FORBIDDEN")
	if h.remoteSHA() != "" {
		t.Fatal("a refused push reached the remote")
	}
	code, e = pushAs(h.coordEnv, h.task)
	if code != 0 {
		t.Fatalf("coordinator push: %d %+v", code, e.Error)
	}
	var res run.PushResult
	_ = json.Unmarshal(e.Data, &res)
	head := gitIn(t, h.alpha, "rev-parse", h.branch)
	if res.SHA != head || res.Remote != "origin" || res.Branch != h.branch || h.remoteSHA() != head {
		t.Fatalf("pushed %+v; remote at %q, branch at %s", res, h.remoteSHA(), head)
	}
	if code, e := pushAs(nil, h.task); code != 0 {
		t.Fatalf("human push: %d %+v", code, e.Error)
	}

	// push = child: the task's own child too, never another task's child or a review run.
	h.setPush("child")
	code, e = pushAs(h.child2Env, h.task)
	wantCode(t, "a child pushes another task", code, e, "FORBIDDEN")
	code, e = pushAs(h.reviewEnv, h.task)
	wantCode(t, "a review run pushes", code, e, "FORBIDDEN")
	if code, e := pushAs(h.childEnv, h.task); code != 0 {
		t.Fatalf("own child push under push = child: %d %+v", code, e.Error)
	}
	if code, e := pushAs(h.coordEnv, h.task); code != 0 {
		t.Fatalf("coordinator push under push = child: %d %+v", code, e.Error)
	}

	// push = none: nobody.
	h.setPush("none")
	for name, extra := range map[string]map[string]string{"human": nil, "coordinator": h.coordEnv, "child": h.childEnv} {
		code, e := pushAs(extra, h.task)
		wantCode(t, name+" pushes under push = none", code, e, "FORBIDDEN")
	}

	// A task without a branch has nothing to push.
	h.setPush("coordinator")
	code, e = pushAs(nil, h.task2)
	wantCode(t, "a task without a branch", code, e, "INVALID_TRANSITION")
}

// The push record (remote, branch, commit, time) is on the task and in task.pushed;
// --force is --force-with-lease against it: it overwrites only what otag pushed, and
// without a record only a remote that lacks the branch. --request-id replays.
func TestTaskPushRecordAndLease(t *testing.T) {
	h := newPushHarness(t)
	show := func(id string) task.Task {
		t.Helper()
		_, e := h.json("task", "show", id)
		var tk task.Task
		if err := json.Unmarshal(e.Data, &tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	if tk := show(h.task); tk.Push != nil || tk.Branch != h.branch {
		t.Fatalf("before: %+v", tk)
	}
	// Never pushed, the remote has no branch: --force is accepted.
	if code, e := h.json("task", "push", h.task, "--force", "--request-id", "p-1"); code != 0 {
		t.Fatalf("first push --force: %d %+v", code, e.Error)
	}
	first := gitIn(t, h.alpha, "rev-parse", h.branch)
	tk := show(h.task)
	if tk.Push == nil || tk.Push.SHA != first || tk.Push.Remote != "origin" || tk.Push.Branch != h.branch || tk.Push.At == "" {
		t.Fatalf("push record %+v", tk.Push)
	}
	if out := h.ok("task", "show", h.task); !strings.Contains(out, "pushed:") || !strings.Contains(out, "origin/"+h.branch) {
		t.Fatalf("task show:\n%s", out)
	}
	code, out, errs := h.run("task", "push", h.task, "--force", "--request-id", "p-1")
	if code != 0 || !strings.Contains(errs, "replayed") || !strings.Contains(out, "force-pushed") {
		t.Fatalf("replay: %d %s %s", code, out, errs)
	}

	// History rewritten: a plain push is refused, --force (lease = the recorded push) works.
	gitIn(t, h.alpha, "checkout", "-q", h.branch)
	gitIn(t, h.alpha, "commit", "-q", "--amend", "-m", "work, amended")
	gitIn(t, h.alpha, "checkout", "-q", "main")
	amended := gitIn(t, h.alpha, "rev-parse", h.branch)
	code, e := h.json("task", "push", h.task)
	wantCode(t, "non-fast-forward push", code, e, "CONFLICT")
	if code, e := h.json("task", "push", h.task, "--force"); code != 0 {
		t.Fatalf("force push with lease: %d %+v", code, e.Error)
	}
	if h.remoteSHA() != amended || show(h.task).Push.SHA != amended {
		t.Fatalf("remote %s, record %+v, want %s", h.remoteSHA(), show(h.task).Push, amended)
	}

	// Someone else moved the remote branch: --force refuses (the lease no longer holds).
	other := filepath.Join(h.root, "other")
	gitIn(t, h.root, "clone", "-q", "-b", h.branch, h.bare, other)
	gitIn(t, other, "config", "user.name", "o")
	gitIn(t, other, "config", "user.email", "o@example.com")
	gitIn(t, other, "commit", "-q", "--allow-empty", "-m", "theirs")
	gitIn(t, other, "push", "-q", "origin", h.branch)
	theirs := gitIn(t, other, "rev-parse", "HEAD")
	code, e = h.json("task", "push", h.task, "--force")
	wantCode(t, "force over a moved remote", code, e, "CONFLICT")
	if h.remoteSHA() != theirs || show(h.task).Push.SHA != amended {
		t.Fatal("a refused push changed the remote or the record")
	}

	// Never pushed by otag, but the remote has the branch: --force refuses.
	if _, err := h.db().Exec(`UPDATE tasks SET push_remote = NULL, push_branch = NULL, push_sha = NULL, pushed_at = NULL
		WHERE id = ?`, h.task); err != nil {
		t.Fatal(err)
	}
	code, e = h.json("task", "push", h.task, "--force")
	wantCode(t, "force without a record over an existing remote branch", code, e, "CONFLICT")
	if !strings.Contains(e.Error.Message, "never pushed") {
		t.Fatalf("message %q", e.Error.Message)
	}
	var events int
	_ = h.db().QueryRow(`SELECT count(*) FROM events WHERE task_id = ? AND kind = 'task.pushed'`, h.task).Scan(&events)
	if events != 2 {
		t.Fatalf("task.pushed events %d, want 2 (the replay records nothing)", events)
	}
}

// otag task reopen: done -> ready with a reason, by the human or the project's
// coordinator; a child or another project's coordinator cannot; a delivered or review
// task cannot be reopened.
func TestTaskReopenCLI(t *testing.T) {
	h := newPushHarness(t)
	d := h.db()
	if _, err := d.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'done' WHERE id = ?`, h.child); err != nil {
		t.Fatal(err)
	}
	if code, e := h.json("task", "edit", h.task, "--status", "done"); code != 0 {
		t.Fatalf("done: %+v", e.Error)
	}
	code, e := h.as(h.childEnv, "task", "reopen", h.task, "CI is red")
	wantCode(t, "child reopens", code, e, "FORBIDDEN")
	code, e = h.as(h.betaCoordEnv, "task", "reopen", h.task, "CI is red")
	wantCode(t, "another project's coordinator reopens", code, e, "FORBIDDEN")
	code, e = h.json("task", "reopen", h.task)
	wantCode(t, "reopen without a reason", code, e, "USAGE")
	code, e = h.as(h.coordEnv, "task", "reopen", h.task, "CI", "is", "red")
	var res run.ReopenResult
	if code != 0 || json.Unmarshal(e.Data, &res) != nil || res.Task.Status != task.Ready ||
		res.Task.StatusReason != "reopened: CI is red" || res.Task.Branch != h.branch {
		t.Fatalf("coordinator reopens: %d %s %+v", code, e.Data, e.Error)
	}
	code, e = h.json("task", "reopen", h.task, "again")
	wantCode(t, "reopen a ready task", code, e, "INVALID_TRANSITION")
	if out := h.ok("task", "list", "--deliverable"); strings.Contains(out, h.task) {
		t.Fatalf("a reopened task is deliverable:\n%s", out)
	}
	// Delivered: no reopen.
	if _, err := d.Exec(`UPDATE tasks SET status = 'delivered', delivered_at = '2026-10-15T00:00:00.000Z' WHERE id = ?`,
		h.task); err != nil {
		t.Fatal(err)
	}
	code, e = h.json("task", "reopen", h.task, "too late")
	wantCode(t, "reopen a delivered task", code, e, "INVALID_TRANSITION")
	if !strings.Contains(e.Error.Message, "delivered") {
		t.Fatalf("message %q", e.Error.Message)
	}
}
