package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Starts that fetch the same remote base at the same time, while the remote branch
// keeps moving, all get a worktree: the fetches of one repository run one at a time
// (lockRepo), so none finds its remote-tracking ref moved under it. (Unlocked, a burst
// of parallel fetches fails with "cannot lock ref ... is at X but expected Y".)
func TestConcurrentBaseFetch(t *testing.T) {
	ctx := context.Background()
	project, bare, _ := remoteRepo(t)
	other := filepath.Join(t.TempDir(), "other")
	mustGit(t, bare, "clone", "-q", "-b", "dev", bare, other)
	for _, kv := range [][2]string{{"user.name", "t"}, {"user.email", "t@example.com"}, {"commit.gpgsign", "false"}} {
		mustGit(t, other, "config", kv[0], kv[1])
	}
	lockDir := filepath.Join(t.TempDir(), "locks")
	// Bursts of parallel base resolutions, the remote one commit further each time:
	// every one succeeds and sees the new tip.
	for round := range 5 {
		mustGit(t, other, "commit", "-q", "--allow-empty", "-m", fmt.Sprint("round ", round))
		mustGit(t, other, "push", "-q", "origin", "dev")
		tip := mustGit(t, other, "rev-parse", "HEAD")
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, sha, err := ResolveBase(ctx, project, "origin/dev", lockDir); err != nil || sha != tip {
					errs <- fmt.Errorf("round %d: %s %v (tip %s)", round, sha, err, tip)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	}
	root := t.TempDir()
	const starts = 8
	var wg sync.WaitGroup
	errs := make(chan error, starts+1)
	wg.Add(1)
	go func() { // the remote moves on meanwhile
		defer wg.Done()
		for i := range 6 {
			if _, err := git(ctx, other, "commit", "-q", "--allow-empty", "-m", fmt.Sprint("more ", i)); err != nil {
				errs <- err
				return
			}
			if _, err := git(ctx, other, "push", "-q", "origin", "dev"); err != nil {
				errs <- err
				return
			}
		}
	}()
	shas := make([]string, starts)
	for i := range starts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("c-%d", i)
			r, err := Prepare(ctx, Spec{Mode: Worktree, ProjectPath: project, Path: filepath.Join(root, id),
				Branch: "feature/" + id, Base: "origin/dev", LockDir: lockDir})
			if err != nil {
				errs <- fmt.Errorf("start %s: %w", id, err)
				return
			}
			shas[i] = r.BaseSHA
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i, sha := range shas {
		if sha == "" {
			continue
		}
		if _, err := git(ctx, bare, "merge-base", "--is-ancestor", sha, "dev"); err != nil {
			t.Errorf("start %d based on %s, not a commit of dev", i, sha)
		}
	}
}

// The lock is exclusive across holders and released by its release function.
func TestLockRepo(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	dir := filepath.Join(t.TempDir(), "locks")
	unlock, err := lockRepo(ctx, dir, repo)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		u, err := lockRepo(ctx, dir, repo)
		if err == nil {
			u()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("second holder got the lock while the first held it: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released")
	}
	cctx, cancel := context.WithCancel(ctx)
	unlock, _ = lockRepo(ctx, dir, repo)
	defer unlock()
	cancel()
	if _, err := lockRepo(cctx, dir, repo); err == nil {
		t.Fatal("a cancelled wait got the lock")
	}
}
