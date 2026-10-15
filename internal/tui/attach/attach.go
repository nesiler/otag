// Package attach plans the TUI's full-screen attach to an agent's window (docs/TUI.md
// "Full-screen attach"): a nested tmux client of otag's server run with
// tea.ExecProcess, or, inside the user's own tmux, a full-screen display-popup that
// runs that client. The return keys (M-q, C-q, the "← otag" status button) detach it.
package attach

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/nesiler/otag/internal/tmux"
)

// Mode is how the TUI attaches.
type Mode int

const (
	Exec  Mode = iota // tmux attach in the TUI's terminal
	Popup             // a display-popup of the user's tmux
)

func (m Mode) String() string {
	if m == Popup {
		return "popup"
	}
	return "exec"
}

// Target is the window to show: the run's tmux session and pane.
type Target struct {
	Run, Session, Pane string
}

// Env is where the TUI runs.
type Env struct {
	TMUX     string   // $TMUX: the user's own tmux server ("" = not inside tmux)
	TMUXPane string   // $TMUX_PANE: the TUI's pane in it
	Outer    string   // the user's tmux binary
	Version  string   // its `tmux -V` ("" unknown: no popup)
	Environ  []string // the TUI's environment
}

// Plan is a command ready to run.
type Plan struct {
	Mode Mode
	Argv []string
	Env  []string
}

// Decide picks the popup inside the user's tmux when it has display-popup -B (3.3),
// else a nested client in the TUI's own terminal.
func Decide(tc *tmux.Client, t Target, e Env) Plan {
	if e.TMUX != "" && e.Outer != "" && tmux.PopupSupported(e.Version) {
		// The popup command goes to the user's server: $TMUX stays.
		return Plan{Mode: Popup, Argv: tc.PopupArgs(e.Outer, e.TMUXPane, t.Session, t.Pane), Env: e.Environ}
	}
	return Plan{Mode: Exec, Argv: tc.AttachArgs(t.Session, t.Pane), Env: withoutTmux(e.Environ)}
}

// Command is the plan as an *exec.Cmd for tea.ExecProcess (which gives it the
// terminal for stdin and stdout). Its stderr goes to stderr, whose text Err adds to a
// failure: tmux prints its errors there, and the TUI's redraw would hide them.
func (p Plan) Command() (*exec.Cmd, *Stderr) {
	c := exec.Command(p.Argv[0], p.Argv[1:]...)
	c.Env = p.Env
	e := &Stderr{}
	c.Stderr = e
	return c, e
}

// Stderr keeps the first 4 KiB written to it.
type Stderr struct{ b []byte }

func (s *Stderr) Write(p []byte) (int, error) {
	if room := 4096 - len(s.b); room > 0 {
		s.b = append(s.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Err is err with the stderr text, if any.
func (s *Stderr) Err(err error) error {
	if err == nil {
		return nil
	}
	if msg := strings.Join(strings.Fields(string(s.b)), " "); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

// withoutTmux drops TMUX and TMUX_PANE: a client of otag's server inside another tmux
// is a nested client, which tmux refuses unless they are unset.
func withoutTmux(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_PANE=") {
			out = append(out, kv)
		}
	}
	return out
}
