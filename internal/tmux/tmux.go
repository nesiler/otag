// Package tmux drives otag's private tmux server (`tmux -L otag`). The user's own tmux
// server and Herdr sessions are never addressed: every command carries -L, and TMUX /
// TMUX_PANE are removed from the environment of tmux commands.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// ErrNoTmux means the tmux binary is not installed.
var ErrNoTmux = errors.New("tmux not found on PATH")

// Client runs tmux commands against one socket.
type Client struct {
	Socket string // tmux -L name
	Conf   string // tmux -f file, used when the server starts
	Bin    string // default "tmux"
}

// New returns a client for socket (tmux -L) using conf as server config.
func New(socket, conf string) *Client { return &Client{Socket: socket, Conf: conf, Bin: "tmux"} }

// Available reports whether tmux can be run.
func (c *Client) Available() bool {
	_, err := exec.LookPath(c.bin())
	return err == nil
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "tmux"
	}
	return c.Bin
}

// Base returns the tmux argv prefix (binary, -L, -f).
func (c *Client) Base() []string {
	args := []string{c.bin(), "-L", c.Socket}
	if c.Conf != "" {
		args = append(args, "-f", c.Conf)
	}
	return args
}

// sep separates tmux commands in one invocation. Every other argument is passed
// through escapeArg, so data can never be read as a separator.
const sep = "\x00;"

// escapeArg keeps a trailing ";" literal: tmux splits any argument ending in ";" into
// a command separator, and turns a trailing "\;" into a literal ";".
func escapeArg(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + `\;`
	}
	return s
}

// run executes tmux with args and returns trimmed stdout.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	return c.runPath(ctx, "", args...)
}

// runPath is run with PATH set to pathEnv (when not empty) in the tmux client's
// environment: tmux gives a new pane the PATH of the client that created it, even over
// new-session/new-window -e PATH=....
func (c *Client) runPath(ctx context.Context, pathEnv string, args ...string) (string, error) {
	return c.exec(ctx, pathEnv, nil, args...)
}

// exec runs tmux with args, PATH set to pathEnv (when not empty) and stdin (nil: none).
func (c *Client) exec(ctx context.Context, pathEnv string, stdin io.Reader, args ...string) (string, error) {
	path, err := exec.LookPath(c.bin())
	if err != nil {
		return "", ErrNoTmux
	}
	// -u: in a non-UTF-8 locale (a run's clean environment, a bare launchd one) tmux
	// prints the tabs of otag's -F formats as "_", and every pane would look gone.
	argv := []string{"-u"}
	for _, a := range append(c.Base()[1:], args...) {
		if a == sep {
			argv = append(argv, ";")
		} else {
			argv = append(argv, escapeArg(a))
		}
	}
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Env = cleanEnv(os.Environ())
	if pathEnv != "" {
		cmd.Env = append(cmd.Env, "PATH="+pathEnv) // the last PATH wins
	}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &Error{Args: args, Msg: msg}
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// Error is a failed tmux command.
type Error struct {
	Args []string
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("tmux %s: %s", e.Args[0], e.Msg) }

// noServer reports whether err means the server (socket) is not running.
func noServer(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	// Only errors that say the server is absent: no socket (ENOENT), or a socket nobody
	// listens on (ECONNREFUSED, a stale socket). "server exited": it shut down while
	// answering (its last window was just killed). Anything else, such as a socket we
	// may not open (EACCES), says nothing about the panes: unknown, not gone.
	m := te.Msg
	return strings.Contains(m, "no server running") || strings.Contains(m, "server exited") ||
		(strings.Contains(m, "error connecting to") &&
			(strings.Contains(m, "(No such file or directory)") || strings.Contains(m, "(Connection refused)")))
}

// noSessions reports whether err of `list-panes -a` means the server has no session
// left: it is exiting after its last window closed ("no current target", "no
// sessions"). For a listing of every pane that is an empty server; for a command with
// a target the same text would mean a wrong target, so only Panes uses it.
func noSessions(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	return strings.Contains(te.Msg, "no current target") || strings.Contains(te.Msg, "no sessions")
}

func notFound(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	return noServer(err) || strings.Contains(te.Msg, "can't find") || strings.Contains(te.Msg, "not found")
}

func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Window describes a window to create.
type Window struct {
	Session string // created when missing
	Name    string
	Cwd     string
	Env     map[string]string
	Argv    []string // exec'd directly
	// Run and Owner tag the window with the user options @otag_run (the run id) and
	// @otag_db (the database the run lives in). They are set in the same command list
	// that creates the window, so no window otag starts ever exists untagged.
	Run, Owner string
	// CleanEnv starts Argv with only Env and the pane's terminal variables (keepEnv):
	// whatever the tmux server's global environment holds (the shell that started
	// it: GITHUB_TOKEN, AWS_*, ...) is unset first. Values never appear on a command
	// line: a small sh wrapper unsets the other names, then execs Argv.
	CleanEnv bool
	// EnvDir, with CleanEnv, is a private (0700) directory where Env is handed to the
	// pane in a 0600 file the pane sources and deletes, instead of `-e NAME=value`
	// arguments that `ps` shows.
	EnvDir string
}

// keepEnv are set by tmux for the pane itself and survive CleanEnv.
var keepEnv = []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TMUX", "TMUX_PANE"}

// cleanArgv wraps argv in a sh that unsets every variable but keep, then execs argv.
// With envFile it first sources the file (Env, see writeEnvFile) and deletes it.
func cleanArgv(keep []string, envFile string, argv []string) []string {
	sort.Strings(keep)
	quoted := make([]string, 0, len(keep))
	for _, k := range keep {
		quoted = append(quoted, ShellQuote([]string{k}))
	}
	script := ""
	if envFile != "" {
		// Open, delete, then source from the fd: the file's own variables (say `f`)
		// cannot redirect the delete, and the secrets are gone before argv runs.
		script = `f=` + ShellQuote([]string{envFile}) + `; exec 3<"$f" || exit 1; rm -f "$f"; . /dev/fd/3 || exit 1; exec 3<&-; `
	}
	script += `for n in $(/usr/bin/env | /usr/bin/sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p'); do ` +
		`case "$n" in ` + strings.Join(quoted, "|") + `) ;; *) unset "$n" ;; esac; done; exec "$@"`
	return append([]string{"/bin/sh", "-c", script, "otag-env"}, argv...)
}

// envName is a variable name sh can export.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// writeEnvFile writes env as `export K='v'` lines to a new 0600 file in dir, so the
// values (the run token, account credentials) never appear on a tmux command line,
// where `ps` shows them. Names sh cannot export are left to -e.
func writeEnvFile(dir string, env map[string]string) (string, error) {
	f, err := os.CreateTemp(dir, ".env-*")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if envName.MatchString(k) {
			b.WriteString("export " + k + "='" + strings.ReplaceAll(env[k], "'", `'\''`) + "'\n")
		}
	}
	_, werr := f.WriteString(b.String())
	if err := errors.Join(werr, f.Chmod(0o600), f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// Window user options that tag otag's windows (see Window.Run).
const (
	OptRun   = "@otag_run"
	OptOwner = "@otag_db"
)

// PaneRef identifies a created pane.
type PaneRef struct {
	Session  string `json:"session"`
	WindowID string `json:"window"`
	PaneID   string `json:"pane"`
	PID      int    `json:"pid"`
}

const refFormat = "#{session_name}\t#{window_id}\t#{pane_id}\t#{pane_pid}"

// NewWindow starts w.Argv in a new detached window of w.Session, creating the session
// if needed. Windows keep their pane after the process exits (remain-on-exit), so the
// exit status stays observable until the window is killed.
func (c *Client) NewWindow(ctx context.Context, w Window) (PaneRef, error) {
	if len(w.Argv) == 0 {
		return PaneRef{}, errors.New("tmux: empty argv")
	}
	var opts []string
	if w.Cwd != "" {
		opts = append(opts, "-c", w.Cwd)
	}
	keys := make([]string, 0, len(w.Env))
	for k := range w.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	envFile := ""
	if w.CleanEnv && w.EnvDir != "" {
		var err error
		if envFile, err = writeEnvFile(w.EnvDir, w.Env); err != nil {
			return PaneRef{}, err
		}
		defer func() {
			if envFile != "" {
				_ = os.Remove(envFile) // the pane never started: nobody else will
			}
		}()
	}
	for _, k := range keys {
		if envFile == "" || !envName.MatchString(k) {
			opts = append(opts, "-e", k+"="+w.Env[k])
		}
	}
	opts = append(opts, "-P", "-F", refFormat, "--")
	argv := w.Argv
	if w.CleanEnv {
		argv = cleanArgv(append(slices.Clone(keepEnv), keys...), envFile, argv)
	}
	if len(argv) == 1 {
		// tmux runs a single shell-command argument through the shell; with two or
		// more it execs directly. env keeps argv[0] from being shell-parsed.
		opts = append(opts, "/usr/bin/env")
	}
	opts = append(opts, argv...)
	// remain-on-exit is set globally on otag's private server, in the same command list,
	// so it is in place before the process can exit.
	pre := []string{"set-option", "-gw", "remain-on-exit", "on", sep}
	// The tags follow in the same list. tmux runs one client's command list without
	// interleaving other clients' commands, and the new window is the session's last
	// one (new-window -a after {end}), so {end} is the window just created.
	var post []string
	last := "=" + w.Session + ":{end}"
	for _, o := range [][2]string{{OptRun, w.Run}, {OptOwner, w.Owner}} {
		if o[1] != "" {
			post = append(post, sep, "set-option", "-w", "-t", last, o[0], o[1])
		}
	}

	var out string
	var err error
	if !c.HasSession(ctx, w.Session) {
		args := append(append([]string{}, pre...), "new-session", "-d", "-s", w.Session, "-n", w.Name, "-x", "200", "-y", "50")
		out, err = c.runPath(ctx, w.Env["PATH"], append(append(args, opts...), post...)...)
		var te *Error
		if err != nil && errors.As(err, &te) && strings.Contains(te.Msg, "duplicate session") {
			err = errRetryNewWindow
		}
	} else {
		err = errRetryNewWindow
	}
	if errors.Is(err, errRetryNewWindow) {
		args := append(append([]string{}, pre...), "new-window", "-d", "-a", "-t", last, "-n", w.Name)
		out, err = c.runPath(ctx, w.Env["PATH"], append(append(args, opts...), post...)...)
	}
	if err != nil {
		return PaneRef{}, err
	}
	envFile = "" // the pane reads and deletes it
	return parseRef(out)
}

var errRetryNewWindow = errors.New("use new-window")

func parseRef(out string) (PaneRef, error) {
	f := strings.Split(strings.TrimSpace(out), "\t")
	if len(f) != 4 {
		return PaneRef{}, fmt.Errorf("tmux: unexpected output %q", out)
	}
	pid, _ := strconv.Atoi(f[3])
	return PaneRef{Session: f[0], WindowID: f[1], PaneID: f[2], PID: pid}, nil
}

// HasSession reports whether session exists (exact name).
func (c *Client) HasSession(ctx context.Context, session string) bool {
	_, err := c.run(ctx, "has-session", "-t", "="+session)
	return err == nil
}

// PaneInfo is the live state of a pane.
type PaneInfo struct {
	PaneID     string
	Session    string
	WindowID   string
	WindowName string
	PID        int
	Dead       bool
	ExitStatus int // valid when Dead
	Command    string
	// Activity is when the pane's window last produced output (#{window_activity},
	// seconds). otag gives every run pane a window of its own. Zero when unknown.
	Activity time.Time
	// Run and Owner are the window's tags (Window.Run, Window.Owner); "" = untagged.
	Run, Owner string
}

// infoFormat puts the fields otag controls first and free text (session, window name,
// command) last, so a tab in a name can only garble the fields after it.
const infoFormat = "#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}\t#{window_activity}\t#{window_id}\t" +
	"#{" + OptRun + "}\t#{" + OptOwner + "}\t#{session_name}\t#{window_name}\t#{pane_current_command}"

// Panes lists every pane of the server by pane id; a stopped server yields an empty map.
// Output it cannot parse is an error, never a shorter map: a pane missing from the map
// counts as gone.
func (c *Client) Panes(ctx context.Context) (map[string]PaneInfo, error) {
	out, err := c.run(ctx, "list-panes", "-a", "-F", infoFormat)
	if noServer(err) || noSessions(err) {
		return map[string]PaneInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]PaneInfo{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 11)
		if len(f) != 11 || !strings.HasPrefix(f[0], "%") || (f[2] != "0" && f[2] != "1") {
			return nil, fmt.Errorf("tmux list-panes: unexpected line %q", line)
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil && f[1] != "" { // tmux 3.8 prints no pid for a dead pane
			return nil, fmt.Errorf("tmux list-panes: unexpected line %q", line)
		}
		status, _ := strconv.Atoi(f[3])
		var activity time.Time
		if sec, err := strconv.ParseInt(f[4], 10, 64); err == nil && sec > 0 {
			activity = time.Unix(sec, 0)
		}
		m[f[0]] = PaneInfo{PaneID: f[0], PID: pid, Dead: f[2] == "1", ExitStatus: status, Activity: activity,
			WindowID: f[5], Run: f[6], Owner: f[7], Session: f[8], WindowName: f[9], Command: f[10]}
	}
	return m, nil
}

// Liveness states.
const (
	Alive   = "alive"   // pane exists and its process runs
	Exited  = "exited"  // pane kept after its process exited (ExitStatus set)
	Gone    = "gone"    // pane, window or server no longer exists
	Unknown = "unknown" // no pane recorded, or tmux unavailable
)

// Liveness is the result of a pane check.
type Liveness struct {
	State      string `json:"state"`
	Pane       string `json:"pane,omitempty"`
	PID        int    `json:"pid,omitempty"`
	ExitStatus *int   `json:"exit_status,omitempty"`
	Command    string `json:"command,omitempty"`
	Detail     string `json:"detail,omitempty"`
	// Activity is the last output of the pane's window (alive panes; zero if unknown).
	Activity time.Time `json:"activity,omitzero"`
}

// Live reports whether the harness may still be running.
func (l Liveness) Live() bool { return l.State == Alive }

// Check returns the liveness of paneID; pid (when non-zero) must match the pane's
// process, which guards against pane ids reused by a restarted server.
func (c *Client) Check(ctx context.Context, paneID string, pid int) Liveness {
	if paneID == "" {
		return Liveness{State: Unknown, Detail: "no pane recorded"}
	}
	panes, err := c.Panes(ctx)
	if err != nil {
		return Liveness{State: Unknown, Pane: paneID, Detail: err.Error()}
	}
	return CheckIn(panes, paneID, pid)
}

// CheckIn evaluates liveness against a Panes snapshot (one tmux call for many runs).
func CheckIn(panes map[string]PaneInfo, paneID string, pid int) Liveness {
	if paneID == "" {
		return Liveness{State: Unknown, Detail: "no pane recorded"}
	}
	p, ok := panes[paneID]
	switch {
	case !ok:
		return Liveness{State: Gone, Pane: paneID, PID: pid, Detail: "pane not found"}
	// tmux 3.8+ reports an empty pane_pid for a dead pane, so a dead pane without a
	// pid is judged by its id alone.
	case pid != 0 && p.PID != pid && (!p.Dead || p.PID != 0):
		return Liveness{State: Gone, Pane: paneID, PID: pid, Detail: fmt.Sprintf("pane now runs pid %d", p.PID)}
	case p.Dead:
		st := p.ExitStatus
		if p.PID == 0 {
			p.PID = pid
		}
		return Liveness{State: Exited, Pane: paneID, PID: p.PID, ExitStatus: &st, Detail: fmt.Sprintf("exit status %d", st)}
	case !processAlive(p.PID):
		return Liveness{State: Exited, Pane: paneID, PID: p.PID, Detail: "process not running"}
	}
	return Liveness{State: Alive, Pane: paneID, PID: p.PID, Command: p.Command, Activity: p.Activity}
}

// ProcessAlive reports whether a process with this pid exists.
func ProcessAlive(pid int) bool { return processAlive(pid) }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// KillWindow kills the window containing target (a pane or window id); a missing
// window or server is not an error.
func (c *Client) KillWindow(ctx context.Context, target string) error {
	if target == "" {
		return nil
	}
	_, err := c.run(ctx, "kill-window", "-t", target)
	if notFound(err) {
		return nil
	}
	return err
}

// KillPane kills the window of paneID only if that pane still runs pid (pid 0: no
// check). A pane id reused after a server restart is left alone. It reports whether a
// window was killed.
func (c *Client) KillPane(ctx context.Context, paneID string, pid int) (bool, error) {
	return c.KillOwned(ctx, paneID, pid, nil)
}

// KillOwned is KillPane with one more condition (nil: none) on the pane as tmux lists
// it now, e.g. its tags: a pane that fails it is left alone, like a reused one.
func (c *Client) KillOwned(ctx context.Context, paneID string, pid int, own func(PaneInfo) bool) (bool, error) {
	if paneID == "" {
		return false, nil
	}
	var l Liveness
	var info PaneInfo
	look := func() {
		panes, err := c.Panes(ctx)
		if err != nil {
			l = Liveness{State: Unknown, Pane: paneID, Detail: err.Error()}
			return
		}
		info, l = panes[paneID], CheckIn(panes, paneID, pid)
	}
	look()
	// An unknown answer may be a race with another client killing windows: look twice
	// more before giving up.
	for i := 0; i < 2 && l.State == Unknown && ctx.Err() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
		look()
	}
	switch l.State {
	case Gone:
		return false, nil
	case Unknown:
		return false, errors.New(l.Detail)
	}
	if own != nil && !own(info) {
		return false, nil
	}
	if err := c.KillWindow(ctx, paneID); err != nil {
		return false, err
	}
	return true, nil
}

// WaitGone polls until paneID's pane disappears or dies (true), or timeout passes or
// ctx ends (false). A check that cannot tell (tmux failed) is not "gone".
func (c *Client) WaitGone(ctx context.Context, paneID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if l := c.Check(ctx, paneID, 0); l.State == Exited || l.State == Gone {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// KillServer stops the tmux server and removes its socket file (tests only; production
// never calls it).
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.run(ctx, "kill-server")
	if noServer(err) {
		err = nil
	}
	if err == nil {
		_ = os.Remove(SocketPath(c.Socket))
	}
	return err
}

// SocketPath is where tmux puts the socket of `tmux -L name`.
func SocketPath(name string) string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return fmt.Sprintf("%s/tmux-%d/%s", strings.TrimRight(dir, "/"), os.Getuid(), name)
}

// AttachArgs returns the argv that attaches a terminal to the window of pane.
func (c *Client) AttachArgs(session, pane string) []string {
	args := []string{c.bin(), "-L", c.Socket}
	if pane != "" {
		args = append(args, "select-window", "-t", pane, ";")
	}
	return append(args, "attach-session", "-t", "="+session)
}

// ShellQuote renders argv as a shell command line (";" becomes `\;`).
func ShellQuote(argv []string) string {
	out := make([]string, len(argv))
	for i, s := range argv {
		switch {
		case s == ";":
			out[i] = `\;`
		// A leading "=" is quoted too: zsh expands =word to the path of command word
		// (tmux's exact-match target "=session").
		case s != "" && s[0] != '=' && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~%"):
			out[i] = s
		default:
			out[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}

// Control returns an adapter.Pane for paneID.
func (c *Client) Control(paneID string) adapter.Pane { return paneCtl{c, paneID} }

type paneCtl struct {
	c  *Client
	id string
}

func (p paneCtl) SendKeys(ctx context.Context, keys ...string) error {
	_, err := p.c.run(ctx, append([]string{"send-keys", "-t", p.id}, keys...)...)
	return err
}

func (p paneCtl) SendText(ctx context.Context, text string) error {
	_, err := p.c.run(ctx, "send-keys", "-t", p.id, "-l", "--", text) // "--": text such as "-R x" is not an option
	return err
}

func (p paneCtl) Capture(ctx context.Context) (string, error) {
	return p.c.run(ctx, "capture-pane", "-p", "-t", p.id)
}
