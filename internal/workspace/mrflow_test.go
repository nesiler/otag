package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// remoteRepo makes a bare repository and a project cloned from it (remote origin);
// the remote also has a branch dev one commit ahead of main that the clone has not
// fetched yet. No network: the remote is a local path.
func remoteRepo(t *testing.T) (project, bare, devSHA string) {
	t.Helper()
	ctx := context.Background()
	src := initRepo(t)
	bare = filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, src, "clone", "-q", "--bare", src, bare)
	project = filepath.Join(t.TempDir(), "project")
	mustGit(t, src, "clone", "-q", bare, project)
	for _, kv := range [][2]string{{"user.name", "t"}, {"user.email", "t@example.com"}, {"commit.gpgsign", "false"}} {
		mustGit(t, project, "config", kv[0], kv[1])
	}
	// dev reaches the remote from elsewhere (src), after the project was cloned.
	mustGit(t, src, "checkout", "-q", "-b", "dev")
	commitFile(t, src, "dev.txt")
	mustGit(t, src, "push", "-q", bare, "dev")
	devSHA, _ = git(ctx, src, "rev-parse", "dev")
	return project, bare, devSHA
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A remote-tracking base is fetched before the branch is created, so the task starts
// from what the remote has now; a failed fetch fails the workspace (no silent stale
// base). A local ref or commit is used as it is; one that does not resolve fails.
func TestPrepareBaseFetch(t *testing.T) {
	ctx := context.Background()
	project, bare, devSHA := remoteRepo(t)
	root := t.TempDir()
	if _, err := git(ctx, project, "rev-parse", "--verify", "-q", "refs/remotes/origin/dev"); err == nil {
		t.Fatal("fixture: origin/dev already fetched")
	}
	for i, base := range []string{"origin/dev", "refs/remotes/origin/dev"} {
		path := filepath.Join(root, "t-"+string(rune('a'+i)))
		r, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: path, Branch: "feature/t-" + string(rune('a'+i)),
			Base: base})
		if err != nil {
			t.Fatalf("base %s: %v", base, err)
		}
		if r.BaseSHA != devSHA || r.BaseRef != "origin/dev" {
			t.Fatalf("base %s: %+v, want %s", base, r, devSHA)
		}
		if head, _, _ := Head(ctx, path); head != devSHA {
			t.Fatalf("worktree head %s", head)
		}
	}
	// The remote moves on: the next task starts from the new tip.
	other := filepath.Join(t.TempDir(), "other")
	mustGit(t, bare, "clone", "-q", "-b", "dev", bare, other)
	for _, kv := range [][2]string{{"user.name", "t"}, {"user.email", "t@example.com"}, {"commit.gpgsign", "false"}} {
		mustGit(t, other, "config", kv[0], kv[1])
	}
	commitFile(t, other, "more.txt")
	mustGit(t, other, "push", "-q", "origin", "dev")
	newSHA := mustGit(t, other, "rev-parse", "HEAD")
	r, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, "t-c"), Branch: "feature/t-c",
		Base: "origin/dev"})
	if err != nil || r.BaseSHA != newSHA {
		t.Fatalf("after the remote moved: %+v %v (want %s)", r, err, newSHA)
	}

	// A branch the remote does not have, and a remote that cannot be reached, fail.
	_, err = Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, "t-d"), Branch: "feature/t-d",
		Base: "origin/nope"})
	if err == nil || !strings.Contains(err.Error(), "fetch origin nope failed") {
		t.Fatalf("missing remote branch: %v", err)
	}
	mustGit(t, project, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	_, err = Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, "t-e"), Branch: "feature/t-e",
		Base: "origin/dev"})
	if err == nil || !strings.Contains(err.Error(), "fetch origin dev failed") {
		t.Fatalf("unreachable remote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "t-e")); err == nil {
		t.Fatal("a worktree was created after a failed fetch")
	}
	if BranchExists(ctx, project, "feature/t-e") {
		t.Fatal("a branch was created after a failed fetch")
	}

	// Local refs and commits: no fetch.
	r, err = Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, "t-f"), Branch: "feature/t-f",
		Base: devSHA[:12]})
	if err != nil || r.BaseSHA != devSHA {
		t.Fatalf("commit base: %+v %v", r, err)
	}
	mainSHA := mustGit(t, project, "rev-parse", "main")
	r, err = Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, "t-g"), Branch: "feature/t-g",
		Base: "main"})
	if err != nil || r.BaseSHA != mainSHA || r.BaseRef != "main" {
		t.Fatalf("local base: %+v %v", r, err)
	}
	for _, bad := range []string{"nope", "-x", "--upload-pack=touch /tmp/x", "a..b"} {
		if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, "bad"),
			Branch: "feature/bad", Base: bad}); err == nil {
			t.Errorf("base %q accepted", bad)
		}
	}
	// No base: the project's HEAD, as before.
	r, err = Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, "t-h"), Branch: "feature/t-h"})
	if err != nil || r.BaseSHA != mainSHA || r.BaseRef != "main" {
		t.Fatalf("no base: %+v %v", r, err)
	}
}

func TestRefChecks(t *testing.T) {
	for _, ok := range []string{"", "feature/", "otag/", "t/", "team.x/", "x-"} {
		if err := CheckBranchPrefix(ok); err != nil {
			t.Errorf("prefix %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"feature//", "-x/", "a..b/", "x.lock/", "a b/", "a~/", "/x", ".x/", "refs/heads/x/", "a@{/", "x:/"} {
		if err := CheckBranchPrefix(bad); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
	for _, ok := range []string{"main", "origin/dev", "refs/remotes/origin/dev", "HEAD", "0123abcd", "release-1.2"} {
		if err := CheckRef(ok); err != nil {
			t.Errorf("ref %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"-x", "--all", "a..b", "x^", "x~1", "x y", "x:y", "dev/", "@", ""} {
		if err := CheckRef(bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
}

func TestRemoteHead(t *testing.T) {
	ctx := context.Background()
	project, _, _ := remoteRepo(t)
	if got := RemoteHead(ctx, project); got != "origin/main" {
		t.Fatalf("remote head %q", got)
	}
	if got := RemoteHead(ctx, initRepo(t)); got != "" {
		t.Fatalf("no remote: %q", got)
	}
}

// A review worktree is detached at the reviewed commit: no branch is created, it is
// reused as long as it stays detached, and Remove takes it with Branch "".
func TestDetachedWorktree(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	root := filepath.Join(t.TempDir(), "worktrees", "p")
	task := filepath.Join(root, "p-1")
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: task, Branch: "feature/p-1"}); err != nil {
		t.Fatal(err)
	}
	commitFile(t, task, "work.txt")
	head := mustGit(t, task, "rev-parse", "HEAD")
	branches := mustGit(t, repo, "branch", "--list")

	review := filepath.Join(root, "p-2")
	r, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: review, Detach: head})
	if err != nil {
		t.Fatal(err)
	}
	if r.Branch != "" || r.BaseSHA != head || r.Reused {
		t.Fatalf("detached: %+v", r)
	}
	if got, _, _ := Head(ctx, review); got != head {
		t.Fatalf("review head %s, want %s", got, head)
	}
	if _, err := git(ctx, review, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Fatal("review worktree is on a branch")
	}
	if got := mustGit(t, repo, "branch", "--list"); got != branches {
		t.Fatalf("a branch was created: %q -> %q", branches, got)
	}
	if r, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: review, Detach: head}); err != nil || !r.Reused {
		t.Fatalf("reuse: %+v %v", r, err)
	}
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: filepath.Join(root, "p-3"),
		Detach: strings.Repeat("0", 40)}); err == nil {
		t.Fatal("a commit that is not there accepted")
	}
	// On a branch now: not reused as a review checkout, and not the review's to remove.
	mustGit(t, review, "switch", "-q", "-c", "mine")
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: review, Detach: head}); err == nil {
		t.Fatal("a worktree on a branch reused as a detached review checkout")
	}
	if got, err := Remove(ctx, RemoveSpec{ProjectPath: repo, Path: review, Root: root}); err != nil || got != Replaced {
		t.Fatalf("remove a review path on a branch: %s %v", got, err)
	}
	mustGit(t, review, "switch", "-q", "--detach", head)
	if got, err := Remove(ctx, RemoveSpec{ProjectPath: repo, Path: review, Root: root}); err != nil || got != Removed {
		t.Fatalf("remove the detached review worktree: %s %v", got, err)
	}
	// A task worktree that became detached is still refused (it should be on its branch).
	mustGit(t, task, "switch", "-q", "--detach")
	if _, err := Remove(ctx, RemoveSpec{ProjectPath: repo, Path: task, Branch: "feature/p-1", Root: root}); err == nil {
		t.Fatal("a detached task worktree removed")
	}
}

// Push sends the branch's commit to the remote's branch of the same name and nothing
// else; a non-fast-forward is PushRejected; a lease forces only while the remote is
// where the lease says ("" = the branch must not exist there). otag's push block in the
// environment does not stop it.
func TestPush(t *testing.T) {
	ctx := context.Background()
	project, bare, _ := remoteRepo(t)
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "pusher")
	t.Setenv("GIT_CONFIG_KEY_1", PushBlock[0][0])
	t.Setenv("GIT_CONFIG_VALUE_1", PushBlock[0][1])
	t.Setenv("GIT_CONFIG_KEY_2", PushBlock[1][0])
	t.Setenv("GIT_CONFIG_VALUE_2", PushBlock[1][1])
	if _, err := git(ctx, project, "push", "-q", "origin", "main:refs/heads/blocked"); err == nil {
		t.Fatal("fixture: the push block does not block a plain push")
	}

	mustGit(t, project, "switch", "-q", "-c", "feature/p-1")
	commitFile(t, project, "a.txt")
	mustGit(t, project, "tag", "v-local")
	first := mustGit(t, project, "rev-parse", "HEAD")
	if sha, err := RemoteBranch(ctx, project, "origin", "feature/p-1"); err != nil || sha != "" {
		t.Fatalf("remote branch before: %q %v", sha, err)
	}
	none := ""
	res, err := Push(ctx, PushSpec{ProjectPath: project, Remote: "origin", Branch: "feature/p-1", Lease: &none})
	if err != nil || res.SHA != first {
		t.Fatalf("first push: %+v %v", res, err)
	}
	if got := mustGit(t, bare, "rev-parse", "refs/heads/feature/p-1"); got != first {
		t.Fatalf("remote has %s", got)
	}
	if tags := mustGit(t, bare, "tag", "--list"); tags != "" {
		t.Fatalf("tags pushed: %q", tags)
	}
	if sha, err := RemoteBranch(ctx, project, "origin", "feature/p-1"); err != nil || sha != first {
		t.Fatalf("remote branch after: %q %v", sha, err)
	}

	// Rewritten history: a plain push is rejected, a lease at the last push forces it,
	// a stale lease does not.
	mustGit(t, project, "commit", "-q", "--amend", "-m", "amended")
	amended := mustGit(t, project, "rev-parse", "HEAD")
	_, err = Push(ctx, PushSpec{ProjectPath: project, Remote: "origin", Branch: "feature/p-1"})
	var rej *PushRejected
	if !errors.As(err, &rej) {
		t.Fatalf("non-fast-forward: %v", err)
	}
	if _, err := Push(ctx, PushSpec{ProjectPath: project, Remote: "origin", Branch: "feature/p-1", Lease: &amended}); !errors.As(err, &rej) {
		t.Fatalf("stale lease: %v", err)
	}
	if _, err := Push(ctx, PushSpec{ProjectPath: project, Remote: "origin", Branch: "feature/p-1", Lease: &none}); !errors.As(err, &rej) {
		t.Fatalf("lease 'must not exist' on an existing branch: %v", err)
	}
	if res, err := Push(ctx, PushSpec{ProjectPath: project, Remote: "origin", Branch: "feature/p-1", Lease: &first}); err != nil ||
		res.SHA != amended {
		t.Fatalf("lease push: %+v %v", res, err)
	}
	if got := mustGit(t, bare, "rev-parse", "refs/heads/feature/p-1"); got != amended {
		t.Fatalf("remote has %s after the forced push", got)
	}
	if _, err := Push(ctx, PushSpec{ProjectPath: project, Remote: "-x", Branch: "feature/p-1"}); err == nil {
		t.Fatal("an option as the remote accepted")
	}
	if _, err := Push(ctx, PushSpec{ProjectPath: project, Remote: "origin", Branch: "feature/none"}); err == nil {
		t.Fatal("a missing branch pushed")
	}
}

func TestPushEnv(t *testing.T) {
	env := []string{"A=1", "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=" + PushBlock[0][0], "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=user.name", "GIT_CONFIG_VALUE_1=x", "GIT_CONFIG_KEY_2=" + PushBlock[1][0],
		"GIT_CONFIG_VALUE_2=" + PushBlock[1][1]}
	got := strings.Join(pushEnv(env), " ")
	if got != "A=1 GIT_CONFIG_KEY_0=user.name GIT_CONFIG_VALUE_0=x GIT_CONFIG_COUNT=1" {
		t.Fatalf("pushEnv: %s", got)
	}
	only := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=" + PushBlock[1][0], "GIT_CONFIG_VALUE_0=" + PushBlock[1][1]}
	if got := pushEnv(only); len(got) != 0 {
		t.Fatalf("pushEnv of the block alone: %v", got)
	}
	if got := strings.Join(pushEnv([]string{"B=2"}), " "); got != "B=2" {
		t.Fatalf("no git config: %s", got)
	}
}
