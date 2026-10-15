// Package guard decides whether an agent's write or shell command is allowed: writes
// must stay inside the workspace and off the project's deny globs, and commands must not
// push or write outside it. Adapters call it from ParseEvent (PreToolUse and the like)
// and turn a denial into the harness's own decision output.
//
// It is a best-effort filter over what the harness reports, not a sandbox: shell
// constructs it cannot see into ($(...), eval, scripts) are inspected only for an
// obvious git push or ref change. The write boundary is the harness's native sandbox
// where it has one (docs/ADAPTERS.md, "Threat model"); adapters also put hard denies
// in the harness config.
package guard

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Policy is the guard of one run.
type Policy struct {
	Workdir string // writes must stay inside (symlinks resolved)
	// Deny are gitignore-style globs relative to Workdir that must not be written:
	// without "/" they match a name at any depth (".env*", "*.pem"); with "/" they are
	// anchored ("infra/prod/**"); "**" spans directories; a match on a directory covers
	// everything below it.
	Deny []string
	// AllowOutside are absolute paths (files or directories) writable outside Workdir.
	// /dev/null, /dev/stdout and /dev/stderr are always allowed.
	AllowOutside []string
	// DenyCommands are extra patterns matched against each simple command, its words
	// joined by single spaces; "*" matches any run of characters, "?" one character.
	DenyCommands []string
	// Cwd is where relative paths resolve: the shell's current directory as the harness
	// reports it (Claude's payload cwd; OpenCode's shell workdir, which may be relative to
	// Workdir). "" means Workdir. It never moves the boundary:
	// writes must still stay inside Workdir. CheckCommand follows `cd` within a line.
	Cwd string
	// Branch is the run's own branch; "" in same mode or a detached review worktree (then
	// no branch may be moved). Only it may be
	// force-moved or deleted (`git branch -f/-D/-M`, `checkout -B`).
	Branch string
	// Detached: Workdir is a detached review checkout (Branch ""): at both levels HEAD
	// must stay detached and no ref may be created, moved or deleted (detached.go).
	Detached bool
	// Level selects the rule set (level.go); "" is strict.
	Level Level

	// Child protections, set for child runs: otag's own state is not the agent's to
	// touch. DB is the database (reads, writes and sqlite3 on it and its -wal/-shm are
	// refused), TmuxSocket otag's tmux socket name (tmux on it, or plain tmux inside a
	// otag pane, is refused), OtagBin the otag binary (only child commands may run).
	DB         string
	TmuxSocket string
	OtagBin    string

	cwdUnknown bool        // a `cd` went somewhere the guard cannot resolve
	links      [][2]string // symlinks created earlier on the line (notePending)
	stdinFed   bool        // a wrapped script inherits a piped or redirected stdin
}

// Decision is the outcome of a check.
type Decision struct {
	Allow  bool
	Reason string // why it was denied, for the agent
}

var allow = Decision{Allow: true}

func deny(format string, args ...string) Decision {
	r := format
	for _, a := range args {
		r = strings.Replace(r, "%s", a, 1)
	}
	return Decision{Reason: "otag guard: " + r}
}

// CheckWrite decides whether the agent may create, modify or delete p (absolute, or
// relative to Workdir). Symlinks are followed, dangling ones included; a path whose
// resolution is ambiguous (a loop, an unreadable link) is refused. A file with other
// hard links is refused too: one of its other names may be outside or protected.
func (p Policy) CheckWrite(target string) Decision {
	if p.Workdir == "" {
		return allow // unknown run: no opinion
	}
	switch target {
	case "", os.DevNull, "/dev/stdout", "/dev/stderr", "/dev/tty":
		return allow
	}
	abs, d := p.absolute(target)
	if !d.Allow {
		return d
	}
	real, err := resolve(p.throughPending(abs))
	if err != nil {
		return deny("cannot tell where %s leads (%s); refusing to write through it", target, err.Error())
	}
	rel, inside := within(resolveOr(p.Workdir), real)
	if !inside {
		if p.allowedOutside(real) {
			return allow
		}
		return deny("writing outside the workspace (%s) is not allowed: %s", p.Workdir, target)
	}
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return deny("writing git metadata is not allowed: %s", target)
	}
	for _, g := range p.Deny {
		if Match(g, rel) {
			return deny("%s is protected by the project deny list (%s)", target, g)
		}
	}
	if hardLinked(real) {
		return deny("%s has other hard links, which may be outside the workspace or protected; copy it instead", target)
	}
	return allow
}

// CheckRead decides whether the agent may read target: files on the project deny list
// (secrets such as .env*) and otag's database are refused; anything else is allowed.
func (p Policy) CheckRead(target string) Decision {
	if p.Workdir == "" || target == "" {
		return allow
	}
	abs, d := p.absolute(target)
	if !d.Allow {
		return allow // reads are not the boundary; an unknown cwd is no reason to refuse
	}
	real, err := resolve(abs)
	if err != nil {
		return deny("cannot tell where %s leads (%s)", target, err.Error())
	}
	if p.otagState(real) {
		return deny("otag's database is not for agents; use otag commands (otag task me, otag done)")
	}
	rel, inside := within(resolveOr(p.Workdir), real)
	if !inside {
		return allow
	}
	for _, g := range p.Deny {
		if Match(g, rel) {
			return deny("%s is protected by the project deny list (%s) and may hold secrets", target, g)
		}
	}
	return allow
}

// absolute makes target absolute against the shell's current directory.
func (p Policy) absolute(target string) (string, Decision) {
	if filepath.IsAbs(target) {
		return target, allow
	}
	if p.cwdUnknown {
		return "", deny("cannot tell where %s is after a cd to an unknown directory; use an absolute path", target)
	}
	return join(p.base(), target), allow
}

// otagState reports whether real (resolved) is otag's database or one of its files.
func (p Policy) otagState(real string) bool {
	if p.DB == "" {
		return false
	}
	db := resolveOr(p.DB)
	for _, s := range []string{"", "-wal", "-shm", "-journal"} {
		if real == db+s {
			return true
		}
	}
	return false
}

// hardLinked reports whether p is a regular file with more than one link.
func hardLinked(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}

// base is the directory relative paths resolve against.
// A relative Cwd (OpenCode's shell workdir argument) is relative to Workdir.
func (p Policy) base() string {
	switch {
	case p.Cwd == "":
		return p.Workdir
	case filepath.IsAbs(p.Cwd):
		return p.Cwd
	}
	return join(p.Workdir, p.Cwd)
}

// join appends rel to dir without cleaning: "link/.." must reach resolve intact.
func join(dir, rel string) string {
	return strings.TrimSuffix(dir, "/") + "/" + rel
}

// Inside reports whether dir (absolute, or relative to Workdir), symlinks resolved,
// is Workdir or below it.
func (p Policy) Inside(dir string) bool {
	if !filepath.IsAbs(dir) {
		dir = join(p.Workdir, dir)
	}
	real, err := resolve(dir)
	if err != nil {
		return false
	}
	_, ok := within(resolveOr(p.Workdir), real)
	return ok
}

// chdir follows a `cd`/`pushd` in a command line.
func (p *Policy) chdir(args []string) {
	ops := operands(args)
	dir := ""
	if len(ops) > 0 {
		dir = ops[0]
	}
	home, herr := os.UserHomeDir()
	switch {
	case dir == "" || dir == "~" || strings.HasPrefix(dir, "~/"):
		if herr != nil {
			p.cwdUnknown = true
			return
		}
		dir = join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
	case dir == "-" || strings.ContainsAny(dir, "$`~*?["):
		p.cwdUnknown = true
		return
	}
	if !filepath.IsAbs(dir) {
		if p.cwdUnknown {
			return // relative to an unknown directory: still unknown
		}
		dir = join(p.base(), dir)
	}
	p.Cwd, p.cwdUnknown = dir, false
}

// checkShellTarget checks a path written by a shell command: ~ is expanded, and a
// path with an expansion the guard cannot evaluate ($VAR, `...`) is refused. A light
// run checks only deny-glob files and otag's database (checkLightTarget).
func (p Policy) checkShellTarget(t string) Decision {
	if p.Level.IsLight() {
		return p.checkLightTarget(t, false)
	}
	if strings.ContainsAny(t, "$`") {
		return deny("cannot check a write to %s (shell expansion); use a plain path", t)
	}
	t, ok := expandHome(t)
	if !ok {
		return deny("cannot resolve %s", t)
	}
	return p.CheckWrite(t)
}

// within reports whether p is root or below it, with the relative path.
func within(root, p string) (string, bool) {
	if p == root {
		return ".", true
	}
	if strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") {
		return strings.TrimPrefix(p, strings.TrimSuffix(root, "/")+"/"), true
	}
	return "", false
}

// maxLinks bounds the symlinks followed while resolving one path, as the kernel does.
const maxLinks = 40

var errLoop = errors.New("too many levels of symbolic links")

// resolve resolves every symlink on p, component by component and before any ".." is
// applied (as the kernel does: "link/../x" is the parent of the link's target), so a
// link inside the workspace pointing outside is seen as outside, including a dangling
// link (writing through it creates its target). Missing components are kept as
// written. An unreadable component or a link loop is an error: the caller cannot tell
// where the path leads. Callers must not filepath.Clean or Join a path before this.
func resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("not an absolute path")
	}
	comps := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur, links := "/", 0
	for len(comps) > 0 {
		c := comps[0]
		comps = comps[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			cur = next // not there (yet): a plain name; a later ".." comes back to real dirs
			continue
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > maxLinks {
			return "", errLoop
		}
		t, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(t) {
			cur = "/"
		}
		comps = append(strings.Split(t, "/"), comps...)
	}
	return cur, nil
}

// resolveOr is resolve falling back to the cleaned path, for otag's own paths.
func resolveOr(p string) string {
	if r, err := resolve(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Match reports whether the slash-separated relative path rel matches the
// gitignore-style glob g (see Policy.Deny).
func Match(g, rel string) bool {
	g = strings.TrimSuffix(strings.TrimPrefix(g, "./"), "/")
	if g == "" {
		return false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	if !strings.Contains(g, "/") {
		for _, s := range segs {
			if ok, _ := path.Match(g, s); ok {
				return true
			}
		}
		return false
	}
	return matchSegs(strings.Split(strings.TrimPrefix(g, "/"), "/"), segs)
}

func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return true // a matched directory covers its contents
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegs(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if ok, _ := path.Match(pat[0], segs[0]); !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}

// CheckCommand decides whether a shell command line may run: no git push, no write
// (redirection, tee, rm, mv, cp, touch, ...) outside the workspace or on a deny glob,
// no DenyCommands pattern. `sh -c`/`bash -c` scripts are checked recursively; a shell
// fed its commands through a pipe or input redirect is refused. A light run refuses
// only git pushes and ref/worktree changes, `rm -r` outside the workspace and writes
// of deny-glob files; what it cannot inspect it allows.
func (p Policy) CheckCommand(cmd string) Decision {
	if strings.Contains(cmd, "*** Begin Patch") {
		if d := p.CheckPatch(cmd); !d.Allow {
			return d
		}
	}
	return p.checkCommand(cmd, 0)
}

func (p Policy) checkCommand(cmd string, depth int) Decision {
	if depth > 8 {
		return deny("shell wrapper nesting is too deep to inspect")
	}
	// $(...), backticks and variables hide commands from the parser; refuse the
	// obvious dangerous git ones written inside them.
	if dangerousGit(cmd, p.Level) {
		return deny("a git push or ref change inside $(...), backticks, eval or a variable cannot be inspected; run it as a plain command")
	}
	if p.Detached {
		if d := p.checkSubstitutions(cmd, depth); !d.Allow {
			return d
		}
	}
	// The shell's directory before each open subshell, restored when it closes.
	type dir struct {
		cwd     string
		unknown bool
	}
	var scopes []dir
	var undo func() // ends an `env -C` directory after its command
	for _, sc := range parse(cmd) {
		if undo != nil {
			undo()
			undo = nil
		}
		switch sc.scope {
		case 1:
			scopes = append(scopes, dir{p.Cwd, p.cwdUnknown})
			continue
		case -1:
			if n := len(scopes); n > 0 {
				p.Cwd, p.cwdUnknown = scopes[n-1].cwd, scopes[n-1].unknown
				scopes = scopes[:n-1]
			}
			continue
		}
		sc.piped = sc.piped || p.stdinFed
		inner := p
		inner.stdinFed = sc.piped || sc.stdin
		for _, r := range sc.redirects {
			if d := p.checkShellTarget(r); !d.Allow {
				return d
			}
		}
		for _, r := range sc.inputs {
			if d := p.checkShellRead(r); !d.Allow {
				return d
			}
		}
		if d := p.checkOtagState(sc.words); !d.Allow {
			return d
		}
		if d := p.checkOtagEnv(sc.words); !d.Allow {
			return d
		}
		if d := p.checkGitEnv(sc.words); !d.Allow {
			return d
		}
		words, split, chdir, d := unwrap(sc.words, p.Level)
		if !d.Allow {
			return d
		}
		if chdir != "" {
			// A light run follows `env -C dir cmd` for that command only.
			c, u := p.Cwd, p.cwdUnknown
			p.chdir([]string{chdir})
			inner.Cwd, inner.cwdUnknown = p.Cwd, p.cwdUnknown
			undo = func() { p.Cwd, p.cwdUnknown = c, u }
		}
		if split != nil {
			// env -S splits its string into words and appends the remaining arguments.
			if d := inner.checkCommand(*split, depth+1); !d.Allow {
				return d
			}
			continue
		}
		if len(words) == 0 {
			continue
		}
		line := strings.Join(words, " ")
		for _, pat := range p.DenyCommands {
			if globText(pat, line) {
				return deny("command matches the deny pattern %s: %s", pat, line)
			}
		}
		name := filepath.Base(words[0])
		args := words[1:]
		switch name {
		case "cd", "pushd":
			p.chdir(args)
		case "git", "git-push", "git-send-pack", "git-http-push", "git-update-ref":
			if d := p.checkGit(name, args); !d.Allow {
				return d
			}
		case "eval":
			if d := inner.checkCommand(strings.Join(args, " "), depth+1); !d.Allow {
				return d
			}
		case "tmux":
			if d := p.checkTmux(args); !d.Allow {
				return d
			}
		case "sed", "gsed":
			if d := p.checkSed(args); !d.Allow {
				return d
			}
		case "perl":
			if d := p.checkPerl(args); !d.Allow {
				return d
			}
		case "dd":
			for _, a := range args {
				if of, ok := strings.CutPrefix(a, "of="); ok {
					if d := p.checkShellTarget(of); !d.Allow {
						return d
					}
				}
			}
		case "curl", "wget":
			if d := p.checkDownload(name, args); !d.Allow {
				return d
			}
		case "sh", "bash", "zsh", "dash", "ksh":
			// Without -c or a script operand (or with -s) the shell runs what it
			// reads from stdin, which the guard cannot see when piped or redirected.
			command, fromStdin := false, true
			for i := 0; i < len(args); i++ {
				a := args[i]
				if a == "-o" || a == "+o" || a == "-O" || a == "+O" || a == "--rcfile" || a == "--init-file" {
					i++
					continue
				}
				if a == "--" || a == "-" || !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
					fromStdin = a == "--" || a == "-" // no operand follows the options
					if fromStdin && i+1 < len(args) {
						fromStdin = false
					}
					break
				}
				if strings.HasPrefix(a, "--") {
					continue
				}
				if strings.Contains(a, "c") {
					if i+1 == len(args) {
						if p.Level.IsLight() {
							break
						}
						// xargs/find supply the script from their input.
						return deny("a %s -c script supplied by another command cannot be inspected", name)
					}
					if d := inner.checkCommand(args[i+1], depth+1); !d.Allow {
						return d
					}
					command = true
					break
				}
				if strings.Contains(a, "s") {
					fromStdin = true
					break
				}
			}
			if !command && fromStdin && (sc.piped || sc.stdin) && !p.Level.IsLight() {
				return deny("a shell reading commands from a pipe or redirect cannot be inspected; use %s -c", name)
			}
		case "xargs":
			// GNU and BSD options that take a separate value; optional values
			// (-e, -i, -l, --eof, --replace, --max-lines) must be attached.
			args = skipOptions(args, "adEIJLnPRSs", "--arg-file", "--delimiter", "--max-args", "--max-procs", "--max-chars", "--process-slot-var")
			q := p
			q.stdinFed = false // xargs consumes its input; children read /dev/null
			if d := q.checkCommand(joinWords(args), depth+1); !d.Allow {
				return d
			}
		case "find":
			for i, a := range args {
				if a != "-exec" && a != "-execdir" && a != "-ok" && a != "-okdir" {
					continue
				}
				j := i + 1
				for j < len(args) && args[j] != ";" && args[j] != "+" {
					j++
				}
				q := inner // -exec commands inherit find's stdin
				if strings.HasSuffix(a, "dir") {
					q.cwdUnknown = true
				}
				if d := q.checkCommand(joinWords(args[i+1:j]), depth+1); !d.Allow {
					return d
				}
			}
		case "tee", "rm", "rmdir", "unlink", "mv", "cp", "touch", "mkdir", "chmod", "truncate", "install":
			removes := p.Level.IsLight() && name == "rm" && recursive(args)
			for _, a := range append(operands(args), targetDirs(args)...) {
				d := p.checkShellTarget(a)
				if removes {
					d = p.checkLightTarget(a, true)
				}
				if !d.Allow {
					return d
				}
			}
		case "ln":
			if d := p.checkLink(args); !d.Allow {
				return d
			}
			p.notePending(args)
		default:
			if d := p.checkReader(name, args); !d.Allow {
				return d
			}
		}
		if p.isOtag(words[0]) {
			if d := checkOtag(args); !d.Allow {
				return d
			}
		}
	}
	return allow
}

// CheckPatch inspects all apply_patch destinations, including indented headers
// embedded in shell heredocs. Shell callers check it before tokenization.
func (p Policy) CheckPatch(patch string) Decision {
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:"} {
			if target, ok := strings.CutPrefix(line, prefix); ok {
				target = strings.TrimSpace(target)
				d := p.checkShellTarget(target)
				if p.Level.IsLight() {
					// A patch is a file-tool write: outside the workspace is refused too.
					target, _ = expandHome(target)
					d = p.CheckWrite(target)
				}
				if !d.Allow {
					return d
				}
			}

		}
	}
	return allow
}

func joinWords(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = "'" + strings.ReplaceAll(w, "'", "'\"'\"'") + "'"
	}
	return strings.Join(out, " ")
}

// skipOptions drops leading options. Short options listed in values take a value
// (attached, "-n1", or the next word, also at the end of a cluster "-tn 1"); long
// options in longValues take the next word unless written --opt=value.
func skipOptions(w []string, values string, longValues ...string) []string {
	for len(w) > 0 && strings.HasPrefix(w[0], "-") && w[0] != "-" {
		a := w[0]
		w = w[1:]
		switch {
		case a == "--":
			return w
		case strings.HasPrefix(a, "--"):
			for _, l := range longValues {
				if a == l && len(w) > 0 {
					w = w[1:]
				}
			}
		default:
			for i := 1; i < len(a); i++ {
				if strings.IndexByte(values, a[i]) >= 0 {
					if i == len(a)-1 && len(w) > 0 {
						w = w[1:]
					}
					break
				}
			}
		}
	}
	return w
}

// unwrap drops env assignments, shell reserved words and transparent wrappers in
// front of the command word. A script for `env -S` is returned for a recursive
// check (with the remaining words appended); `env -C` is refused rather than
// silently resolving destinations in the wrong directory, except in a light run,
// which gets the directory back (chdir) and resolves the command there.
func unwrap(w []string, level Level) (words []string, split *string, chdir string, d Decision) {
	for len(w) > 0 {
		name := filepath.Base(w[0])
		switch {
		case strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "=") && !strings.HasPrefix(w[0], "-"):
			w = w[1:]
		case name == "env":
			w = w[1:]
		envOptions:
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				a := w[0]
				w = w[1:]
				long, value, attached := strings.Cut(a, "=")
				switch {
				case a == "--":
					break envOptions
				case a == "-":
					// same as -i
				case long == "--chdir":
					if !level.IsLight() {
						return nil, nil, "", deny("env cwd switches cannot be inspected; use cd")
					}
					if !attached && len(w) > 0 {
						value, w = w[0], w[1:]
					}
					chdir = value
				case long == "--split-string":
					if !attached {
						if len(w) == 0 {
							return nil, nil, chdir, allow
						}
						value, w = w[0], w[1:]
					}
					script := value + " " + joinWords(w)
					return nil, &script, chdir, allow
				case strings.HasPrefix(a, "--"):
					if (long == "--unset" || long == "--argv0") && !attached && len(w) > 0 {
						w = w[1:]
					}
				default:
					for i := 1; i < len(a); i++ {
						switch a[i] {
						case 'C':
							if !level.IsLight() {
								return nil, nil, "", deny("env cwd switches cannot be inspected; use cd")
							}
							chdir = a[i+1:]
							if chdir == "" && len(w) > 0 {
								chdir, w = w[0], w[1:]
							}
						case 'S':
							script := a[i+1:]
							if script == "" {
								if len(w) == 0 {
									return nil, nil, chdir, allow
								}
								script, w = w[0], w[1:]
							}
							script += " " + joinWords(w)
							return nil, &script, chdir, allow
						case 'u', 'P', 'a':
							if i == len(a)-1 && len(w) > 0 {
								w = w[1:]
							}
						default:
							continue
						}
						break
					}
				}
			}
		case name == "exec":
			w = skipOptions(w[1:], "a")
		case name == "sudo":
			w = skipOptions(w[1:], "CDghprRtTUu", "--chdir", "--close-from", "--group", "--host", "--prompt", "--chroot", "--role", "--type", "--command-timeout", "--other-user", "--user")
		case name == "time":
			w = skipOptions(w[1:], "fo", "--format", "--output")
		case name == "nice":
			w = skipOptions(w[1:], "n", "--adjustment")
		case name == "stdbuf":
			w = skipOptions(w[1:], "ioe", "--input", "--output", "--error")
		case name == "timeout":
			w = skipOptions(w[1:], "sk", "--signal", "--kill-after")
			if len(w) > 0 {
				w = w[1:] // the duration
			}
		case name == "command" || name == "builtin" || name == "nohup" || w[0] == "!" || w[0] == "{" ||
			w[0] == "if" || w[0] == "then" || w[0] == "elif" || w[0] == "else" || w[0] == "do" || w[0] == "while" || w[0] == "until":
			w = skipOptions(w[1:], "")
		default:
			return w, nil, chdir, allow
		}
	}
	return w, nil, chdir, allow
}

func operands(args []string) []string {
	var out []string
	opts := true
	for _, a := range args {
		if opts && a == "--" {
			opts = false
			continue
		}
		if opts && strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// globText matches s against pattern where '*' matches any run and '?' one character.
func globText(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if globText(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && globText(pattern[1:], s[1:])
	}
	return s != "" && s[0] == pattern[0] && globText(pattern[1:], s[1:])
}

type simpleCommand struct {
	words     []string
	redirects []string // output redirection targets
	inputs    []string // input redirection sources (< file), not here-documents
	piped     bool     // stdin is the previous command's output (cmd | this)
	stdin     bool     // stdin is redirected (< file, <<EOF, <<< string)
	// scope marks a "(" (+1) or ")" (-1) with no command: a subshell, $(...) or
	// <(...) starts or ends, and a `cd` inside it does not move the commands after it.
	scope int
}

// parse splits a command line into simple commands (on ; & | && || ( ) and
// newlines), honouring quotes and backslashes, and collects output redirection
// targets. Input redirection sources and here-document delimiters are not words.
// Parentheses also yield scope markers (simpleCommand.scope).
func parse(line string) []simpleCommand {
	var cmds []simpleCommand
	var cur simpleCommand
	var word strings.Builder
	inWord, redirect, input, inputFile := false, false, false, false
	flushWord := func() {
		if !inWord {
			return
		}
		w := word.String()
		word.Reset()
		inWord = false
		if input {
			input = false
			if inputFile {
				cur.inputs = append(cur.inputs, w)
			}
			return
		}
		if redirect {
			redirect = false
			if !strings.HasPrefix(w, "&") { // >&2
				cur.redirects = append(cur.redirects, w)
			}
			return
		}
		cur.words = append(cur.words, w)
	}
	flushCmd := func() {
		flushWord()
		if len(cur.words) > 0 || len(cur.redirects) > 0 {
			cmds = append(cmds, cur)
		}
		cur = simpleCommand{}
		redirect, input = false, false // <(...) and >(...) start a command
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			i++
			word.WriteRune(rs[i])
			inWord = true
		case c == '\'':
			inWord = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				word.WriteRune(rs[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				word.WriteRune(rs[i])
			}
		case c == ' ' || c == '\t':
			flushWord()
		case c == '|':
			pipe := true
			if i+1 < len(rs) && rs[i+1] == '|' { // ||
				i++
				pipe = false
			} else if i+1 < len(rs) && rs[i+1] == '&' { // |&
				i++
			}
			flushCmd()
			cur.piped = pipe
		case c == ';' || c == '\n' || c == '&' || c == '(' || c == ')':
			if c == '&' && i+1 < len(rs) && rs[i+1] == '>' { // &> file
				flushWord()
				redirect = true
				i++
				if i+1 < len(rs) && rs[i+1] == '>' {
					i++
				}
				continue
			}
			flushCmd()
			switch c {
			case '(':
				cmds = append(cmds, simpleCommand{scope: 1})
			case ')':
				cmds = append(cmds, simpleCommand{scope: -1})
			}
		case c == '<':
			// "0<" or "<<-": the fd number and operator are not words.
			if inWord {
				if w := word.String(); strings.Trim(w, "0123456789") == "" {
					word.Reset()
					inWord = false
				}
			}
			flushWord()
			inputFile = true // "< file"; not "<<EOF", "<<< str" or "<&3"
			for i+1 < len(rs) && (rs[i+1] == '<' || rs[i+1] == '-' || rs[i+1] == '&') {
				i++
				inputFile = false
			}
			cur.stdin, input = true, true
		case c == '>':
			// "2>" or "1>": the fd number is not a word.
			if inWord {
				if w := word.String(); strings.Trim(w, "0123456789") == "" {
					word.Reset()
					inWord = false
				}
			}
			flushWord()
			if i+1 < len(rs) && (rs[i+1] == '>' || rs[i+1] == '|') {
				i++
			}
			redirect = true
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	flushCmd()
	return cmds
}
