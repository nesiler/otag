package guard

import (
	"os"
	"slices"
	"strings"
)

// Level is how much the guard enforces, the project setting `guard` (docs/ADAPTERS.md,
// "Threat model"). The run records the level it started with.
//
//   - Light (the project default) keeps a short list of rules and allows everything
//     else: no git push, no git change to other refs, other worktrees or the main
//     checkout, no file-tool write and no `rm -r` outside the workspace (temporary
//     directories excepted), no read or write of deny-glob files. A command the guard
//     cannot inspect is allowed. Harnesses run without their native sandbox.
//   - Strict is the full guard behind the harness's native sandbox.
//
// The zero Level is strict: runs from before levels, and policies built by hand.
type Level string

// Levels.
const (
	Light  Level = "light"
	Strict Level = "strict"
)

// Levels lists the valid level names; DefaultLevel is a project's level when it sets none.
var (
	Levels       = []string{string(Light), string(Strict)}
	DefaultLevel = Light
)

// IsLight reports whether l is the light level ("" is strict).
func (l Level) IsLight() bool { return l == Light }

// String returns the level name, "strict" for the zero Level.
func (l Level) String() string {
	if l == "" {
		return string(Strict)
	}
	return string(l)
}

// DefaultDeny are the deny globs a light run adds to the project's: secrets.
var DefaultDeny = []string{".env*"}

// DenyGlobs returns the deny globs of a run at level l: the project's, plus
// DefaultDeny for a light run.
func DenyGlobs(l Level, project []string) []string {
	out := slices.Clone(project)
	if l.IsLight() {
		for _, g := range DefaultDeny {
			if !slices.Contains(out, g) {
				out = append(out, g)
			}
		}
	}
	return out
}

// tempDirs are the temporary directories a light run may write and remove anywhere in
// (a variable for tests, whose own directories are temporary).
var tempDirs = func() []string {
	return []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
}

// inTemp reports whether real (resolved) is in a temporary directory.
func inTemp(real string) bool {
	for _, t := range tempDirs() {
		if _, ok := within(resolveOr(t), real); ok && strings.TrimSpace(t) != "" {
			return true
		}
	}
	return false
}

// allowedOutside reports whether real (resolved, outside Workdir) is writable anyway:
// an AllowOutside entry, or for a light run a temporary directory.
func (p Policy) allowedOutside(real string) bool {
	for _, a := range p.AllowOutside {
		if _, ok := within(resolveOr(a), real); ok {
			return true
		}
	}
	return p.Level.IsLight() && inTemp(real)
}

// checkLightTarget is a light run's check of a path a shell command writes: only
// deny-glob files in the workspace and otag's database are refused; a path the guard
// cannot resolve is allowed. With outside set (rm -r), a target outside the workspace
// and the temporary directories is refused too; its last component is not resolved,
// since rm removes a symlink itself, not what it points to.
func (p Policy) checkLightTarget(t string, outside bool) Decision {
	if strings.ContainsAny(t, "$`") {
		return allow
	}
	t, ok := expandHome(t)
	if !ok {
		return allow
	}
	abs, d := p.absolute(t)
	if !d.Allow {
		return allow
	}
	real, err := resolve(p.throughPending(abs))
	if i := strings.LastIndex(abs, "/"); outside && i >= 0 {
		if base := abs[i+1:]; base != "" && base != "." && base != ".." {
			var parent string
			if parent, err = resolve(p.throughPending(abs[:i] + "/")); err == nil {
				real = join(parent, base)
			}
		}
	}
	if err != nil {
		return allow
	}
	if p.otagState(real) {
		return deny(dbDenied)
	}
	rel, inside := within(resolveOr(p.Workdir), real)
	if !inside {
		if outside && !p.allowedOutside(real) {
			return deny("removing outside the workspace (%s) is not allowed: %s", p.Workdir, t)
		}
		return allow
	}
	for _, g := range p.Deny {
		if Match(g, rel) {
			return deny("%s is protected by the project deny list (%s)", t, g)
		}
	}
	return allow
}

// recursive reports whether rm args ask for a recursive removal.
func recursive(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--recursive" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR") {
			return true
		}
	}
	return false
}

// worktreeGit are git subcommands that move a checkout's HEAD, branch, index or files:
// a light run may run them only in its own workspace.
var worktreeGit = []string{"reset", "checkout", "switch", "restore", "clean", "merge", "rebase", "commit",
	"cherry-pick", "revert", "pull", "am", "stash", "rm", "mv", "add", "apply"}
