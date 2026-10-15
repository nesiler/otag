package guard

import (
	"bufio"
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
// and all inspection, also with variables in its operands. Like the rest of the guard
// this protects against accidents (ADAPTERS.md "Threat model"), not against an agent
// that builds its command out of reach of the parser.

const detachedDenied = "this is a detached review checkout: HEAD stays detached and no branch, tag or ref is " +
	"created, moved or deleted (git switch --detach <commit>, git checkout -- <paths> and git restore work)"

// checkDetached applies the detached-checkout rules to `git <sub> <rest>` run in dir.
// Options are read before a "--"; operands are read on both sides of it, except for
// checkout, where what follows "--" are paths.
func (p Policy) checkDetached(sub string, rest []string, dir string) Decision {
	opts, paths := splitDashDash(rest)
	ops := operands(rest)
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
		for _, op := range operands(opts) {
			if detachedTarget(op) {
				continue
			}
			// A path that is also a branch (or a remote branch git would make one of) is
			// a branch switch without "--".
			if branch, known := p.branchNamed(op); branch || !known || !p.existsIn(dir, op) {
				return deny(detachedDenied + "; `git checkout " + op + "` could attach HEAD to a branch (restore files " +
					"with git checkout -- " + op + ")")
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
		if len(ops) > 0 && (ops[0] == "branch" || expands(ops[0])) {
			return deny(detachedDenied)
		}
	case "worktree":
		if len(ops) > 0 && (ops[0] == "add" || expands(ops[0])) {
			return deny(detachedDenied + "; a new worktree would get a branch")
		}
	case "rebase":
		n := len(ops)
		if hasFlag(opts, "--onto") && !slices.ContainsFunc(opts, func(a string) bool { return strings.HasPrefix(a, "--onto=") }) {
			n-- // its value
		}
		if n > 1 || slices.ContainsFunc(ops, expands) {
			return deny(detachedDenied + "; `git rebase <upstream> <branch>` checks out <branch>")
		}
	case "fetch", "pull":
		for _, op := range ops {
			if strings.Contains(op, ":") && !strings.Contains(op, "://") || expands(op) {
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

// checkSubstitutions checks the commands inside every $(...) and `...` of line, quoted
// or not, with the full checker: the parser already splits unquoted substitutions into
// their own commands, but one inside double quotes stays part of a word. Variables and
// other indirection stay best effort.
func (p Policy) checkSubstitutions(line string, depth int) Decision {
	for _, inner := range substitutions(line) {
		if d := p.checkCommand(inner, depth+1); !d.Allow {
			return d
		}
	}
	return allow
}

// substitutions returns the bodies of the outermost $(...) and `...` in s that the
// shell expands: outside quotes and inside double quotes, not inside single quotes
// ('`rm -rf x`' is text), and not escaped (\$(, \`). An unterminated one runs to the
// end.
func substitutions(s string) []string {
	var out []string
	rs := []rune(s)
	inSingle, inDouble := false, false
	for i := 0; i < len(rs); i++ {
		switch {
		case inSingle:
			if rs[i] == '\'' {
				inSingle = false
			}
		case rs[i] == '\\':
			i++
		case rs[i] == '\'' && !inDouble:
			inSingle = true
		case rs[i] == '"':
			inDouble = !inDouble
		case rs[i] == '`':
			j := i + 1
			for j < len(rs) && rs[j] != '`' {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			out = append(out, string(rs[i+1:min(j, len(rs))]))
			i = j
		case rs[i] == '$' && i+1 < len(rs) && rs[i+1] == '(':
			depth, j := 1, i+2
			for ; j < len(rs) && depth > 0; j++ {
				switch rs[j] {
				case '\\':
					j++
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			end := j
			if depth == 0 {
				end = j - 1
			}
			out = append(out, string(rs[i+2:min(end, len(rs))]))
			i = j - 1
		}
	}
	return out
}

// expands reports whether s holds an expansion the guard cannot evaluate.
func expands(s string) bool { return strings.ContainsAny(s, "$`") }

// branchNamed reports whether name is a local branch of the repository, or a branch of a
// remote that `git checkout <name>` would turn into a new local one (known = false: the
// refs could not be read, so the answer is unknown).
func (p Policy) branchNamed(name string) (branch, known bool) {
	if expands(name) {
		return false, false
	}
	common := gitCommonDir(p.Workdir)
	if common == "" {
		return false, false
	}
	if _, err := os.Stat(filepath.Join(common, "reftable")); err == nil {
		return false, false // one binary store: not read here
	}
	// Only a ref file is a branch: refs/heads/docs/ is the directory of docs/update.
	if isFile(filepath.Join(common, "refs", "heads", filepath.FromSlash(name))) {
		return true, true
	}
	m, _ := filepath.Glob(filepath.Join(common, "refs", "remotes", "*", filepath.FromSlash(name)))
	if slices.ContainsFunc(m, isFile) {
		return true, true
	}
	f, err := os.Open(filepath.Join(common, "packed-refs"))
	if os.IsNotExist(err) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		_, ref, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if ref == "refs/heads/"+name {
			return true, true
		}
		if r, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
			if _, b, ok := strings.Cut(r, "/"); ok && b == name {
				return true, true
			}
		}
	}
	return false, sc.Err() == nil
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// gitCommonDir returns the git common directory of the checkout at workdir: its .git
// directory, or for a linked worktree the commondir its .git file leads to ("" when
// it cannot be read).
func gitCommonDir(workdir string) string {
	if workdir == "" {
		return ""
	}
	dotgit := filepath.Join(workdir, ".git")
	st, err := os.Stat(dotgit)
	if err != nil {
		return ""
	}
	if st.IsDir() {
		return dotgit
	}
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(workdir, gd)
	}
	c, err := os.ReadFile(filepath.Join(gd, "commondir"))
	if err != nil {
		return gd
	}
	common := strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gd, common)
	}
	return filepath.Clean(common)
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

// existsIn reports whether op is an existing path relative to dir.
func (p Policy) existsIn(dir, op string) bool {
	if dir == "" || expands(op) {
		return false
	}
	path := op
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, op)
	}
	_, err := os.Lstat(path)
	return err == nil
}
