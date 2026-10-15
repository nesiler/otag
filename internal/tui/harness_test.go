package tui

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/attach"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/fixture"
	"github.com/nesiler/otag/internal/tui/typing"
)

// timer is a pending deps.after: the test decides when it fires.
type timer struct {
	kind string // tick | watch | load | capture
	d    time.Duration
	fn   func() tea.Msg
}

// harness drives a Model with fake side effects, synchronously: Cmds run in the
// test's goroutine, timers wait until fired.
type harness struct {
	t       *testing.T
	m       *Model
	now     time.Time
	snap    func() *data.Snapshot
	live    map[string]tmux.Liveness
	screen  []string // what capture returns
	loads   []data.Request
	capture []string // panes captured live; "h:<pane>" for scrollback
	texts   [][]string
	changed bool
	timers  []timer
	quit    bool
	// Actions: calls has the arguments of each Options.Call (reqs their request
	// ids); result answers them; with hold set, outcomes wait in held for release.
	calls  [][]string
	reqs   []string
	result func(args []string) Outcome
	hold   bool
	held   []tea.Msg
	ids    int
	// Typing and attach: typed has the ops the sender got (sendErr answers them);
	// attaches the planned targets, execs the command lines run (execErr answers).
	typed    []typing.Op
	sendErr  func(op typing.Op) error
	full     bool // the sender's queue is full
	cancels  int  // Cancel calls
	attaches []attach.Target
	execs    [][]string
	execErr  error
	holdExec bool    // the attach's exec waits in heldExec
	heldExec tea.Cmd // ...until runExec
	plan     attach.Mode
	// T4: the debug log.
	logs []string
}

// fakeSender answers each op at once, like typing.Sender: an op of an ended
// generation is skipped, a failure ends the generation.
type fakeSender struct {
	h   *harness
	out chan typing.Result
	gen uint64
}

func (s *fakeSender) Send(op typing.Op) bool {
	if s.h.full {
		return false
	}
	if op.Gen != s.gen {
		s.out <- typing.Result{Op: op, N: 1, Skipped: true}
		return true
	}
	s.h.typed = append(s.h.typed, op)
	var err error
	if s.h.sendErr != nil {
		if err = s.h.sendErr(op); err != nil {
			s.gen++
		}
	}
	s.out <- typing.Result{Op: op, N: 1, Err: err}
	return true
}

func (s *fakeSender) Results() <-chan typing.Result { return s.out }
func (s *fakeSender) Gen() uint64                   { return s.gen }
func (s *fakeSender) Cancel() uint64                { s.gen++; s.h.cancels++; return s.gen }

func newHarness(t *testing.T, w, h int) *harness {
	t.Helper()
	hs := &harness{t: t, now: fixture.Now, live: fixture.Live(), screen: []string{"agent output", "> "},
		snap: func() *data.Snapshot { return fixture.Snapshot(false) }}
	d := deps{
		load: func(_ context.Context, req data.Request) (*data.Snapshot, error) {
			hs.loads = append(hs.loads, req)
			return hs.snap(), nil
		},
		changed: func(context.Context) (bool, error) { c := hs.changed; hs.changed = false; return c, nil },
		live:    func(context.Context, []run.Run) map[string]tmux.Liveness { return hs.live },
		capture: func(_ context.Context, pane string, history int) (tmux.PaneView, error) {
			if history > 0 {
				hs.capture = append(hs.capture, "h:"+pane)
				return tmux.PaneView{Lines: []string{"old 1", "old 2"}}, nil
			}
			hs.capture = append(hs.capture, pane)
			return tmux.PaneView{Width: 80, Height: len(hs.screen), Lines: append([]string(nil), hs.screen...)}, nil
		},
		text: func(_ context.Context, args ...string) (string, error) {
			hs.texts = append(hs.texts, args)
			return "output of " + strings.Join(args, " "), nil
		},
		call: func(_ context.Context, args ...string) Outcome {
			if len(args) < 2 || args[0] != "--request-id" {
				hs.t.Errorf("call without a request id: %q", args)
				return Outcome{}
			}
			hs.reqs = append(hs.reqs, args[1])
			hs.calls = append(hs.calls, args[2:])
			if hs.result != nil {
				return hs.result(args[2:])
			}
			return Outcome{Human: "ok: " + strings.Join(args[2:], " ") + "\n"}
		},
		task: func(_ context.Context, id string) (*task.Task, error) {
			for _, t := range hs.snap().Open {
				if t.ID == id {
					t.Version = 7
					t.Body = "the body"
					t.Deps = []task.Dep{{Kind: "blocks", TaskID: "al-16"}}
					return &t, nil
				}
			}
			return nil, apperr.New(apperr.NotFound, "task %s not found", id)
		},
		newID: func() string { hs.ids++; return fmt.Sprintf("tui-test-%d", hs.ids) },
		now:   func() time.Time { return hs.now },
		after: func(kind string, d time.Duration, fn func() tea.Msg) tea.Cmd {
			return func() tea.Msg { return timer{kind, d, fn} }
		},
		sender: func(context.Context) keySender { return &fakeSender{h: hs, out: make(chan typing.Result, 1024)} },
		prepare: func(_ context.Context, t attach.Target) (attach.Plan, error) {
			hs.attaches = append(hs.attaches, t)
			return attach.Plan{Mode: hs.plan, Argv: []string{"tmux", "attach", t.Session, t.Pane}}, nil
		},
		attachCmd: func(session, pane string) string { return "tmux attach " + session + " " + pane },
		debug:     func(f string, a ...any) { hs.logs = append(hs.logs, fmt.Sprintf(f, a...)) },
		exec: func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
			hs.execs = append(hs.execs, c.Args)
			cmd := func() tea.Msg { return fn(hs.execErr) }
			if hs.holdExec {
				hs.heldExec = cmd
				return nil
			}
			return cmd
		},
	}
	hs.m = newModel(context.Background(), d, "")
	hs.run(hs.m.Init())
	hs.send(tea.WindowSizeMsg{Width: w, Height: h})
	return hs
}

// send delivers msg and runs what it returns.
func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	_, cmd := h.m.Update(msg)
	h.run(cmd)
}

// run executes cmd and delivers its messages; timers are kept for fire.
func (h *harness) run(cmd tea.Cmd) {
	h.t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			h.run(c)
		}
	case timer:
		h.timers = append(h.timers, msg)
	case tea.QuitMsg:
		h.quit = true
	case outcomeMsg:
		if h.hold {
			h.held = append(h.held, msg)
			return
		}
		h.send(msg)
	default:
		if reflect.TypeOf(msg).PkgPath() != "github.com/nesiler/otag/internal/tui" {
			return // tea's own (clear screen, ...)
		}
		h.send(msg)
	}
}

// fire fires the pending timers of kind ("" = all) and returns how many fired.
// Timers they schedule wait for the next fire.
func (h *harness) fire(kind string) int {
	h.t.Helper()
	pending := h.timers
	h.timers = nil
	n := 0
	for _, tm := range pending {
		if kind != "" && tm.kind != kind {
			h.timers = append(h.timers, tm)
			continue
		}
		n++
		h.send(tm.fn())
	}
	return n
}

// pending returns the delays of the pending timers of kind.
func (h *harness) pending(kind string) []time.Duration {
	var out []time.Duration
	for _, tm := range h.timers {
		if tm.kind == kind {
			out = append(out, tm.d)
		}
	}
	return out
}

// key presses a key given as tea writes it ("j", "ctrl+r", "shift+tab", "pgdown").
func (h *harness) key(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		h.send(keyMsg(k))
	}
}

var named = map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"home": tea.KeyHome, "end": tea.KeyEnd, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "tab": tea.KeyTab,
	"enter": tea.KeyEnter, "esc": tea.KeyEscape, "space": tea.KeySpace, "f5": tea.KeyF5, "f10": tea.KeyF10, "menu": tea.KeyMenu, "backspace": tea.KeyBackspace}

func keyMsg(s string) tea.KeyPressMsg {
	var mod tea.KeyMod
	for {
		switch {
		case strings.HasPrefix(s, "ctrl+") && len(s) > 5:
			mod |= tea.ModCtrl
			s = s[5:]
			continue
		case strings.HasPrefix(s, "alt+") && len(s) > 4:
			mod |= tea.ModAlt
			s = s[4:]
			continue
		case strings.HasPrefix(s, "shift+") && len(s) > 6:
			mod |= tea.ModShift
			s = s[6:]
			continue
		}
		break
	}
	if c, ok := named[s]; ok {
		k := tea.KeyPressMsg{Code: c, Mod: mod}
		if c == tea.KeySpace && mod == 0 {
			k.Text = " "
		}
		return k
	}
	r := []rune(s)[0]
	if mod != 0 {
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	return tea.KeyPressMsg{Code: r, Text: s}
}

// click is a left click (press) at x, y; release follows.
func (h *harness) click(x, y int) {
	h.t.Helper()
	h.send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.send(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

// at returns a cell inside the region of kind k with id ("" = any).
func (h *harness) at(k regionKind, id string) (int, int) {
	h.t.Helper()
	for _, r := range layout(h.m).regions {
		if r.kind == k && (id == "" || r.id == id) {
			return r.rect.X + r.rect.W/2, r.rect.Y
		}
	}
	h.t.Fatalf("no region %d %q", k, id)
	return 0, 0
}

func (h *harness) selected() string { return h.m.tree.Cursor() }

// release delivers the held outcomes.
func (h *harness) release() {
	h.t.Helper()
	held := h.held
	h.held, h.hold = nil, false
	for _, msg := range held {
		h.send(msg)
	}
}

// typeText types s as key presses.
func (h *harness) typeText(s string) {
	h.t.Helper()
	for _, r := range s {
		if r == ' ' {
			h.key("space")
			continue
		}
		h.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// lastCall returns the arguments of the latest Options.Call.
func (h *harness) lastCall() []string {
	h.t.Helper()
	if len(h.calls) == 0 {
		h.t.Fatal("no call")
	}
	return h.calls[len(h.calls)-1]
}

// selectRow selects key as a key press would.
func (h *harness) selectRow(key string) {
	h.t.Helper()
	if !h.m.tree.Select(key) {
		h.t.Fatalf("no row %s", key)
	}
	h.m.frameValid = false
	h.run(h.m.selected())
}
