package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// This file holds the per-command checks of CheckCommand: git ref and config changes,
// in-place writers, readers of protected files and the child protections (otag's
// database, tmux server and coordinator commands). All are best effort; the native
// sandbox is the boundary where the harness has one.

// expandHome expands a leading ~ ("~" or "~/x").
func expandHome(t string) (string, bool) {
	if t != "~" && !strings.HasPrefix(t, "~/") {
		return t, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return join(home, strings.TrimPrefix(strings.TrimPrefix(t, "~"), "/")), true
}

// checkShellRead checks a path read by a shell command. A path with an expansion the
// guard cannot evaluate is let through: reads are not the boundary (checkOtagState
// still refuses $OTAG_DB).
func (p Policy) checkShellRead(t string) Decision {
	if strings.ContainsAny(t, "$`") {
		return allow
	}
	t, ok := expandHome(t)
	if !ok {
		return allow
	}
	return p.CheckRead(t)
}

// readers read the files named by their operands; greps take a pattern first.
var (
	readers = []string{"cat", "head", "tail", "less", "more", "bat", "nl", "base64", "xxd", "od", "hexdump",
		"strings", "source", ".", "cut", "sort", "uniq", "wc", "diff", "cmp", "jq", "tac", "column"}
	greps = []string{"grep", "egrep", "fgrep", "rg", "ag", "ack"}
)

// checkReader refuses reading a protected file with a common reader (cat .env).
func (p Policy) checkReader(name string, args []string) Decision {
	if len(p.Deny) == 0 && p.DB == "" {
		return allow
	}
	var ops []string
	switch {
	case slices.Contains(readers, name):
		ops = operands(args)
	case slices.Contains(greps, name):
		ops = operands(args)
		pattern := true // the first operand is the pattern unless -e/-f gives it
		for _, a := range args {
			if a == "-e" || a == "-f" || strings.HasPrefix(a, "--regexp") || strings.HasPrefix(a, "--file") {
				pattern = false
			}
		}
		if pattern && len(ops) > 0 {
			ops = ops[1:]
		}
	}
	for _, o := range ops {
		if d := p.checkShellRead(o); !d.Allow {
			return d
		}
	}
	return allow
}

const dbDenied = "otag's database is not for agents; use otag commands (otag task me, otag done)"

// checkOtagState refuses any word naming otag's database ($OTAG_DB, its path or its
// -wal/-shm), whatever the command (sqlite3, cp, cat, ...).
func (p Policy) checkOtagState(words []string) Decision {
	if p.DB == "" {
		return allow
	}
	base := filepath.Base(p.DB)
	for _, w := range words {
		if strings.Contains(w, "OTAG_DB") {
			return deny(dbDenied)
		}
		if strings.HasPrefix(w, "-") {
			_, w, _ = strings.Cut(w, "=") // --db=path
		}
		if !strings.Contains(filepath.Base(w), base) || strings.ContainsAny(w, "$`") {
			continue
		}
		w, ok := expandHome(w)
		if !ok {
			continue
		}
		abs, d := p.absolute(w)
		if !d.Allow {
			continue
		}
		if real, err := resolve(abs); err == nil && p.otagState(real) {
			return deny(dbDenied)
		}
	}
	return allow
}

// isOtag reports whether word runs otag (a child's policy names its binary).
func (p Policy) isOtag(word string) bool {
	name := filepath.Base(word)
	return p.OtagBin != "" && (name == "otag" || name == filepath.Base(p.OtagBin) || strings.Contains(word, "OTAG_BIN"))
}

// checkOtagEnv keeps a child's otag identity (OTAG_RUN_ID, OTAG_RUN_TOKEN, ...) intact:
// without it the CLI would take the child for the human. It refuses a otag command
// whose environment is changed in front of it (OTAG_*= assignments, env -i, env -u
// OTAG_*), and changing OTAG_* variables in the shell at all (unset, export, a bare
// assignment), since the shell keeps them for later commands.
func (p Policy) checkOtagEnv(words []string) Decision {
	if p.OtagBin == "" || len(words) == 0 {
		return allow
	}
	const why = "a child keeps its otag environment (OTAG_*); otag commands act as this run"
	assign := func(w string) bool {
		return strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.HasPrefix(w, "-")
	}
	bare := true
	for _, w := range words {
		if !assign(w) {
			bare = false
			break
		}
	}
	if bare {
		for _, w := range words {
			if strings.HasPrefix(w, "OTAG_") {
				return deny(why)
			}
		}
		return allow
	}
	cmd, _, _, _ := unwrap(words, p.Level)
	if len(cmd) == 0 {
		return allow
	}
	switch filepath.Base(cmd[0]) {
	case "unset", "export", "declare", "typeset", "readonly", "local", "set":
		for _, w := range cmd[1:] {
			if strings.Contains(w, "OTAG_") {
				return deny(why)
			}
		}
	}
	if !p.isOtag(cmd[0]) {
		return allow
	}
	for _, w := range words[:len(words)-len(cmd)] {
		short := strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "i")
		if strings.Contains(w, "OTAG_") || w == "-" || w == "--ignore-environment" || short {
			return deny(why)
		}
	}
	return allow
}

// checkTmux refuses tmux on otag's server: -L <otag socket>, or plain tmux, which inside
// a otag pane ($TMUX) talks to otag's server.
func (p Policy) checkTmux(args []string) Decision {
	if p.TmuxSocket == "" {
		return allow
	}
	socket, path := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-L":
			socket = value()
		case strings.HasPrefix(a, "-L"):
			socket = a[2:]
		case a == "-S":
			path = value()
		case strings.HasPrefix(a, "-S"):
			path = a[2:]
		case a == "-f" || a == "-T" || a == "-c":
			value()
		case !strings.HasPrefix(a, "-"):
			i = len(args)
		}
	}
	switch {
	case socket == "" && path == "":
		return deny("plain tmux inside a otag pane talks to otag's own tmux server; use tmux -L <name> for a server of your own")
	case socket == p.TmuxSocket || strings.Contains(socket+path, "OTAG_TMUX") ||
		path != "" && filepath.Base(path) == p.TmuxSocket:
		return deny("otag's tmux server (-L %s) is not for agents", p.TmuxSocket)
	}
	return allow
}

// childCommands are the otag commands a child may run: its own run's commands and
// read-only views ("cmd" or "cmd sub"). send and reply are open here; the CLI limits
// them to the child's scope (send only to coord, reply only to messages for its run or
// task), verified with the run token; review only for a review run (run.Review).
var childCommands = []string{"done", "fail", "ask", "inbox", "reply", "send", "review", "prime", "context", "status", "profiles", "version",
	"help", "task me", "task list", "task show", "run list", "run show", "notify list"}

// IsChildCommand reports whether `otag <cmd> [<sub>]` is open to a child (the CLI
// enforces the same list itself, for commands no shell line shows).
func IsChildCommand(cmd, sub string) bool {
	return slices.Contains(childCommands, cmd) || sub != "" && slices.Contains(childCommands, cmd+" "+sub)
}

// checkOtag refuses otag coordinator commands (start/stop runs, task changes, sync,
// daemon, raw events) from a child.
func checkOtag(args []string) Decision {
	var pos []string
	for i := 0; i < len(args) && len(pos) < 2; i++ {
		a := args[i]
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case strings.HasPrefix(a, "-") && (name == "run" || name == "project"):
			return deny("a child acts only on its own run and project; drop --%s", name)
		case a == "--request-id" || a == "-request-id":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 || slices.Contains(childCommands, pos[0]) ||
		len(pos) == 2 && slices.Contains(childCommands, pos[0]+" "+pos[1]) {
		return allow
	}
	return deny("`otag %s` is a coordinator command; a child runs otag done, fail, ask, inbox, reply, send coord, task me, "+
		"context, status and read-only list/show", strings.Join(pos, " "))
}

// gitCall is a git command line split at the subcommand.
type gitCall struct {
	sub     string
	rest    []string // arguments after the subcommand
	configs []string // -c / --config-env values
	dirs    []string // -C values, in order
	trees   []string // --git-dir / --work-tree values
}

// parseGit skips git's global options, collecting the ones that matter.
func parseGit(args []string) gitCall {
	var g gitCall
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-c" || a == "--config-env":
			g.configs = append(g.configs, value())
		case strings.HasPrefix(a, "--config-env="):
			g.configs = append(g.configs, strings.TrimPrefix(a, "--config-env="))
		case a == "-C":
			g.dirs = append(g.dirs, value())
		case a == "--git-dir" || a == "--work-tree":
			g.trees = append(g.trees, value())
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			_, v, _ := strings.Cut(a, "=")
			g.trees = append(g.trees, v)
		case a == "--namespace" || a == "--exec-path" || a == "--super-prefix" || a == "--attr-source":
			value()
		case strings.HasPrefix(a, "-c"):
			g.configs = append(g.configs, strings.TrimPrefix(a, "-c"))
		case strings.HasPrefix(a, "-"):
		default:
			g.sub, g.rest = a, args[i+1:]
			return g
		}
	}
	return g
}

// gitDirEnv are the variables that point git at another repository or work tree.
var gitDirEnv = []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_COMMON_DIR=", "GIT_INDEX_FILE=", "GIT_OBJECT_DIRECTORY="}

// checkGitEnv refuses GIT_DIR=... and friends pointing outside the workspace.
func (p Policy) checkGitEnv(words []string) Decision {
	for _, w := range words {
		for _, e := range gitDirEnv {
			if v, ok := strings.CutPrefix(w, e); ok {
				if d := p.gitPath(p.base(), v, strings.TrimSuffix(e, "=")); !d.Allow {
					return d
				}
			}
		}
	}
	return allow
}

// gitPath checks that a directory given to git (relative to dir) is in the workspace.
// A light run allows a path it cannot resolve.
func (p Policy) gitPath(dir, v, what string) Decision {
	if v == "" {
		return allow
	}
	light := p.Level.IsLight()
	if strings.ContainsAny(v, "$`") {
		if light {
			return allow
		}
		return deny("cannot check %s %s (shell expansion); use a plain path", what, v)
	}
	v, ok := expandHome(v)
	if !ok {
		if light {
			return allow
		}
		return deny("cannot resolve %s %s", what, v)
	}
	if !filepath.IsAbs(v) {
		if p.cwdUnknown {
			if light {
				return allow
			}
			return deny("cannot tell where %s %s points after a cd to an unknown directory", what, v)
		}
		v = join(dir, v)
	}
	if !p.Inside(v) {
		return deny("%s %s points outside the workspace (%s); git may only act on this worktree", what, v, p.Workdir)
	}
	return allow
}

const pushDenied = "git push is not allowed; commit and run `otag done`, the coordinator merges"

// checkGit refuses pushes, aliases, git pointed outside the workspace and changes to
// refs other than the run's own branch. A light run leaves aliases, config and the
// command-running features (submodule foreach, rebase --exec) alone, and refuses the
// commands that move a checkout (worktreeGit) run in another one.
func (p Policy) checkGit(name string, args []string) Decision {
	g := parseGit(args)
	light := p.Level.IsLight()
	for _, v := range g.configs {
		if strings.HasPrefix(strings.ToLower(v), "alias.") && !light {
			return deny("git command aliases supplied through config are not allowed")
		}
	}
	dir := p.base()
	for _, c := range g.dirs {
		if d := p.gitPath(dir, c, "git -C"); !d.Allow {
			return d
		}
		if c != "" {
			c, _ = expandHome(c)
			if filepath.IsAbs(c) {
				dir = c
			} else {
				dir = join(dir, c)
			}
		}
	}
	for _, t := range g.trees {
		if d := p.gitPath(dir, t, "git --git-dir/--work-tree"); !d.Allow {
			return d
		}
	}
	sub, rest := g.sub, g.rest
	if name != "git" {
		sub, rest = strings.TrimPrefix(name, "git-"), args
	}
	own := p.Branch
	if own == "" {
		own = "(none)"
	}
	if light && slices.Contains(worktreeGit, sub) && !p.cwdUnknown && len(g.trees) == 0 && !p.Inside(dir) {
		return deny("git %s in %s would change another checkout; git may only act on this worktree (%s)", sub, dir, p.Workdir)
	}
	switch sub {
	case "push", "send-pack", "http-push":
		return deny(pushDenied)
	case "update-ref":
		return deny("git update-ref is not allowed; commit on your branch %s instead", own)
	case "branch":
		return p.checkGitBranch(rest)
	case "checkout", "switch":
		for i, a := range rest {
			target := ""
			switch {
			case a == "-B" || a == "-C" || a == "--force-create":
				if i+1 < len(rest) {
					target = rest[i+1]
				}
			case strings.HasPrefix(a, "--force-create="):
				target = strings.TrimPrefix(a, "--force-create=")
			default:
				continue
			}
			if target != p.Branch || p.Branch == "" {
				return deny("git %s %s %s would move another branch; only your branch %s may be reset", sub, a, target, own)
			}
		}
	case "worktree":
		if ops := operands(rest); len(ops) > 0 && slices.Contains([]string{"remove", "prune", "move"}, ops[0]) {
			return deny("git worktree %s is not allowed: otag manages worktrees", ops[0])
		}
	case "config":
		if light {
			break
		}
		for _, a := range rest {
			if strings.HasPrefix(strings.ToLower(a), "alias.") {
				return deny("defining git aliases is not allowed")
			}
			if a == "--global" || a == "--system" {
				return deny("changing the user's or the system git config is not allowed")
			}
		}
	case "submodule":
		if slices.Contains(rest, "foreach") && !light {
			return deny("git submodule foreach is not allowed: it runs commands the guard cannot inspect")
		}
	case "rebase":
		if light {
			break
		}
		for _, a := range rest {
			if a == "-x" || a == "--exec" || strings.HasPrefix(a, "--exec=") || strings.HasPrefix(a, "-x") && !strings.HasPrefix(a, "--") {
				return deny("git rebase --exec runs commands the guard cannot inspect; rebase without it")
			}
		}
	}
	return allow
}

// checkGitBranch refuses `git branch` forms that move, rename or delete a branch other
// than the run's own.
func (p Policy) checkGitBranch(args []string) Decision {
	var force, del, move, forceCopy bool
	var ops []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			ops = append(ops, args[i+1:]...)
			i = len(args)
		case a == "--force":
			force = true
		case a == "--delete":
			del = true
		case a == "--move":
			move = true
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && a != "-":
			for j, c := range a[1:] {
				switch c {
				case 'f':
					force = true
				case 'd', 'D':
					del = true
				case 'm', 'M':
					move = true
				case 'C':
					forceCopy = true
				case 'u':
					if j == len(a)-2 {
						i++ // -u <upstream>
					}
				}
			}
		default:
			ops = append(ops, a)
		}
	}
	others := func(names []string) string {
		for _, n := range names {
			if n != p.Branch || p.Branch == "" {
				return n
			}
		}
		return ""
	}
	var bad string
	switch {
	case del || move:
		bad = others(ops)
		if bad == "" && len(ops) == 0 && move {
			bad = "(rename)"
		}
	case forceCopy && len(ops) > 0:
		bad = others(ops[len(ops)-1:])
	case force && len(ops) > 0:
		bad = others(ops[:1])
	}
	if bad != "" {
		own := p.Branch
		if own == "" {
			own = "(none in this workspace)"
		}
		return deny("git branch may not move, rename or delete %s; only your branch %s", bad, own)
	}
	return allow
}

// checkSed refuses `sed -i` on files that fail CheckWrite.
func (p Policy) checkSed(args []string) Decision {
	inPlace, script := false, false
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case a == "-i" || a == "-I":
			inPlace = true
			// BSD sed takes the backup extension as its own word (sed -i '' ...).
			if i+1 < len(args) && (args[i+1] == "" || strings.HasPrefix(args[i+1], ".")) {
				i++
			}
		case strings.HasPrefix(a, "--in-place"):
			inPlace = true
		case a == "-e" || a == "-f" || a == "--expression" || a == "--file":
			script = true
			i++
		case strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file="):
			script = true
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && a != "-":
			flags := a[1:]
			if j := strings.IndexAny(flags, "iI"); j >= 0 {
				inPlace = true // the rest is GNU's backup suffix
				flags = flags[:j]
			}
			if k := strings.IndexAny(flags, "ef"); k >= 0 {
				script = true
				if k == len(flags)-1 && len(flags) == len(a)-1 {
					i++
				}
			}
		default:
			files = append(files, a)
		}
	}
	if !inPlace {
		return allow
	}
	if !script && len(files) > 0 {
		files = files[1:] // the script
	}
	for _, f := range files {
		if d := p.checkShellTarget(f); !d.Allow {
			return d
		}
	}
	return allow
}

// checkPerl refuses `perl -i` on files that fail CheckWrite.
func (p Policy) checkPerl(args []string) Decision {
	inPlace, script := false, false
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-") && a != "-":
		flags:
			for j, c := range a[1:] {
				switch {
				case c == 'i':
					inPlace = true
					break flags // the rest is the backup suffix
				case c == 'e' || c == 'E':
					script = true
					if j == len(a)-2 {
						i++
					}
					break flags
				case strings.ContainsRune("IMmlx0dDC", c):
					break flags // takes the rest as its value
				}
			}
		default:
			files = append(files, a)
		}
	}
	if !inPlace {
		return allow
	}
	if !script && len(files) > 0 {
		files = files[1:] // the program file
	}
	for _, f := range files {
		if d := p.checkShellTarget(f); !d.Allow {
			return d
		}
	}
	return allow
}

// checkDownload checks the output files of curl (-o, --output, --output-dir) and wget
// (-O, --output-document, -P).
func (p Policy) checkDownload(name string, args []string) Decision {
	short, long := "o", []string{"--output", "--output-dir"}
	if name == "wget" {
		short, long = "OP", []string{"--output-document", "--directory-prefix"}
	}
	var outs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains(long, a):
			if i+1 < len(args) {
				i++
				outs = append(outs, args[i])
			}
		case strings.HasPrefix(a, "--"):
			if k, v, ok := strings.Cut(a, "="); ok && slices.Contains(long, k) {
				outs = append(outs, v)
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if j := strings.IndexAny(a[1:], short); j >= 0 {
				if v := a[j+2:]; v != "" {
					outs = append(outs, v)
				} else if i+1 < len(args) {
					i++
					outs = append(outs, args[i])
				}
			}
		}
	}
	for _, o := range outs {
		if o == "-" {
			continue
		}
		if d := p.checkShellTarget(o); !d.Allow {
			return d
		}
	}
	return allow
}

// targetDirs returns the -t/--target-directory values of cp, mv, install and ln.
func targetDirs(args []string) []string {
	var out []string
	for i, a := range args {
		switch {
		case (a == "-t" || a == "--target-directory") && i+1 < len(args):
			out = append(out, args[i+1])
		case strings.HasPrefix(a, "--target-directory="):
			out = append(out, strings.TrimPrefix(a, "--target-directory="))
		}
	}
	return out
}

// checkLink checks ln: the new link must be writable, and a hard link makes its source
// writable through the new name, so the source must be too. A symlink may point
// anywhere: a later write through it is resolved (CheckWrite follows links).
func (p Policy) checkLink(args []string) Decision {
	symbolic := symbolicLink(args)
	ops := operands(args)
	if len(ops) == 0 {
		return allow
	}
	link, srcs := ops[len(ops)-1], ops[:len(ops)-1]
	if len(ops) == 1 {
		link, srcs = filepath.Base(ops[0]), ops
	}
	for _, t := range append(targetDirs(args), link) {
		if d := p.checkShellTarget(t); !d.Allow {
			return d
		}
	}
	if symbolic {
		return allow // writes through the link are resolved (see notePending for this line)
	}
	for _, s := range srcs {
		if d := p.checkShellTarget(s); !d.Allow {
			return deny("a hard link to %s would make it writable from the workspace (%s)", s, d.Reason)
		}
	}
	return allow
}

// symbolicLink reports whether ln args ask for a symbolic link (-s, --symbolic).
func symbolicLink(args []string) bool {
	for _, a := range args {
		if a == "--symbolic" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "s") {
			return true
		}
	}
	return false
}

// notePending remembers the symlinks an `ln -s` creates, so a later write on the same
// line through one (`ln -s /x l && echo > l/f`) is judged by its target although the
// link does not exist yet when the line is checked.
func (p *Policy) notePending(args []string) {
	ops := operands(args)
	if len(ops) < 2 || !symbolicLink(args) {
		return
	}
	link, err := p.absolute(ops[len(ops)-1])
	if !err.Allow {
		return
	}
	for _, src := range ops[:len(ops)-1] {
		l := link
		if fi, err := os.Stat(l); err == nil && fi.IsDir() {
			l = join(l, filepath.Base(src))
		}
		src, _ = expandHome(src)
		if !filepath.IsAbs(src) {
			src = join(filepath.Dir(l), src)
		}
		p.links = append(p.links, [2]string{tidy(l), src})
	}
}

// tidy drops empty and "." components but keeps "..", which only resolve may apply.
func tidy(p string) string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return "/" + strings.Join(out, "/")
}

// throughPending maps abs through a symlink created earlier on the line.
func (p Policy) throughPending(abs string) string {
	abs = tidy(abs)
	for _, l := range p.links {
		if abs == l[0] {
			return l[1]
		}
		if rest, ok := strings.CutPrefix(abs, l[0]+"/"); ok {
			return join(l[1], rest)
		}
	}
	return abs
}

// dangerousSubst marks text the parser cannot see into: command substitution,
// backticks and parameter expansion.
var dangerousSubst = regexp.MustCompile("\\$[({A-Za-z_]|`")

// dangerousGit reports whether text mentions a git push, a ref change, an alias or a
// command-running git feature: the textual fallback for $(...), backticks, eval and
// sh -c payloads with expansions. A light run looks only for pushes and ref changes.
func dangerousGit(text string, level Level) bool {
	if !dangerousSubst.MatchString(text) {
		return false
	}
	f := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(";&|()<>`'\"${}\\=", r)
	})
	for i, w := range f {
		switch w {
		case "git-push", "git-send-pack", "git-http-push", "git-update-ref":
			return true
		}
		if w != "git" && !strings.HasSuffix(w, "/git") {
			continue
		}
		next := f[i+1 : min(len(f), i+12)]
		for j, n := range next {
			after := ""
			if j+1 < len(next) {
				after = next[j+1]
			}
			switch {
			case n == "push" || n == "send-pack" || n == "http-push" || n == "update-ref":
				return true
			case n == "worktree" && slices.Contains([]string{"remove", "prune", "move"}, after):
				return true
			case n == "branch" && strings.HasPrefix(after, "-") && strings.ContainsAny(after, "fdDmMC"):
				return true
			case level.IsLight():
				// aliases and command-running features are left alone
			case n == "foreach" || strings.HasPrefix(strings.ToLower(n), "alias."):
				return true
			case n == "rebase" && (slices.Contains(next, "--exec") || slices.Contains(next, "-x")):
				return true
			}
		}
	}
	return false
}
