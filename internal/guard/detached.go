package guard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A detached review checkout (Policy.Detached) has no branch of its own: its HEAD must
// stay detached and no ref may be created, moved or deleted, at both levels. Without
// these rules a light run (no native sandbox) could `git switch` to a branch no
// worktree has checked out and commit on it, or create one. What stays allowed:
// working-tree operations, `git checkout -- <paths>`, `git restore`, `git switch
// --detach <commit>`, `git checkout <commit>`, commits and resets of the detached HEAD,
// and all inspection.

const detachedDenied = "this is a detached review checkout: HEAD stays detached and no branch, tag or ref is " +
	"created, moved or deleted (git switch --detach <commit>, git checkout -- <paths> and git restore work)"

// checkDetached applies the detached-checkout rules to `git <sub> <rest>` run in dir.
func (p Policy) checkDetached(sub string, rest []string, dir string) Decision {
	opts, paths := splitDashDash(rest)
	ops := operands(opts)
	switch sub {
	case "switch":
		if hasFlag(opts, "--detach") || hasShort(opts, 'd') {
			if hasFlag(opts, "--create", "--force-create", "--orphan") || hasShort(opts, 'c', 'C') {
				return deny(detachedDenied)
			}
			return allow
		}
		return deny(detachedDenied)
	case "checkout":
		if hasFlag(opts, "--orphan") || hasShort(opts, 'b', 'B') {
			return deny(detachedDenied)
		}
		if hasFlag(opts, "--detach") || paths != nil {
			return allow // detaches, or only restores files (a tree-ish before -- moves no HEAD)
		}
		for _, op := range ops {
			if !detachedTarget(op) && !p.existsIn(dir, op) {
				return deny(detachedDenied + "; `git checkout " + op + "` could attach HEAD to a branch")
			}
		}
	case "branch":
		if len(ops) > 0 && !hasFlag(opts, "--list", "--all", "--remotes", "--show-current", "--contains", "--no-contains",
			"--merged", "--no-merged", "--points-at") && !hasShort(opts, 'l', 'a', 'r') {
			return deny(detachedDenied)
		}
	case "tag":
		if len(ops) > 0 && !hasFlag(opts, "--list", "--contains", "--no-contains", "--merged", "--no-merged",
			"--points-at", "--verify") && !hasShort(opts, 'l', 'v') {
			return deny(detachedDenied)
		}
	case "symbolic-ref":
		if len(ops) > 1 || hasFlag(opts, "--delete") || hasShort(opts, 'd') {
			return deny(detachedDenied)
		}
	case "stash":
		if len(ops) > 0 && ops[0] == "branch" {
			return deny(detachedDenied)
		}
	case "worktree":
		if len(ops) > 0 && ops[0] == "add" {
			return deny(detachedDenied + "; a new worktree would get a branch")
		}
	case "rebase":
		n := len(ops)
		if hasFlag(opts, "--onto") && !slices.ContainsFunc(opts, func(a string) bool { return strings.HasPrefix(a, "--onto=") }) {
			n-- // its value
		}
		if n > 1 {
			return deny(detachedDenied + "; `git rebase <upstream> <branch>` checks out <branch>")
		}
	case "fetch", "pull":
		for _, op := range ops {
			if strings.Contains(op, ":") && !strings.Contains(op, "://") {
				return deny(detachedDenied + "; a fetch refspec with a destination writes a ref")
			}
		}
	case "notes":
		if len(ops) > 0 && ops[0] != "list" && ops[0] != "show" {
			return deny(detachedDenied)
		}
	case "replace":
		if !hasFlag(opts, "--list") && !hasShort(opts, 'l') {
			return deny(detachedDenied)
		}
	}
	return allow
}

// detachedText refuses, in a detached checkout, a git command hidden in a substitution
// that could attach HEAD or write a ref (the textual fallback of dangerousGit).
func detachedText(text string) bool {
	if !dangerousSubst.MatchString(text) {
		return false
	}
	f := strings.Fields(text)
	for i, w := range f {
		if w != "git" && !strings.HasSuffix(w, "/git") {
			continue
		}
		for _, n := range f[i+1 : min(len(f), i+8)] {
			switch strings.Trim(n, `"'`) {
			case "switch", "checkout", "branch", "tag", "symbolic-ref", "worktree", "stash", "notes", "replace":
				return true
			}
		}
	}
	return false
}

// splitDashDash splits args at the first "--": the options and operands before it, and
// the paths after it (nil when there is no "--").
func splitDashDash(args []string) (before, after []string) {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[:i], append([]string{}, args[i+1:]...)
	}
	return args, nil
}

// hasFlag reports whether args have one of the long flags (alone or as --flag=value).
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

// hasShort reports whether a cluster of short options (-xyz) in args has one of the
// letters.
func hasShort(args []string, letters ...rune) bool {
	for _, a := range args {
		if len(a) < 2 || a[0] != '-' || a[1] == '-' {
			continue
		}
		for _, l := range letters {
			if strings.ContainsRune(a[1:], l) {
				return true
			}
		}
	}
	return false
}

// detachedTarget reports whether op names a commit a checkout detaches at: HEAD and
// HEAD~n/^n/@{n}, or a commit id (with ~/^ suffixes).
func detachedTarget(op string) bool {
	base := op
	if i := strings.IndexAny(op, "~^@"); i >= 0 {
		base = op[:i]
	}
	if base == "HEAD" {
		return true
	}
	if len(base) < 7 || len(base) > 64 {
		return false
	}
	for _, r := range base {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// existsIn reports whether op is an existing path relative to dir (a checkout of files,
// not of a branch).
func (p Policy) existsIn(dir, op string) bool {
	if dir == "" || strings.ContainsAny(op, "$`") {
		return false
	}
	path := op
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, op)
	}
	_, err := os.Lstat(path)
	return err == nil
}
