package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// With a relative core.hooksPath, the hooks that run on a push are those of the working
// tree git runs in. Push runs in Dir (the task's worktree), so the task branch's own
// pre-push hook decides, not the one the main checkout happens to have.
func TestPushRunsHooksOfTheWorktree(t *testing.T) {
	ctx := context.Background()
	project, bare, _ := remoteRepo(t)
	hook := func(dir, body string) {
		t.Helper()
		p := filepath.Join(dir, "hooks", "pre-push")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustGit(t, dir, "add", "hooks/pre-push")
		mustGit(t, dir, "commit", "-q", "-m", "hook")
	}
	// The main checkout carries a pre-push hook that refuses every push.
	hook(project, "echo 'old hook refuses' >&2; exit 1")
	mustGit(t, project, "config", "core.hooksPath", "hooks")

	// The task's worktree is on a branch whose hook allows the push.
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, project, "worktree", "add", "-q", "-b", "feature/h-1", wt)
	hook(wt, "exit 0")
	commitFile(t, wt, "b.txt")
	head := mustGit(t, wt, "rev-parse", "HEAD")

	if _, err := Push(ctx, PushSpec{ProjectPath: project, Remote: "origin", Branch: "feature/h-1"}); err == nil {
		t.Fatal("fixture: the main checkout's hook did not refuse the push")
	}
	res, err := Push(ctx, PushSpec{ProjectPath: project, Dir: wt, Remote: "origin", Branch: "feature/h-1"})
	if err != nil || res.SHA != head {
		t.Fatalf("push from the worktree: %+v %v", res, err)
	}
	if got := mustGit(t, bare, "rev-parse", "refs/heads/feature/h-1"); got != head {
		t.Fatalf("remote has %s, want %s", got, head)
	}
}
