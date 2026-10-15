package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// detachedWorktree makes a repository with a branch "docs" (also a directory), a
// remote-tracking branch "origin/feat" and a file main.go, and returns a detached
// linked worktree of it.
func detachedWorktree(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	wt := filepath.Join(t.TempDir(), "wt")
	run := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "docs", "a.md"), []byte("a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "feat"), []byte("a file named like a remote branch\n"), 0o644)
	run(repo, "init", "-q", "-b", "main")
	run(repo, "add", ".")
	run(repo, "-c", "user.name=t", "-c", "user.email=t@e", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	run(repo, "branch", "docs")
	run(repo, "update-ref", "refs/remotes/origin/feat", "HEAD")
	run(repo, "worktree", "add", "-q", "--detach", wt, "HEAD")
	return wt
}

// Round 2: a path that is also a branch (local, or remote that git would turn into a
// local one) is a branch switch without "--", also with packed refs; operands after
// "--" count; variables in read-only operands are fine, in ones that decide a ref
// write they are not; quoted command substitutions and backticks are checked like
// unquoted ones.
func TestDetachedReviewWorktreeRound2(t *testing.T) {
	wd := detachedWorktree(t)
	p := Policy{Workdir: wd, Level: Light, Detached: true}
	deny := []string{"git checkout docs", "git checkout feat", "git symbolic-ref -- HEAD refs/heads/other",
		"git branch -- scratch", "git tag -- v1", `git switch "$B"`, `git checkout "$X"`, `git fetch origin "$SPEC"`,
		`git tag "$T"`, `git branch "$N"`, `git rebase "$UP"`, `git stash "$OP"`, `echo "$(git switch other)"`,
		"echo \"`git switch other`\"", `x="$(git branch scratch)"`, `echo "$(echo "$(git checkout docs)")"`,
		"echo `git symbolic-ref HEAD refs/heads/x`"}
	allow := []string{"git checkout -- docs", "git checkout main.go", "git checkout HEAD -- feat", "git restore docs",
		`git branch --contains "$SHA"`, `git stash show "$STASH"`, `git log "$X"`, `git diff "$A...$B"`,
		`git show "$SHA":main.go`, `echo "$(git branch --show-current)"`, `echo "$(git rev-parse HEAD)"`,
		"echo `git log -1 --format=%H`"}
	check := func() {
		t.Helper()
		for _, cmd := range deny {
			if d := p.CheckCommand(cmd); d.Allow {
				t.Errorf("allowed %q", cmd)
			}
		}
		for _, cmd := range allow {
			if d := p.CheckCommand(cmd); !d.Allow {
				t.Errorf("denied %q: %s", cmd, d.Reason)
			}
		}
	}
	check()
	// The same with every ref packed into packed-refs.
	if out, err := exec.Command("git", "-C", wd, "pack-refs", "--all", "--prune").CombinedOutput(); err != nil {
		t.Fatalf("pack-refs: %v %s", err, out)
	}
	check()
	// Round 3: single quotes and escapes make substitutions text; double quotes do not.
	for _, cmd := range []string{"git commit -m 'Document `rm -rf /var/cache/otag` cleanup'",
		`git commit -m 'run $(git switch other) by hand'`, "git commit -m \"see \\`git switch x\\`\"",
		`echo "\$(git switch other)"`, `git commit -m "it's \$(git branch x)"`} {
		if d := p.CheckCommand(cmd); !d.Allow {
			t.Errorf("denied %q: %s", cmd, d.Reason)
		}
	}
	for _, cmd := range []string{"git commit -m \"Document `git switch other`\"", `echo 'x' "$(git branch y)"`,
		`echo "it's $(git switch other)"`, "echo '`a`'`git branch z`"} {
		if d := p.CheckCommand(cmd); d.Allow {
			t.Errorf("allowed %q", cmd)
		}
	}
	// A directory of nested branches is not a branch: with docs/update and no branch
	// docs, `git checkout docs` restores the directory docs; with the remote branch
	// origin/feat/x, `git checkout feat` likewise (feat is a file here).
	if out, err := exec.Command("git", "-C", wd, "branch", "-D", "docs").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, args := range [][]string{{"branch", "docs/update"}, {"update-ref", "-d", "refs/remotes/origin/feat"},
		{"update-ref", "refs/remotes/origin/feat/x", "HEAD"}} {
		if out, err := exec.Command("git", append([]string{"-C", wd}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	for _, cmd := range []string{"git checkout docs", "git checkout feat"} {
		if d := p.CheckCommand(cmd); !d.Allow {
			t.Errorf("loose refs: denied %q: %s", cmd, d.Reason)
		}
	}
	if d := p.CheckCommand("git checkout docs/update"); d.Allow {
		t.Error("checkout of the nested branch allowed")
	}
	if out, err := exec.Command("git", "-C", wd, "pack-refs", "--all", "--prune").CombinedOutput(); err != nil {
		t.Fatalf("pack-refs: %v %s", err, out)
	}
	for _, cmd := range []string{"git checkout docs", "git checkout feat"} {
		if d := p.CheckCommand(cmd); !d.Allow {
			t.Errorf("packed refs: denied %q: %s", cmd, d.Reason)
		}
	}

	// Refs that cannot be read: a path operand without "--" is refused, with "--" not.
	unknown := Policy{Workdir: t.TempDir(), Level: Light, Detached: true}
	_ = os.WriteFile(filepath.Join(unknown.Workdir, "main.go"), []byte("x"), 0o644)
	if d := unknown.CheckCommand("git checkout main.go"); d.Allow {
		t.Error("a path checkout with unreadable refs allowed")
	}
	if d := unknown.CheckCommand("git checkout -- main.go"); !d.Allow {
		t.Errorf("git checkout -- main.go: %s", d.Reason)
	}
}

// A detached review worktree has no branch of its own: at both levels HEAD stays
// detached (no switch or checkout onto a branch, no new branch) and no ref is created,
// moved or deleted; normal work (reading, building, restoring files, a scratch commit
// or reset on the detached HEAD, detaching at another commit) is not refused. A push
// denial points to `otag task push`. A normal run keeps its own rules.
func TestDetachedReviewWorktree(t *testing.T) {
	for _, level := range []Level{Strict, Light} {
		wd := detachedWorktree(t)
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
