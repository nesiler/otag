package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/attach"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/typing"
)

// typedTexts renders the ops the sender got as "lit:x", "key:Enter", "paste:x".
func (h *harness) typedTexts() []string {
	var out []string
	for _, op := range h.typed {
		kind := map[typing.Kind]string{typing.Literal: "lit", typing.Key: "key", typing.Paste: "paste"}[op.Kind]
		out = append(out, kind+":"+op.Text)
	}
	return out
}

func (h *harness) screenText() string {
	h.t.Helper()
	return ansi.Strip(h.m.View().Content)
}

func TestTypingMode(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.key("i")
	if !h.m.typ.on || h.m.typ.pane != "%7" || h.m.typ.run != "al-12-r2" || h.m.focus != focusPane {
		t.Fatalf("typing %+v focus %d", h.m.typ, h.m.focus)
	}
	if s := h.screenText(); !strings.Contains(s, "TYPING → al-12-r2") || !strings.Contains(s, "C-q / M-q / click outside to leave") {
		t.Fatalf("no banner:\n%s", s)
	}
	// Every key goes to the agent, one op each: text, Enter, Esc, Esc Esc (alt+esc),
	// ctrl+c, q and the global keys too.
	h.typeText("hi")
	h.key("enter", "esc", "alt+esc", "ctrl+c", "q", "?", "tab", "1", "shift+enter", "alt+enter", "ctrl+left", "up")
	want := []string{"lit:h", "lit:i", "key:Enter", "key:Escape", "key:M-Escape", "key:C-c", "lit:q", "lit:?",
		"key:Tab", "lit:1", "key:S-Enter", "key:M-Enter", "key:C-Left", "key:Up"}
	if got := h.typedTexts(); !slices.Equal(got, want) {
		t.Fatalf("typed %q\nwant  %q", got, want)
	}
	for _, op := range h.typed {
		if op.Pane != "%7" {
			t.Fatalf("op to %s", op.Pane)
		}
	}
	if !h.m.typ.on || h.quit || h.m.help || h.m.tab != tabs.Preview {
		t.Fatal("a key was taken by the TUI")
	}
	if h.m.typ.pending != 0 || h.m.typ.listening {
		t.Fatalf("results not taken: %+v", h.m.typ)
	}
	// Paste: one op, cleaned.
	h.send(tea.PasteMsg{Content: "a\r\nb\x1b"})
	if got := h.typedTexts(); got[len(got)-1] != "paste:a\nb" {
		t.Fatalf("paste %q", got[len(got)-1])
	}
	// A super key is not sent.
	n := len(h.typed)
	h.send(tea.KeyPressMsg{Code: 'v', Mod: tea.ModSuper})
	if len(h.typed) != n || !strings.Contains(h.m.toast.text, "not sent") {
		t.Fatalf("super key: %d ops, toast %q", len(h.typed)-n, h.m.toast.text)
	}
	// C-q leaves; the keys work again.
	h.key("ctrl+q")
	if h.m.typ.on || len(h.typed) != n {
		t.Fatal("ctrl+q did not leave")
	}
	h.key("i")
	h.key("alt+q")
	if h.m.typ.on || len(h.typed) != n {
		t.Fatal("alt+q did not leave")
	}
	h.key("2")
	if h.m.tab != tabs.Details {
		t.Fatal("keys not back to the TUI")
	}
}

func TestTypingBurstAndRate(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.fire("capture")
	h.key("i")
	h.timers = nil
	h.now = h.now.Add(time.Second)
	h.typeText("a")
	if got := h.pending("capture"); !slices.Equal(got, []time.Duration{burstDelay}) {
		t.Fatalf("burst %v", got)
	}
	// A second key within burstGap starts no other chain.
	h.typeText("b")
	if got := h.pending("capture"); len(got) != 1 {
		t.Fatalf("captures %v", got)
	}
	// While typing the rate never slows to 1 Hz, even with a still pane.
	h.m.cap.still = stillAfter + 1
	if h.m.captureInterval() != captureEvery {
		t.Fatal("typing slowed down")
	}
}

func TestTypingClicks(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.fire("capture")
	// A click on the preview enters typing mode; another click inside is not forwarded.
	x, y := h.at(regPane, "")
	h.click(x, y+3)
	if !h.m.typ.on {
		t.Fatal("click on the preview did not enter typing mode")
	}
	h.now = h.now.Add(time.Second)
	h.click(x, y+3)
	if !h.m.typ.on || len(h.typed) != 0 || len(h.execs) != 0 {
		t.Fatal("click inside the preview")
	}
	// A click outside leaves and does what it does: select a row.
	h.now = h.now.Add(time.Second)
	rx, ry := h.at(regRow, "t:al-13")
	h.click(rx, ry)
	if h.m.typ.on || h.selected() != "t:al-13" {
		t.Fatalf("click outside: typing %v selected %s", h.m.typ.on, h.selected())
	}
	// A double click on the preview attaches.
	h.selectRow("r:al-12-r2")
	h.now = h.now.Add(time.Second)
	h.click(x, y+3)
	h.click(x, y+3)
	if len(h.execs) != 1 || h.m.typ.on || !slices.Equal(h.execs[0], []string{"tmux", "attach", "otag-alpha", "%7"}) {
		t.Fatalf("double click: execs %q typing %v", h.execs, h.m.typ.on)
	}
}

func TestTypingNeedsLivePane(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-9") // done, its run ended
	h.key("i")
	if h.m.typ.on || !strings.Contains(h.m.toast.text, "no live pane") {
		t.Fatalf("typing %v toast %q", h.m.typ.on, h.m.toast.text)
	}
	// From another tab, i shows the preview.
	h.selectRow("r:al-12-r2")
	h.key("2", "i")
	if !h.m.typ.on || h.m.tab != tabs.Preview {
		t.Fatal("i from Details")
	}
	// Another tab ends it (a click on the tabs: outside).
	h.m.setTab(tabs.Stats)
	if h.m.typ.on {
		t.Fatal("typing on another tab")
	}
}

func TestTypingEnds(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.key("i")
	// The pane dies: the next liveness check ends typing mode.
	h.live = map[string]tmux.Liveness{"al-12-r2": {State: tmux.Exited, Pane: "%7"}}
	h.fire("tick")
	if h.m.typ.on || !strings.Contains(h.m.toast.text, "typing ended") {
		t.Fatalf("typing %v toast %q", h.m.typ.on, h.m.toast.text)
	}
	// Any failed send (a gone pane, a timeout, a pane that belongs to another run now)
	// ends it, and nothing after it goes out.
	h.live = map[string]tmux.Liveness{"al-12-r2": {State: tmux.Alive, Pane: "%7", PID: 4242}}
	h.fire("tick")
	h.key("i")
	h.sendErr = func(op typing.Op) error {
		if op.Text == "x" {
			return errors.New("pane %7 now belongs to run be-1-r1, not al-12-r2")
		}
		return nil
	}
	h.typeText("wx")
	if h.m.typ.on || !strings.Contains(h.m.toast.text, "typing stopped, the rest was not sent: pane %7 now belongs") {
		t.Fatalf("typing %v toast %q", h.m.typ.on, h.m.toast.text)
	}
	// The ops carried the run's identity: run, pane pid, generation.
	if op := h.typed[len(h.typed)-1]; op.Run != "al-12-r2" || op.PID != 4242 || op.Pane != "%7" {
		t.Fatalf("op %+v", op)
	}
	// A full queue loses the key: typing mode ends, so no later key (an Enter) follows.
	h.sendErr = nil
	h.key("i")
	h.full = true
	h.typeText("y")
	if h.m.typ.on || !strings.Contains(h.m.toast.text, "a key was lost") || h.m.typ.pending != 0 {
		t.Fatalf("full: %+v %q", h.m.typ, h.m.toast.text)
	}
}

// Leaving typing mode (or the pane's run changing) ends the generation: keys still
// queued are skipped and counted. An attach waits until the queue has drained.
func TestTypingLeaveCancelsAndAttachWaits(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.key("i")
	gen := h.m.typ.gen
	h.typeText("a")
	if h.typed[0].Gen != gen {
		t.Fatalf("op gen %d, want %d", h.typed[0].Gen, gen)
	}
	// A paste and its Enter still queued (the sender holds the Enter 500 ms).
	stale := []typing.Op{{Kind: typing.Paste, Pane: "%7", Text: "p", Gen: gen}, {Kind: typing.Key, Pane: "%7", Text: "Enter", Gen: gen}}
	h.m.typ.pending = 2 // their results are not back (the listen Cmd is not run yet)
	h.key("ctrl+q")
	if h.cancels != 1 || h.m.typ.on {
		t.Fatalf("leaving: %d cancels, typing %v", h.cancels, h.m.typ.on)
	}
	// Enter attaches only once the queue has drained.
	h.holdExec = true
	h.key("enter")
	if len(h.attaches) != 0 || h.m.typ.attachAfter == nil {
		t.Fatalf("attached with keys pending: %+v", h.attaches)
	}
	// The sender skips both (their generation ended); the results drain the queue.
	out := h.m.typ.s.(*fakeSender).out
	out <- typing.Result{Op: stale[0], N: 1, Skipped: true}
	out <- typing.Result{Op: stale[1], N: 1, Skipped: true}
	h.run(h.m.listen())
	if len(h.attaches) != 1 || len(h.execs) != 1 || h.m.typ.attachAfter != nil || h.m.typ.pending != 0 {
		t.Fatalf("no attach after the drain: %+v pending %d", h.attaches, h.m.typ.pending)
	}
	if !strings.Contains(h.m.toast.text, "2 key(s) not sent") {
		t.Fatalf("toast %q", h.m.toast.text)
	}
	h.holdExec = false
	h.run(h.heldExec)
	// A new typing session has a new generation; an op of the old one is skipped.
	h.key("i")
	if h.m.typ.gen == gen {
		t.Fatal("same generation")
	}
	n := len(h.typed)
	h.typeText("b")
	if len(h.typed) != n+1 || h.typed[n].Gen != h.m.typ.gen {
		t.Fatalf("typed %+v", h.typed[n:])
	}
}

// While an attach is being prepared no typing mode starts, and a planned attach still
// waits for keys in flight.
func TestTypingBlockedDuringAttachHandoff(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	tg := attach.Target{Run: "al-12-r2", Session: "otag-alpha", Pane: "%7"}
	prep := h.m.attachTo(tg) // the bindings and the plan are on their way
	h.key("i")
	if h.m.typ.on || !strings.Contains(h.m.toast.text, "an attach is starting") {
		t.Fatalf("typing during the handoff: %v %q", h.m.typ.on, h.m.toast.text)
	}
	h.send(tea.PasteMsg{Content: "x"})
	if len(h.typed) != 0 {
		t.Fatal("a paste was forwarded during the handoff")
	}
	// A key still in flight when the plan arrives: the exec waits for it.
	h.m.typ.s = h.m.d.sender(h.m.ctx)
	h.m.typ.pending = 1
	h.run(prep)
	if len(h.execs) != 0 || h.m.typ.readyAfter == nil {
		t.Fatalf("attached with a key in flight: %q", h.execs)
	}
	h.m.typ.s.(*fakeSender).out <- typing.Result{Op: typing.Op{Kind: typing.Key, Text: "Enter"}, N: 1, Skipped: true}
	h.run(h.m.listen())
	if len(h.execs) != 1 || h.m.typ.readyAfter != nil {
		t.Fatalf("no attach after the drain: %q", h.execs)
	}
}

// A failure of an earlier session's op does not end a newer session.
func TestTypingFailureScopedToGeneration(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.key("i")
	old := h.m.typ.gen
	h.key("ctrl+q", "i")
	if h.m.typ.gen == old || !h.m.typ.on {
		t.Fatal("no new generation")
	}
	out := h.m.typ.s.(*fakeSender).out
	h.m.typ.pending++
	out <- typing.Result{Op: typing.Op{Kind: typing.Literal, Text: "a", Gen: old}, N: 1, Err: errors.New("timeout")}
	h.run(h.m.listen())
	if !h.m.typ.on || h.m.typ.failed {
		t.Fatalf("an old failure ended the new session: on %v failed %v", h.m.typ.on, h.m.typ.failed)
	}
	// This session's failure ends it.
	h.sendErr = func(typing.Op) error { return errors.New("timeout") }
	h.typeText("b")
	if h.m.typ.on || !strings.Contains(h.m.toast.text, "typing stopped") {
		t.Fatalf("on %v toast %q", h.m.typ.on, h.m.toast.text)
	}
}

func TestTypingQueuesDialogs(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.key("i")
	h.m.open(h.m.resultDialog("Late", "late result"))
	if h.m.dlg != nil {
		t.Fatal("a dialog took the keys of typing mode")
	}
	h.key("ctrl+q")
	if h.m.dlg == nil || h.m.dlg.form.Title != "Late" {
		t.Fatal("the queued dialog did not open")
	}
}

func TestTypingCursor(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.fire("capture")
	h.key("i")
	h.m.prev.Set(tmux.PaneView{Width: 80, Height: 3, CursorOn: true, CursorX: 2, CursorY: 1,
		Lines: []string{"agent", "> x", ""}})
	s := layout(h.m)
	c := h.m.View().Cursor
	if c == nil || c.X != s.body.X+2 || c.Y != s.body.Y+1 {
		t.Fatalf("cursor %+v (body %+v)", c, s.body)
	}
	h.key("ctrl+q")
	if h.m.View().Cursor != nil {
		t.Fatal("cursor outside typing mode")
	}
}

func TestAttach(t *testing.T) {
	h := newHarness(t, 120, 40)
	// Enter on a run row attaches: bindings + plan, then the exec.
	h.selectRow("r:al-12-r2")
	h.key("enter")
	if !slices.Equal(h.attaches, []attach.Target{{Run: "al-12-r2", Session: "otag-alpha", Pane: "%7"}}) || len(h.execs) != 1 {
		t.Fatalf("attaches %+v execs %q", h.attaches, h.execs)
	}
	if h.m.attaching {
		t.Fatal("attaching after return")
	}
	// A task row with a live run attaches to it; a project row toggles.
	h.selectRow("t:al-12")
	h.key("enter")
	if len(h.execs) != 2 || h.attaches[1].Pane != "%7" {
		t.Fatalf("task enter: %+v", h.attaches)
	}
	h.selectRow("p:beta")
	h.key("enter")
	if len(h.execs) != 2 || h.m.tree.Selected().Expanded {
		t.Fatal("project enter")
	}
	// Enter in the preview attaches to its run.
	h.selectRow("r:al-14-r1")
	h.key("tab", "enter")
	if len(h.execs) != 3 || h.attaches[2].Pane != "%8" {
		t.Fatalf("preview enter: %+v", h.attaches)
	}
	// c on a project whose coordinator runs attaches to it.
	h.key("shift+tab")
	h.selectRow("p:alpha")
	h.key("c")
	if len(h.execs) != 4 || h.attaches[3].Run != "alpha-c1" || h.selected() != "c:alpha" {
		t.Fatalf("c: %+v selected %s", h.attaches, h.selected())
	}
	// A double click on a run row attaches.
	x, y := h.at(regRow, "r:al-12-r2")
	h.click(x, y)
	h.click(x, y)
	if len(h.execs) != 5 {
		t.Fatalf("double click on a run: %d execs", len(h.execs))
	}
	// No live pane: no attach, a reason.
	h.live = map[string]tmux.Liveness{"al-12-r2": {State: tmux.Gone, Pane: "%7", Detail: "pane not found"}}
	h.fire("tick")
	h.selectRow("r:al-12-r2")
	h.key("enter")
	if len(h.execs) != 5 || !strings.Contains(h.m.toast.text, "attach: pane %7 is gone") {
		t.Fatalf("dead pane: %d execs, toast %q", len(h.execs), h.m.toast.text)
	}
}

func TestAttachReturn(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.fire("capture")
	h.holdExec = true
	h.key("enter")
	if !h.m.attaching {
		t.Fatal("not attaching")
	}
	// A second Enter while attaching does nothing.
	h.key("enter")
	if len(h.attaches) != 1 {
		t.Fatalf("attached twice: %d", len(h.attaches))
	}
	loads, captures := len(h.loads), len(h.capture)
	h.execErr = errors.New("exit status 1")
	h.run(h.heldExec)
	if h.m.attaching || !strings.Contains(h.m.toast.text, "attach al-12-r2 (exec): exit status 1") {
		t.Fatalf("return: attaching %v toast %q", h.m.attaching, h.m.toast.text)
	}
	if len(h.loads)+len(h.pending("load")) == loads || len(h.capture) == captures {
		t.Fatalf("no reload (%d) or capture (%d) after the return", len(h.loads)-loads, len(h.capture)-captures)
	}
}

func TestResultDialogAttachType(t *testing.T) {
	h := newHarness(t, 120, 40)
	started := &run.StartResult{Run: &run.Run{ID: "al-12-r2", Session: &run.Session{TmuxSession: "otag-alpha", TmuxPane: "%7"}}}
	h.result = func(args []string) Outcome { return Outcome{Data: started, Human: "started al-12-r2\n"} }
	h.selectRow("t:al-12")
	h.key("s", "enter")
	if h.m.dlg == nil || h.m.dlg.form.Title != "Run started" {
		t.Fatal("no result dialog")
	}
	if s := h.screenText(); !strings.Contains(s, "[ Attach ]") || !strings.Contains(s, "[ Type ]") {
		t.Fatalf("buttons:\n%s", s)
	}
	h.key("enter") // [Attach] has the focus
	if h.m.dlg != nil || len(h.execs) != 1 || h.attaches[0].Pane != "%7" {
		t.Fatalf("attach: %+v", h.attaches)
	}
	// [Type]: the run's row, the preview, typing mode.
	h.selectRow("t:al-13")
	h.key("s", "enter", "tab", "enter")
	if h.m.dlg != nil || !h.m.typ.on || h.m.typ.pane != "%7" || h.selected() != "r:al-12-r2" {
		t.Fatalf("type: dlg %v typing %+v selected %s", h.m.dlg != nil, h.m.typ, h.selected())
	}
}
