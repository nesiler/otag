package tmux

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PaneView is one capture of a pane for the TUI preview: the pane's geometry and
// cursor, and its screen (or the history above it) with SGR escapes.
type PaneView struct {
	Width, Height    int
	CursorX, CursorY int
	CursorOn         bool // cursor_flag: the application shows its cursor
	AltScreen        bool
	InMode           bool // copy or view mode
	Dead             bool
	Lines            []string
}

// viewFormat is printed by display-message before the capture, on a line of its own.
const viewFormat = "#{pane_width}\t#{pane_height}\t#{cursor_x}\t#{cursor_y}\t#{cursor_flag}\t#{alternate_on}\t" +
	"#{pane_in_mode}\t#{pane_dead}"

// ViewTimeout bounds one PaneView call.
const ViewTimeout = time.Second

// PaneView captures pane with one tmux invocation. history 0 is the visible screen,
// row for row (no -J, so rows match the pane grid and the cursor); history n > 0 is
// up to n lines of scrollback above it, wrapped lines joined.
func (c *Client) PaneView(ctx context.Context, pane string, history int) (PaneView, error) {
	ctx, cancel := context.WithTimeout(ctx, ViewTimeout)
	defer cancel()
	capture := []string{"capture-pane", "-p", "-e", "-t", pane}
	if history > 0 {
		capture = []string{"capture-pane", "-p", "-e", "-J", "-S", strconv.Itoa(-history), "-E", "-1", "-t", pane}
	}
	out, err := c.run(ctx, append([]string{"display-message", "-p", "-t", pane, viewFormat, sep}, capture...)...)
	if err != nil {
		return PaneView{}, err
	}
	head, body, _ := strings.Cut(out, "\n")
	v, err := parseView(head)
	if err != nil {
		return PaneView{}, err
	}
	if body != "" {
		v.Lines = strings.Split(body, "\n")
	}
	// The trailing newlines of out were trimmed: blank rows at the bottom of the
	// screen come back, so the screen keeps its height.
	for history == 0 && len(v.Lines) < v.Height {
		v.Lines = append(v.Lines, "")
	}
	return v, nil
}

func parseView(line string) (PaneView, error) {
	f := strings.Split(line, "\t")
	if len(f) != 8 {
		return PaneView{}, fmt.Errorf("tmux display-message: unexpected output %q", line)
	}
	n := make([]int, 4)
	for i := range n {
		v, err := strconv.Atoi(f[i])
		if err != nil {
			return PaneView{}, fmt.Errorf("tmux display-message: unexpected output %q", line)
		}
		n[i] = v
	}
	return PaneView{Width: n[0], Height: n[1], CursorX: n[2], CursorY: n[3], CursorOn: f[4] == "1",
		AltScreen: f[5] == "1", InMode: f[6] == "1", Dead: f[7] == "1"}, nil
}

// OptUI is the server option that turns EnsureUI off: `set -g @otag_ui off`.
const OptUI = "@otag_ui"

// uiCommands are the bindings and status bar that EnsureUI puts on otag's server: the
// return keys M-q and C-q, a clickable "← otag" at the left of the status bar (its
// range=control|0 is the mouse key Control0), and mouse and extended keys on.
var uiCommands = []string{
	"set -g mouse on",
	"set -s extended-keys on",
	"bind -n M-q detach-client",
	"bind -n C-q detach-client",
	"bind -n MouseDown1Control0 detach-client",
	`set -g status-left '#[range=control|0,reverse] ← otag #[norange default] '`,
	`set -g status-right '#{?` + OptRun + `,run #{` + OptRun + `} · ,}M-q / C-q: back to otag '`,
}

// UIScript is the tmux command EnsureUI runs: uiCommands, unless @otag_ui is off.
func UIScript() []string {
	return []string{"if-shell", "-F", "#{!=:#{" + OptUI + "},off}", strings.Join(uiCommands, " ; ")}
}

// EnsureUI applies otag's attach bindings to its server in one invocation, at runtime
// (never written to a file), before every attach: they survive a server restart and
// respect `set -g @otag_ui off` in the user's own otag tmux.conf. Only otag's private
// server (c.Socket) is addressed. A server that is not running is not an error.
func (c *Client) EnsureUI(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := c.run(ctx, UIScript()...)
	if noServer(err) {
		return nil
	}
	return err
}

// Guard is who a pane must still belong to when typed keys reach it: alive, its
// window's @otag_run tag is Run, and its process is PID. With PID 0 the tag must match;
// with a PID an untagged window passes on the PID alone. A restarted server reuses pane
// ids; the guard keeps typed keys from reaching whatever runs in a reused pane.
type Guard struct {
	Pane, Run string
	PID       int
}

// safeName: pane ids, run ids and buffer names put into a tmux format or command
// string; nothing else is ever interpolated there.
var safeName = regexp.MustCompile(`^[%A-Za-z0-9_.-]+$`)

// cond is the guard as a tmux format condition.
func (g Guard) cond() (string, error) {
	if !safeName.MatchString(g.Pane) || !strings.HasPrefix(g.Pane, "%") || (g.Run != "" && !safeName.MatchString(g.Run)) {
		return "", fmt.Errorf("tmux: bad pane %q or run %q", g.Pane, g.Run)
	}
	alive := "#{==:#{pane_dead},0}"
	tag := "#{==:#{" + OptRun + "}," + g.Run + "}"
	if g.PID == 0 {
		if g.Run == "" {
			return "", fmt.Errorf("pane %s: nothing to check that it still belongs to the run", g.Pane)
		}
		return "#{&&:" + alive + "," + tag + "}", nil
	}
	pid := "#{==:#{pane_pid}," + strconv.Itoa(g.PID) + "}"
	tagOK := "#{||:" + tag + ",#{==:#{" + OptRun + "},}}"
	return "#{&&:" + alive + ",#{&&:" + pid + "," + tagOK + "}}", nil
}

// mismatch is what the guard's else branch prints.
const mismatch = "otag-guard-mismatch"

// guarded runs send (a tmux command string of safe words only) when g holds, as one
// tmux invocation: check and send cannot be split by a server restart or another
// client. pre runs first in the same invocation (load-buffer) with stdin; onFail runs
// when g fails (cleanup).
func (c *Client) guarded(ctx context.Context, g Guard, stdin io.Reader, pre []string, send, onFail string) error {
	cond, err := g.cond()
	if err != nil {
		return err
	}
	// A missing pane does not fail if-shell -t; the else branch prints what it saw.
	fail := `display-message -p -t ` + g.Pane + ` '` + mismatch + `	#{pane_id}	#{pane_dead}	#{pane_pid}	#{` + OptRun + `}'`
	if onFail != "" {
		fail = onFail + " ; " + fail
	}
	args := append(append([]string{}, pre...), "if-shell", "-F", "-t", g.Pane, cond, send, fail)
	out, err := c.exec(ctx, "", stdin, args...)
	if err != nil {
		return err
	}
	if rest, ok := strings.CutPrefix(out, mismatch+"\t"); ok {
		f := strings.SplitN(rest, "\t", 4)
		switch {
		case len(f) != 4:
		case f[0] != g.Pane:
			return fmt.Errorf("pane %s is gone", g.Pane)
		case f[1] == "1":
			return fmt.Errorf("pane %s is dead", g.Pane)
		case f[3] != "" && f[3] != g.Run:
			return fmt.Errorf("pane %s now belongs to run %s, not %s", g.Pane, f[3], g.Run)
		case g.PID != 0 && f[2] != strconv.Itoa(g.PID):
			return fmt.Errorf("pane %s now runs pid %s, not %d", g.Pane, f[2], g.PID)
		}
		return fmt.Errorf("pane %s no longer belongs to run %s", g.Pane, g.Run)
	}
	return nil
}

// SendKey sends one key by its tmux name (Enter, C-a, M-;, ...) to g.Pane if g holds.
// The name is quoted for tmux's parser; it is never text the user typed.
func (c *Client) SendKey(ctx context.Context, g Guard, key string) error {
	if key == "" || strings.ContainsAny(key, "\x00\n\r") {
		return fmt.Errorf("tmux: bad key name %q", key)
	}
	return c.guarded(ctx, g, nil, nil, "send-keys -t "+g.Pane+" "+QuoteCommandArg(key), "")
}

// SendText writes text to g.Pane if g holds, as typed characters (bracket: as a paste,
// with bracketed-paste codes when the application asked for them, LF as CR). The text
// goes through a buffer loaded from stdin, never through a command line or a tmux
// command string, so no text can be read as a tmux command, an option or a format, and
// `ps` never shows it. The buffer is deleted either way.
func (c *Client) SendText(ctx context.Context, g Guard, buffer, text string, bracket bool) error {
	if !safeName.MatchString(buffer) {
		return fmt.Errorf("tmux: bad buffer name %q", buffer)
	}
	paste := "paste-buffer -d -r -b " + buffer + " -t " + g.Pane
	if bracket {
		paste = "paste-buffer -p -d -b " + buffer + " -t " + g.Pane
	}
	return c.guarded(ctx, g, strings.NewReader(text), []string{"load-buffer", "-b", buffer, "-", sep}, paste,
		"delete-buffer -b "+buffer)
}

// QuoteCommandArg quotes s as one argument of a tmux command string: single quotes
// (inside them tmux takes everything literally), or double quotes with \ " $ escaped
// when s holds a single quote.
func QuoteCommandArg(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}

// PopupArgs is the command that shows the window of pane in a full-screen popup of
// the user's own tmux (outer: its tmux binary; $TMUX names its server): a nested
// client of otag's server, addressed by socket path, so the popup's environment (the
// outer server's) cannot point it elsewhere. The return keys detach that client and
// -E closes the popup. target, when not empty, is the outer pane the popup belongs to
// ($TMUX_PANE).
func (c *Client) PopupArgs(outer, target, session, pane string) []string {
	inner := []string{"env", "-u", "TMUX", "-u", "TMUX_PANE", c.bin(), "-u", "-S", SocketPath(c.Socket)}
	if pane != "" {
		inner = append(inner, "select-window", "-t", pane, ";")
	}
	inner = append(inner, "attach-session", "-t", "="+session)
	args := []string{outer, "display-popup", "-E", "-B", "-w", "100%", "-h", "100%"}
	if target != "" {
		args = append(args, "-t", target)
	}
	return append(args, ShellQuote(inner))
}

// PopupSupported reports whether a tmux of version v (`tmux -V`: "tmux 3.5a",
// "tmux next-3.6", "tmux master") has display-popup -B (3.3 and later).
func PopupSupported(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "tmux ")
	if v == "master" {
		return true
	}
	major, minor, ok := strings.Cut(strings.TrimPrefix(v, "next-"), ".")
	if !ok {
		return false
	}
	maj, err := strconv.Atoi(major)
	if err != nil {
		return false
	}
	i := 0
	for i < len(minor) && minor[i] >= '0' && minor[i] <= '9' {
		i++
	}
	mi, err := strconv.Atoi(minor[:i])
	if err != nil {
		return false
	}
	return maj > 3 || (maj == 3 && mi >= 3)
}
