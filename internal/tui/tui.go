// Package tui is `otag tui`: a full-screen view of projects, tasks and runs with a
// live preview of the selected agent's pane. State comes from the database, never from
// the screen; the TUI itself only reads it. Actions go through the CLI's own command
// code, handed in as Options.Call, so the import direction stays cli → tui.
//
// Design: docs/TUI.md.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/attach"
	"github.com/nesiler/otag/internal/tui/data"
)

// Options wire the TUI to otag.
type Options struct {
	DB      *db.DB       // the same handle the backend uses
	Tmux    *tmux.Client // otag's socket (paths.TmuxSocket / TmuxConf)
	Getenv  func(string) string
	DataDir string
	Project string // initial selection (--project, else the project of the cwd)
	// Call runs a mutation through the CLI command table, with --json and the given
	// --request-id, never cancelled once started.
	Call func(ctx context.Context, args ...string) Outcome
	// Text runs a read command and returns its human output (Details, Stats).
	Text func(ctx context.Context, args ...string) (string, error)
	// Environ is the environment an attach runs with (default os.Environ).
	Environ func() []string
}

// hoverEnv turns on hover (the row under the pointer is highlighted). The design names
// otag.toml [tui] hover; otag has no global config file, so an environment variable does.
const hoverEnv = "OTAG_TUI_HOVER"

// Outcome is the result of Options.Call.
type Outcome struct {
	Data  any           // res.data exactly as --json would carry it
	Human string        // the human output: toast and Log text
	Err   *apperr.Error // code, message, retryable
}

// fps is the renderer's frame rate cap.
const fps = 20

// deps are the model's side effects; tests replace them.
type deps struct {
	load    func(ctx context.Context, req data.Request) (*data.Snapshot, error)
	changed func(ctx context.Context) (bool, error)
	live    func(ctx context.Context, runs []run.Run) map[string]tmux.Liveness
	capture func(ctx context.Context, pane string, history int) (tmux.PaneView, error)
	text    func(ctx context.Context, args ...string) (string, error)
	call    func(ctx context.Context, args ...string) Outcome
	task    func(ctx context.Context, id string) (*task.Task, error) // a task for a form
	newID   func() string                                            // a request id
	now     func() time.Time
	// after runs fn after d (a timer; kind names it for tests, which fire it by hand).
	after func(kind string, d time.Duration, fn func() tea.Msg) tea.Cmd
	// Typing mode and attach (T3).
	// Polish (T4): the UI state file, the attach command to copy, the
	// debug log. Each may be nil.
	attachCmd func(session, pane string) string
	debug     func(format string, args ...any)
	sender    func(ctx context.Context) keySender                             // typed keys to panes
	prepare   func(ctx context.Context, t attach.Target) (attach.Plan, error) // bindings + plan; the error is a warning
	exec      func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd                  // tea.ExecProcess
}

// preparer applies otag's tmux bindings and plans an attach: a popup inside the
// user's own tmux (when it has display-popup -B), else a client in this terminal.
func preparer(o Options) func(ctx context.Context, t attach.Target) (attach.Plan, error) {
	environ := o.Environ
	if environ == nil {
		environ = os.Environ
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	var once sync.Once
	var version string
	return func(ctx context.Context, t attach.Target) (attach.Plan, error) {
		warn := o.Tmux.EnsureUI(ctx)
		e := attach.Env{TMUX: getenv("TMUX"), TMUXPane: getenv("TMUX_PANE"), Environ: environ()}
		if e.TMUX != "" {
			// The user's tmux: the same binary as otag's (its client speaks to either server).
			e.Outer = o.Tmux.Bin
			once.Do(func() {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				out, _ := exec.CommandContext(ctx, e.Outer, "-V").Output()
				version = strings.TrimSpace(string(out))
			})
			e.Version = version
		}
		return attach.Decide(o.Tmux, t, e), warn
	}
}

// Run shows the TUI until the user quits or parent ends.
func Run(parent context.Context, o Options) error {
	// Every job of the TUI runs under ctx: none outlives Run.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w, err := data.NewWatcher(ctx, o.DB)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	// The baseline comes before the first load: a commit between the two is a change.
	if _, err := w.Changed(ctx); err != nil {
		return err
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	dbg := openDebug(getenv, o.DataDir)
	defer dbg.Close()
	d := deps{
		load: func(ctx context.Context, req data.Request) (*data.Snapshot, error) {
			return data.Load(ctx, o.DB, o.DataDir, req, time.Now())
		},
		changed: w.Changed,
		live: func(ctx context.Context, runs []run.Run) map[string]tmux.Liveness {
			return data.Liveness(ctx, o.Tmux, runs)
		},
		capture: o.Tmux.PaneView,
		text:    o.Text,
		call:    o.Call,
		task: func(ctx context.Context, id string) (*task.Task, error) {
			return data.Task(ctx, o.DB, id)
		},
		newID: newRequestID,
		now:   time.Now,
		after: func(_ string, d time.Duration, fn func() tea.Msg) tea.Cmd {
			return tea.Tick(d, func(time.Time) tea.Msg { return fn() })
		},
		attachCmd: func(session, pane string) string {
			argv := o.Tmux.AttachArgs(session, pane)
			argv[0] = "tmux"
			return tmux.ShellQuote(argv)
		},
		debug:   dbg.Printf,
		sender:  tmuxSender(o.Tmux, fmt.Sprintf("otag-tui-%d", os.Getpid())),
		prepare: preparer(o),
		exec:    execProcess,
	}
	if d.text == nil {
		d.text = func(context.Context, ...string) (string, error) { return "", errors.New("not available") }
	}
	if d.call == nil {
		d.call = func(context.Context, ...string) Outcome {
			return Outcome{Err: apperr.New(apperr.Unavailable, "actions are not available")}
		}
	}
	m := newModel(ctx, d, o.Project)
	m.hover = getenv(hoverEnv) == "1"
	m.applyState(loadState(statePath(o.DataDir)))
	// The renderer wakes up fps times a second even when nothing changed; 60 (the
	// default) alone costs more CPU than all of the TUI's own work.
	_, err = tea.NewProgram(m, tea.WithContext(ctx), tea.WithFPS(fps)).Run()
	// The layout is written once, here, after the program has ended: a crash loses only
	// what changed since the last start.
	if serr := saveState(statePath(o.DataDir), m.state()); serr != nil {
		fmt.Fprintf(os.Stderr, "otag tui: the layout was not saved: %v\n", serr)
	}
	if errors.Is(err, tea.ErrProgramKilled) && parent.Err() != nil {
		return nil
	}
	return err
}
