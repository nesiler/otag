package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepo creates a git repository with one commit.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "t"}, {"config", "user.email", "t@example.com"},
		{"config", "commit.gpgsign", "false"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestWorktreeCreateReuse(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	path := filepath.Join(t.TempDir(), "wt", "p", "p-1")
	spec := Spec{Mode: Worktree, ProjectPath: repo, Path: path, Branch: BranchFor("p-1")}
	r, err := Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reused || r.BaseRef != "main" || len(r.BaseSHA) != 40 || r.Branch != "otag/p-1" {
		t.Fatalf("result %+v", r)
	}
	sha, dirty, err := Head(ctx, path)
	if err != nil || sha != r.BaseSHA || dirty {
		t.Fatalf("head %s %v %v", sha, dirty, err)
	}
	// The project's working tree is untouched.
	if st, _ := git(ctx, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("project dirty: %q", st)
	}

	_ = os.WriteFile(filepath.Join(path, "x"), []byte("x"), 0o644)
	if _, dirty, _ := Head(ctx, path); !dirty {
		t.Fatal("dirty not detected")
	}
	_, _ = git(ctx, path, "add", "x")
	if _, err := git(ctx, path, "commit", "-q", "-m", "x"); err != nil {
		t.Fatal(err)
	}
	if n, err := CommitsSince(ctx, path, r.BaseSHA); err != nil || n != 1 {
		t.Fatalf("commits %d %v", n, err)
	}

	r2, err := Prepare(ctx, spec)
	if err != nil || !r2.Reused {
		t.Fatalf("reuse: %+v %v", r2, err)
	}
	// Deleted by hand: recreated from the surviving branch.
	_ = os.RemoveAll(path)
	r3, err := Prepare(ctx, spec)
	if err != nil || r3.Reused {
		t.Fatalf("recreate: %+v %v", r3, err)
	}
	if _, err := os.Stat(filepath.Join(path, "x")); err != nil {
		t.Fatal("branch content lost")
	}
	if !r3.BranchExisted || MergeBase(ctx, repo, "otag/p-1") != r.BaseSHA {
		t.Fatalf("recreated from branch: %+v", r3)
	}
	// A directory inside a checkout on the right branch, or the main checkout, is no worktree.
	_ = os.MkdirAll(filepath.Join(path, "sub"), 0o755)
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: filepath.Join(path, "sub"), Branch: "otag/p-1"}); err == nil {
		t.Fatal("subdirectory accepted as worktree")
	}
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: repo, Branch: "main"}); err == nil {
		t.Fatal("main checkout accepted as worktree")
	}
	// Wrong branch is refused.
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: path, Branch: "otag/other"}); err == nil {
		t.Fatal("wrong branch accepted")
	}
}

func TestSameAndNonRepo(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	r, err := Prepare(ctx, Spec{Mode: Same, ProjectPath: repo})
	if err != nil || r.Path != repo || len(r.BaseSHA) != 40 {
		t.Fatalf("same: %+v %v", r, err)
	}
	plain := t.TempDir()
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: plain, Path: filepath.Join(plain, "wt"), Branch: "otag/x"}); err == nil {
		t.Fatal("worktree of a non-repo accepted")
	}
	if r, err := Prepare(ctx, Spec{Mode: Same, ProjectPath: plain}); err != nil || r.BaseSHA != "" {
		t.Fatalf("same non-repo: %+v %v", r, err)
	}
}
