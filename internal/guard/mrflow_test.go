package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A detached review worktree has no branch of its own: at both levels HEAD stays
// detached (no switch or checkout onto a branch, no new branch) and no ref is created,
// moved or deleted; normal work (reading, building, restoring files, a scratch commit
// or reset on the detached HEAD, detaching at another commit) is not refused. A push
// denial points to `otag task push`. A normal run keeps its own rules.
func TestDetachedReviewWorktree(t *testing.T) {
	for _, level := range []Level{Strict, Light} {
		wd := t.TempDir()
		if err := os.WriteFile(filepath.Join(wd, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p := Policy{Workdir: wd, Level: level, Detached: true}
		for _, cmd := range []string{"git switch other", "git switch -c x", "git switch -C x", "git switch --orphan x",
			"git switch -d -c x HEAD", "git checkout other", "git checkout -", "git checkout -b x", "git checkout -B x HEAD",
			"git checkout --orphan x", "git checkout -fb x", "git branch x", "git branch x HEAD", "git tag v1",
			"git tag -a v1 -m x", "git tag -d v1", "git symbolic-ref HEAD refs/heads/other", "git symbolic-ref -d HEAD",
			"git stash branch x", "git worktree add ../w", "git worktree add -b x ../w", "git rebase main other",
			"git rebase --onto main base other", "git fetch . HEAD:refs/heads/x", "git fetch origin +dev:dev",
			"git pull origin dev:dev", "git notes add -m x", "git replace HEAD abc1234", `sh -c "git switch $B"`,
			"git -C . switch other", "cd sub && git checkout other"} {
			if d := p.CheckCommand(cmd); d.Allow {
				t.Errorf("%s detached: allowed %q", level, cmd)
			} else if strings.Contains(cmd, "switch other") && !strings.Contains(d.Reason, "detached review checkout") {
				t.Errorf("%s: reason %q", level, d.Reason)
			}
		}
		for _, cmd := range []string{"git switch --detach abc1234", "git switch -d HEAD~1", "git switch --detach origin/dev",
			"git checkout --detach", "git checkout abcdef1", "git checkout HEAD~2", "git checkout -- main.go",
			"git checkout HEAD -- main.go", "git checkout main.go", "git restore main.go", "git branch", "git branch -a",
			"git branch --show-current", "git branch --contains HEAD", "git tag", "git tag -l", "git tag -l 'v*'",
			"git reset --hard HEAD~1", "git commit -m scratch", "git rebase HEAD~2", "git fetch origin",
			"git fetch https://example.com/x.git", "git symbolic-ref HEAD", "git stash", "git stash pop",
			"git worktree list", "git notes list", "git log --oneline", "git diff abc...def", "go test ./..."} {
			if d := p.CheckCommand(cmd); !d.Allow {
				t.Errorf("%s detached: denied %q: %s", level, cmd, d.Reason)
			}
		}
		// A normal run on its own branch keeps today's rules: switching branches is the
		// worktree's own business there.
		normal := Policy{Workdir: wd, Level: level, Branch: "feature/x-1"}
		for _, cmd := range []string{"git switch -c scratch", "git checkout -- main.go"} {
			if d := normal.CheckCommand(cmd); !d.Allow {
				t.Errorf("%s normal: denied %q: %s", level, cmd, d.Reason)
			}
		}
	}
	for _, level := range []Level{Strict, Light} {
		p := Policy{Workdir: t.TempDir(), Level: level, Detached: true}
		for _, cmd := range []string{"git push origin HEAD", "git push --force", "git branch -f main HEAD",
			"git branch -D feature/x-1", "git branch -m feature/x-1 y", "git checkout -B feature/x-1",
			"git switch -C main", "git update-ref refs/heads/main HEAD", "git send-pack origin"} {
			if d := p.CheckCommand(cmd); d.Allow {
				t.Errorf("%s: allowed %q", level, cmd)
			}
		}
		for _, cmd := range []string{"git log --oneline", "git diff abc...def", "git show HEAD", "go test ./...",
			"git commit -m scratch", "git switch --detach HEAD~1", "git stash list"} {
			if d := p.CheckCommand(cmd); !d.Allow {
				t.Errorf("%s: denied %q: %s", level, cmd, d.Reason)
			}
		}
		if d := p.CheckCommand("git push"); !strings.Contains(d.Reason, "otag task push") {
			t.Errorf("%s: push reason %q", level, d.Reason)
		}
	}
	// The child commands include task push (the CLI applies the project's push
	// setting); reopen and deliver stay coordinator commands.
	if !IsChildCommand("task", "push") || IsChildCommand("task", "reopen") || IsChildCommand("task", "deliver") {
		t.Fatal("child command list")
	}
	p := Policy{Workdir: t.TempDir(), OtagBin: "/usr/local/bin/otag"}
	if d := p.CheckCommand("otag task push al-1"); !d.Allow {
		t.Fatalf("child otag task push: %s", d.Reason)
	}
	if d := p.CheckCommand("otag task reopen al-1 x"); d.Allow {
		t.Fatal("child otag task reopen allowed")
	}
}
