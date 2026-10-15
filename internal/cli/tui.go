package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui"
)

// cmdTui runs the full-screen TUI (docs/TUI.md). It refuses --json, a non-terminal
// stdin or stdout, and otag's own tmux server, where its return key would detach the
// TUI itself. Inside a run the child gate already refused it (not a child command).
func cmdTui(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("tui")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	switch {
	case len(pos) > 0:
		return result{}, usageErr("usage: otag tui [--project P]")
	case a.json:
		return result{}, usageErr("otag tui is interactive: it has no --json output")
	case !isTerminal(a.env.Stdin) || !isTerminal(a.env.Stdout):
		return result{}, usageErr("otag tui needs a terminal on stdin and stdout")
	}
	socket := paths.TmuxSocket(a.env.Getenv)
	if insideSocket(a.env.Getenv("TMUX"), socket) {
		return result{}, apperr.New(apperr.Forbidden,
			"otag tui inside otag's own tmux server (-L %s): its return key would detach the TUI; run it from another terminal", socket)
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	project := a.project
	if project == "" {
		if id, err := a.identity(ctx); err == nil && id.Project != nil {
			project = id.Project.Name
		}
	}
	tc := tmux.New(socket, paths.TmuxConf(a.env.Getenv))
	if p, err := exec.LookPath(tc.Bin); err == nil {
		tc.Bin = p // the TUI runs tmux several times a second: search PATH once
	}
	// The database stays open while an action runs: quitting may leave one behind
	// (a second q), and Main closes the handle on return.
	var calls sync.WaitGroup
	var running atomic.Int64
	call := func(ctx context.Context, args ...string) tui.Outcome {
		calls.Add(1)
		running.Add(1)
		defer func() { running.Add(-1); calls.Done() }()
		return a.tuiCall(ctx, args...)
	}
	err = tui.Run(ctx, tui.Options{
		DB:      d,
		Tmux:    tc,
		Getenv:  a.env.Getenv,
		DataDir: paths.DataDir(a.env.Getenv),
		Project: project,
		Text:    a.tuiText,
		Call:    call,
		Environ: a.env.Environ,
	})
	if n := running.Load(); n > 0 {
		_, _ = fmt.Fprintf(a.env.Stderr, "otag tui: waiting for %d action(s) to finish (ctrl+c to stop)\n", n)
	}
	if !waitFor(ctx, &calls, tuiCallWait) {
		a.db = nil // still in use: the process exit closes it
	}
	return result{}, err
}

// insideSocket reports whether $TMUX (socket path,server pid,session) names the server
// of socket.
func insideSocket(tmuxEnv, socket string) bool {
	path, _, _ := strings.Cut(tmuxEnv, ",")
	return path != "" && path == tmux.SocketPath(socket)
}

// tuiCallWait bounds the wait for actions still running when the TUI ends.
const tuiCallWait = 30 * time.Second

// waitFor waits for wg up to d, or until ctx ends (ctrl+c), and reports whether it
// finished.
func waitFor(ctx context.Context, wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return false
	}
}

// tuiEnv is the environment of the TUI's in-process commands: no working directory
// and no OTAG_PROJECT or OTAG_RUN_* (the TUI refuses to run inside a run, and names
// every project it means with --project), so the cwd it was started from never
// scopes or refuses what it does.
func tuiEnv(e Env) Env {
	drop := func(k string) bool { return k == "OTAG_PROJECT" || strings.HasPrefix(k, "OTAG_RUN_") }
	get := e.Getenv
	e.Cwd = ""
	e.Getenv = func(k string) string {
		if drop(k) {
			return ""
		}
		return get(k)
	}
	if environ := e.Environ; environ != nil {
		e.Environ = func() []string {
			var out []string
			for _, kv := range environ() {
				if k, _, _ := strings.Cut(kv, "="); !drop(k) {
					out = append(out, kv)
				}
			}
			return out
		}
	}
	return e
}

// tuiText runs a read command in-process for the TUI and returns its human output:
// the same code and text as on the command line. It shares a's database handle.
func (a *app) tuiText(ctx context.Context, args ...string) (string, error) {
	_, out, err := a.inProcess(ctx, args)
	return out, err
}

// tuiCall runs a mutation in-process for the TUI (tui.Options.Call): the command
// table, the child gate and the CLI's own code, with --json and a --request-id
// (tui-<random> unless the TUI passed its own, which makes a repeated action
// idempotent). It is not cancelled with ctx: a delivery cut short would have an
// unknown outcome.
func (a *app) tuiCall(ctx context.Context, args ...string) tui.Outcome {
	ctx = context.WithoutCancel(ctx)
	if !hasFlag(args, "request-id") {
		args = append([]string{"--request-id", newRequestID()}, args...)
	}
	res, out, err := a.inProcess(ctx, append([]string{"--json"}, args...))
	o := tui.Outcome{Data: res.data, Human: out}
	if err != nil {
		e, ok := apperr.As(err)
		if !ok {
			e = &apperr.Error{Code: apperr.Internal, Message: err.Error()}
		}
		o.Err = e
	}
	return o
}

// hasFlag reports whether --name (or -name, with or without =value) comes before a
// "--" in args.
func hasFlag(args []string, name string) bool {
	for _, x := range args {
		if x == "--" {
			return false
		}
		if n, _, _ := strings.Cut(strings.TrimLeft(x, "-"), "="); strings.HasPrefix(x, "-") && n == name {
			return true
		}
	}
	return false
}

// newRequestID returns tui-<16 random hex digits>.
func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "tui-" + hex.EncodeToString(b)
}

// inProcess runs a command of the command table with a buffer for stdout and stderr,
// a's database handle and the neutral tuiEnv, after the child gate. The output is
// what the command prints on the command line: its human text (also with --json),
// then stderr.
func (a *app) inProcess(ctx context.Context, args []string) (result, string, error) {
	var out, errb bytes.Buffer
	b := &app{env: tuiEnv(a.env), db: a.db}
	b.env.Stdin, b.env.Stdout, b.env.Stderr = strings.NewReader(""), &out, &errb
	fs := b.flagSet("otag")
	if err := fs.Parse(args); err != nil {
		return result{}, "", usageErr("%v", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return result{}, "", usageErr("missing command")
	}
	for _, c := range commands() {
		if c.name != rest[0] || c.run == nil {
			continue
		}
		if err := b.childGate(ctx, rest); err != nil {
			return result{}, "", err
		}
		res, err := c.run(ctx, b, rest[1:])
		if res.human != nil {
			res.human(&out)
		}
		if b.replayed {
			fmt.Fprintf(&errb, "otag: replayed result of request %s\n", b.requestID)
		}
		if errb.Len() > 0 {
			out.WriteString("\n" + errb.String())
		}
		return res, out.String(), err
	}
	return result{}, "", usageErr("unknown command %q", rest[0])
}
