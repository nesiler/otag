package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commitFile(t *testing.T, dir, name string) {
	t.Helper()
	_ = os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644)
	for _, args := range [][]string{{"add", name}, {"commit", "-q", "-m", name}} {
		if _, err := git(context.Background(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// Remove takes only a linked worktree of the project, inside otag's worktree root, on
// the task's branch: a dirty one only with force, a deleted one is pruned, one that is
// another worktree now is left alone. The branch stays.
func TestRemoveWorktree(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	root := filepath.Join(t.TempDir(), "worktrees", "p")
	prep := func(id string) string {
		path := filepath.Join(root, id)
		if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: path, Branch: BranchFor(id)}); err != nil {
			t.Fatal(err)
		}
		return path
	}
	spec := func(path, id string, force bool) RemoveSpec {
		return RemoveSpec{ProjectPath: repo, Path: path, Branch: BranchFor(id), Root: root, Force: force}
	}
	clean, dirty, gone := prep("p-1"), prep("p-2"), prep("p-3")
	commitFile(t, clean, "a")

	// Never the main checkout, a directory inside a worktree, a plain directory (with
	// something in it: an empty one has nothing to lose), or anything outside the root.
	plain := filepath.Join(root, "plain")
	_ = os.MkdirAll(plain, 0o755)
	_ = os.WriteFile(filepath.Join(plain, "f"), []byte("f"), 0o644)
	_ = os.MkdirAll(filepath.Join(clean, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(clean, "sub", "f"), []byte("f"), 0o644)
	for _, s := range []RemoveSpec{spec(repo, "p-1", true), spec(filepath.Join(clean, "sub"), "p-1", true),
		spec(plain, "p-1", true), {ProjectPath: repo, Path: repo, Branch: "main", Root: filepath.Dir(repo), Force: true}} {
		if _, err := Remove(ctx, s); err == nil {
			t.Errorf("%s removed", s.Path)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatal("main checkout damaged")
	}
	// A worktree of the project outside the root, even on the right branch, and one
	// reached through a symlink in the root.
	outside := filepath.Join(t.TempDir(), "p-9")
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: outside, Branch: "otag/p-9"}); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outside, filepath.Join(root, "p-9"))
	for _, p := range []string{outside, filepath.Join(root, "p-9")} {
		if _, err := Remove(ctx, spec(p, "p-9", true)); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("%s: %v", p, err)
		}
	}
	// The task's path is another worktree of the repository now (another branch): it is
	// left alone, even with force.
	reused := filepath.Join(root, "p-5")
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: reused, Branch: "feature"}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(reused, "work"), []byte("x"), 0o644)
	if got, err := Remove(ctx, spec(reused, "p-5", true)); err != nil || got != Replaced {
		t.Fatalf("reused path: %q %v", got, err)
	}
	if st, err := Uncommitted(ctx, spec(reused, "p-5", false)); err != nil || st != "" {
		t.Fatalf("changes of a replaced worktree: %q %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(reused, "work")); err != nil {
		t.Fatal("reused worktree touched")
	}
	// Detached HEAD: refused.
	_, _ = git(ctx, clean, "checkout", "-q", "--detach")
	if _, err := Remove(ctx, spec(clean, "p-1", true)); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("detached: %v", err)
	}
	_, _ = git(ctx, clean, "checkout", "-q", "otag/p-1")

	_ = os.RemoveAll(filepath.Join(clean, "sub"))
	if got, err := Remove(ctx, spec(clean, "p-1", false)); err != nil || got != Removed {
		t.Fatalf("clean: %q %v", got, err)
	}
	if _, err := os.Stat(clean); !os.IsNotExist(err) || !BranchExists(ctx, repo, "otag/p-1") {
		t.Fatalf("clean worktree still there (%v) or branch gone", err)
	}

	_ = os.WriteFile(filepath.Join(dirty, "untracked"), []byte("x"), 0o644)
	if st, err := Uncommitted(ctx, spec(dirty, "p-2", false)); err != nil || !strings.Contains(st, "untracked") {
		t.Fatalf("uncommitted: %q %v", st, err)
	}
	_, err := Remove(ctx, spec(dirty, "p-2", false))
	var de *DirtyError
	if !errors.As(err, &de) || !strings.Contains(de.Status, "untracked") || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirty, "untracked")); err != nil {
		t.Fatal("dirty worktree touched")
	}
	if got, err := Remove(ctx, spec(dirty, "p-2", true)); err != nil || got != Removed {
		t.Fatalf("force: %q %v", got, err)
	}
	if _, err := os.Stat(dirty); !os.IsNotExist(err) {
		t.Fatal("forced removal left the worktree")
	}

	_ = os.RemoveAll(gone)
	if got, err := Remove(ctx, spec(gone, "p-3", false)); err != nil || got != Gone {
		t.Fatalf("gone: %q %v", got, err)
	}
	if list, _ := git(ctx, repo, "worktree", "list", "--porcelain"); strings.Contains(list, gone) {
		t.Fatal("deleted worktree not pruned")
	}
	// An interrupted removal leaves an empty directory: gone too.
	empty := prep("p-4")
	entries, _ := os.ReadDir(empty)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(empty, e.Name()))
	}
	if got, err := Remove(ctx, spec(empty, "p-4", false)); err != nil || got != Gone {
		t.Fatalf("empty: %q %v", got, err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("empty directory left")
	}
}

// Merged means the target contains every commit of the branch; DeleteBranch keeps an
// unmerged branch, a branch that moved since the check and a checked-out one.
func TestDefaultBranchAndMerge(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	if b := DefaultBranch(ctx, repo); b != "main" {
		t.Fatalf("default branch %q", b)
	}
	root := filepath.Join(t.TempDir(), "wt")
	path := filepath.Join(root, "p-1")
	if _, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: repo, Path: path, Branch: "otag/p-1"}); err != nil {
		t.Fatal(err)
	}
	if n, err := Unmerged(ctx, repo, "otag/p-1", "main"); err != nil || n != 0 {
		t.Fatalf("fresh branch unmerged %d %v", n, err)
	}
	commitFile(t, path, "a")
	commitFile(t, path, "b")
	// Checked out in its worktree: kept, even when merged.
	if _, err := git(ctx, repo, "merge", "-q", "--no-ff", "-m", "merge", "otag/p-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteBranch(ctx, repo, "otag/p-1", "main"); err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("checked-out delete: %v", err)
	}
	commitFile(t, path, "c")
	if _, err := Remove(ctx, RemoveSpec{ProjectPath: repo, Path: path, Branch: "otag/p-1", Root: root}); err != nil {
		t.Fatal(err)
	}
	if n, err := DeleteBranch(ctx, repo, "otag/p-1", "main"); err != nil || n != 1 || !BranchExists(ctx, repo, "otag/p-1") {
		t.Fatalf("unmerged delete: %d %v", n, err)
	}
	// The deletion is a compare-and-swap on both commits checked: a moved branch or a
	// moved target keeps the branch.
	if _, err := git(ctx, repo, "merge", "-q", "--no-ff", "-m", "merge", "otag/p-1"); err != nil {
		t.Fatal(err)
	}
	cur, _ := git(ctx, repo, "rev-parse", "otag/p-1")
	old, _ := git(ctx, repo, "rev-parse", "otag/p-1~1")
	main, _ := git(ctx, repo, "rev-parse", "main")
	mainOld, _ := git(ctx, repo, "rev-parse", "main~1")
	if err := deleteBranchAt(ctx, repo, "otag/p-1", old, "main", main); err == nil || !BranchExists(ctx, repo, "otag/p-1") {
		t.Fatalf("stale branch delete: %v", err)
	}
	if err := deleteBranchAt(ctx, repo, "otag/p-1", cur, "main", mainOld); err == nil || !BranchExists(ctx, repo, "otag/p-1") {
		t.Fatalf("stale target delete: %v", err)
	}
	// A symbolic task ref is never deleted (update-ref would delete what it names).
	_, _ = git(ctx, repo, "symbolic-ref", "refs/heads/otag/sym", "refs/heads/main")
	if _, err := DeleteBranch(ctx, repo, "otag/sym", "main"); err == nil || !strings.Contains(err.Error(), "symbolic") {
		t.Fatalf("symbolic ref: %v", err)
	}
	if !BranchExists(ctx, repo, "main") {
		t.Fatal("main deleted through a symbolic ref")
	}
	_, _ = git(ctx, repo, "symbolic-ref", "--delete", "refs/heads/otag/sym")
	if n, err := DeleteBranch(ctx, repo, "otag/p-1", "main"); err != nil || n != 0 || BranchExists(ctx, repo, "otag/p-1") {
		t.Fatalf("merged delete: %d %v", n, err)
	}
	// origin/HEAD names the default branch when the checkout is elsewhere.
	_, _ = git(ctx, repo, "checkout", "-q", "-b", "feature")
	if b := DefaultBranch(ctx, repo); b != "feature" {
		t.Fatalf("without origin: %q", b)
	}
	_, _ = git(ctx, repo, "update-ref", "refs/remotes/origin/main", "main")
	_, _ = git(ctx, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	if b := DefaultBranch(ctx, repo); b != "main" {
		t.Fatalf("with origin/HEAD: %q", b)
	}
	_, _ = git(ctx, repo, "checkout", "-q", "--detach")
	_, _ = git(ctx, repo, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	if b := DefaultBranch(ctx, repo); b != "" {
		t.Fatalf("detached: %q", b)
	}
}
