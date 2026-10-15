package guard

import (
	"strings"
	"testing"
)

// A detached review worktree has no branch of its own: at both levels every ref move
// and every push is refused, normal work (reading, building, a scratch commit on the
// detached HEAD) is not. A push denial points to `otag task push`.
func TestDetachedReviewWorktree(t *testing.T) {
	for _, level := range []Level{Strict, Light} {
		p := Policy{Workdir: t.TempDir(), Level: level}
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
