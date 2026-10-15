package tui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/fixture"
	"github.com/nesiler/otag/internal/tui/tabs"
)

func TestStartup(t *testing.T) {
	h := newHarness(t, 120, 40)
	if len(h.loads) != 1 || h.m.snap == nil {
		t.Fatalf("loads %d", len(h.loads))
	}
	// The first row is selected; a project row previews its live coordinator.
	if h.selected() != "p:alpha" || h.m.prev.Target.Pane != "%1" {
		t.Fatalf("selected %s, target %+v", h.selected(), h.m.prev.Target)
	}
	if !slices.Equal(h.capture, []string{"%1"}) {
		t.Fatalf("captures %v", h.capture)
	}
	for _, k := range []string{"tick", "watch", "capture"} {
		if len(h.pending(k)) == 0 {
			t.Errorf("no %s timer", k)
		}
	}
}

func TestInitialProject(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.m.project = "beta"
	h.m.snap = nil
	h.send(loadMsg{snap: fixture.Snapshot(false)})
	if h.selected() != "p:beta" {
		t.Fatalf("selected %s", h.selected())
	}
}

func TestSelectionSurvivesReload(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.m.tree.Select("t:al-13")
	h.snap = func() *data.Snapshot {
		s := fixture.Snapshot(false)
		s.Open = append([]task.Task{s.Open[2]}, s.Open...)
		s.Open[0].ID = "al-1"
		return s
	}
	h.changed = true
	h.fire("watch")
	h.fire("load")
	if len(h.loads) != 2 {
		t.Fatalf("loads %d", len(h.loads))
	}
	if h.selected() != "t:al-13" {
		t.Fatalf("selected %s", h.selected())
	}
}

func TestDataLoop(t *testing.T) {
	h := newHarness(t, 120, 40)
	// An unchanged data_version loads nothing and draws nothing.
	h.m.frameValid = true
	h.fire("watch")
	if len(h.loads) != 1 || !h.m.frameValid || len(h.pending("watch")) != 1 {
		t.Fatalf("loads %d, frame %v, watch %v", len(h.loads), h.m.frameValid, h.pending("watch"))
	}
	// A change loads, at least loadGap after the previous load.
	h.changed = true
	h.fire("watch")
	if d := h.pending("load"); len(d) != 1 || d[0] != loadGap {
		t.Fatalf("load timers %v", d)
	}
	// Changes seen during a load load once more afterwards.
	h.m.requestLoad()
	h.m.requestLoad()
	if !h.m.loading.dirty {
		t.Fatal("not dirty")
	}
	h.now = h.now.Add(time.Second)
	h.fire("load") // the first; the dirty one follows at once (loadGap has passed)
	if len(h.loads) < 3 || h.m.loading.inFlight {
		t.Fatalf("loads %d in flight %v", len(h.loads), h.m.loading.inFlight)
	}
	// The safety-net reload.
	n := len(h.loads)
	h.now = h.now.Add(time.Second)
	h.fire("tick") // full reload, liveness and clock ticks
	h.fire("load")
	if len(h.loads) != n+1 {
		t.Fatalf("full reload: %d loads", len(h.loads)-n)
	}
}

func TestPreviewLoop(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("j", "j", "j", "j", "j", "j", "j", "j") // to al-12-r2
	if h.selected() != "r:al-12-r2" || h.m.prev.Target.Pane != "%7" {
		t.Fatalf("selected %s target %+v", h.selected(), h.m.prev.Target)
	}
	h.capture = nil
	h.fire("capture") // the coordinator's scheduled capture (its loop ended) and the next of %7
	if !slices.Equal(h.capture, []string{"%7"}) || !slices.Equal(h.pending("capture"), []time.Duration{captureEvery}) {
		t.Fatalf("captures %v, pending %v", h.capture, h.pending("capture"))
	}
	// Another tab: no capture runs, even the one already scheduled.
	h.capture = nil
	h.key("3")
	h.fire("capture")
	if len(h.capture) != 0 || len(h.pending("capture")) != 0 {
		t.Fatalf("captures on the Needs tab: %v %v", h.capture, h.pending("capture"))
	}
	// Back: a capture at once, one loop only.
	h.key("1")
	h.key("1")
	if !slices.Equal(h.capture, []string{"%7"}) || len(h.pending("capture")) != 1 {
		t.Fatalf("back on Preview: %v %v", h.capture, h.pending("capture"))
	}
	// An unchanged pane slows to 1 Hz after stillAfter captures; a change speeds up.
	for range stillAfter {
		h.fire("capture")
	}
	if p := h.pending("capture"); !slices.Equal(p, []time.Duration{captureSlow}) {
		t.Fatalf("still pane: %v", p)
	}
	h.screen = []string{"new output"}
	h.fire("capture")
	if p := h.pending("capture"); !slices.Equal(p, []time.Duration{captureEvery}) {
		t.Fatalf("changed pane: %v", p)
	}
	// No focus: 1 Hz.
	h.send(tea.BlurMsg{})
	h.fire("capture")
	if p := h.pending("capture"); !slices.Equal(p, []time.Duration{captureSlow}) {
		t.Fatalf("blurred: %v", p)
	}
	// A row without a live pane: no capture.
	h.capture = nil
	h.selectRow("t:al-13")
	h.fire("capture")
	if h.m.prev.Target.Pane != "" || len(h.capture) != 0 {
		t.Fatalf("target %+v captures %v", h.m.prev.Target, h.capture)
	}
}

func TestPreviewGonePane(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	if h.m.prev.Target.Pane != "%7" {
		t.Fatal("no target")
	}
	h.live = map[string]tmux.Liveness{"al-12-r2": {State: tmux.Gone, Detail: "pane not found"}}
	h.fire("tick") // the liveness tick
	if h.m.prev.Target.Pane != "" {
		t.Fatalf("target kept for a gone pane: %+v", h.m.prev.Target)
	}
	r, _ := h.m.target()
	if _, why := h.m.previewable(r); why != "pane %7 is gone (pane not found)" {
		t.Fatalf("why %q", why)
	}
}

func TestScrollLock(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.screen = make([]string, 50)
	for i := range h.screen {
		h.screen[i] = fmt.Sprintf("line %d", i)
	}
	h.fire("capture")
	h.key("tab") // the right pane
	h.capture = nil
	h.key("up")
	if !slices.Equal(h.capture, []string{"h:%1"}) || !h.m.prev.Locked || h.m.prev.Scroll != 1 {
		t.Fatalf("captures %v locked %v scroll %d", h.capture, h.m.prev.Locked, h.m.prev.Scroll)
	}
	h.capture = nil
	h.fire("capture")
	if len(h.capture) != 0 {
		t.Fatalf("live captures while locked: %v", h.capture)
	}
	h.key("down") // back at the bottom: follow
	if h.m.prev.Locked || !slices.Equal(h.capture, []string{"%1"}) {
		t.Fatalf("follow: locked %v captures %v", h.m.prev.Locked, h.capture)
	}
	h.key("pgup", "G")
	if h.m.prev.Locked {
		t.Fatal("G did not follow")
	}
	h.key("right")
	if h.m.prev.XOff != 0 { // nothing wider than the pane
		t.Fatalf("xoff %d", h.m.prev.XOff)
	}
}

func TestTreeKeys(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("G")
	if h.selected() != "t:be-1" {
		t.Fatalf("G: %s", h.selected())
	}
	h.key("h") // no children: to the parent
	if h.selected() != "p:beta" {
		t.Fatalf("h: %s", h.selected())
	}
	h.key("h") // collapse
	if h.m.tree.Selected().Expanded || h.m.tree.Rows[len(h.m.tree.Rows)-1].Key != "p:beta" {
		t.Fatal("not collapsed")
	}
	h.key("l")
	if !h.m.tree.Selected().Expanded {
		t.Fatal("not expanded")
	}
	h.key("space")
	if h.m.tree.Selected().Expanded {
		t.Fatal("space did not toggle")
	}
	h.key("enter")
	if !h.m.tree.Selected().Expanded {
		t.Fatal("enter did not toggle")
	}
	h.key("g", "down", "pgdown")
	if h.selected() != "t:be-1" {
		t.Fatalf("pgdown: %s", h.selected())
	}
	h.key("home", "end", "pgup", "up")
	if h.selected() != "p:alpha" {
		t.Fatalf("pgup up: %s", h.selected())
	}
	// "+1 earlier" loads the task's history.
	h.m.tree.Select("h:al-12")
	h.key("l")
	h.fire("load")
	if last := h.loads[len(h.loads)-1]; !slices.Equal(last.History, []string{"al-12"}) {
		t.Fatalf("history request %+v", last)
	}
	// Closed tasks: a load with ShowClosed.
	h.key(".")
	h.fire("load")
	if last := h.loads[len(h.loads)-1]; !last.ShowClosed {
		t.Fatalf("closed request %+v", last)
	}
}

func TestGlobalKeys(t *testing.T) {
	h := newHarness(t, 120, 40)
	for k, tab := range map[string]tabs.Tab{"2": tabs.Details, "3": tabs.Needs, "4": tabs.Inbox, "5": tabs.Stats,
		"6": tabs.Log, "!": tabs.Needs, "1": tabs.Preview} {
		h.key(k)
		if h.m.tab != tab {
			t.Errorf("%s: tab %d", k, h.m.tab)
		}
	}
	h.key("tab")
	if h.m.focus != focusPane {
		t.Fatal("tab: focus")
	}
	h.key("shift+tab")
	if h.m.focus != focusTree {
		t.Fatal("shift+tab: focus")
	}
	w := treeWidth(0, 120)
	h.key(">")
	if h.m.divider != w+dividerStep {
		t.Fatalf("> %d", h.m.divider)
	}
	h.key("<", "<")
	if h.m.divider != w-dividerStep {
		t.Fatalf("< %d", h.m.divider)
	}
	h.key("=")
	if h.m.divider != 0 {
		t.Fatal("=")
	}
	h.key("?")
	if !h.m.help {
		t.Fatal("?")
	}
	h.key("j") // the help takes the keys
	if h.selected() != "p:alpha" || !h.m.help {
		t.Fatal("help let a key through")
	}
	h.key("esc")
	if h.m.help {
		t.Fatal("esc")
	}
	n := len(h.loads)
	h.key("ctrl+r")
	h.fire("load")
	h.key("f5")
	h.fire("load")
	if len(h.loads) != n+2 {
		t.Fatalf("reload keys: %d loads", len(h.loads)-n)
	}
	h.key("ctrl+l")
	h.key("q")
	if !h.quit {
		t.Fatal("q")
	}
	h.quit = false
	h.key("ctrl+c")
	if !h.quit {
		t.Fatal("ctrl+c")
	}
}

// Every key of the table reaches its binding: none is shadowed by another.
func TestBindingTable(t *testing.T) {
	for _, b := range bindings {
		for _, k := range b.Keys() {
			got, ok := lookup(keyMsg(k), b.scope)
			if !ok || got.act != b.act || got.arg != b.arg || got.scope != b.scope && got.scope != scopeGlobal {
				t.Errorf("%q in scope %d: %+v %v", k, b.scope, got, ok)
			}
		}
	}
	if _, ok := lookup(keyMsg("w"), scopeTree); ok {
		t.Error("w is bound")
	}
}

func TestTextTabs(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("2")
	h.selectRow("r:al-12-r2")
	h.selectRow("t:al-13")
	h.selectRow("p:beta")
	want := [][]string{{"--project", "alpha", "status"}, {"run", "show", "al-12-r2"}, {"task", "show", "al-13"},
		{"--project", "beta", "status"}}
	if !slices.ContainsFunc(h.texts, func(a []string) bool { return slices.Equal(a, want[1]) }) ||
		!slices.ContainsFunc(h.texts, func(a []string) bool { return slices.Equal(a, want[2]) }) ||
		!slices.Equal(h.texts[len(h.texts)-1], want[3]) || !slices.Equal(h.texts[0], want[0]) {
		t.Fatalf("texts %v", h.texts)
	}
	if tv := h.m.text[tabs.Details]; tv.text != "output of --project beta status" || tv.inFlight {
		t.Fatalf("details %+v", tv)
	}
	h.texts = nil
	h.key("5")
	if !reflect.DeepEqual(h.texts, [][]string{{"stats"}, {"quota"}}) {
		t.Fatalf("stats %v", h.texts)
	}
	h.key("1", "5") // cached
	if len(h.texts) != 2 {
		t.Fatalf("stats fetched again: %v", h.texts)
	}
	h.key("ctrl+r")
	if len(h.texts) != 4 {
		t.Fatalf("ctrl+r: %v", h.texts)
	}
}

func TestNarrow(t *testing.T) {
	h := newHarness(t, 80, 24)
	s := layout(h.m)
	if !s.narrow || !s.showTree || s.pane.W != 0 {
		t.Fatalf("narrow layout %+v", s)
	}
	if len(h.capture) != 0 {
		t.Fatalf("captured with the preview hidden: %v", h.capture)
	}
	h.key("tab")
	if s := layout(h.m); s.showTree || s.pane.W != 80 || !h.m.paneFocused() {
		t.Fatalf("swap: %+v", s)
	}
	if !slices.Equal(h.capture, []string{"%1"}) {
		t.Fatalf("captures %v", h.capture)
	}
	h.key("shift+tab")
	if !layout(h.m).showTree {
		t.Fatal("swap back")
	}
	// The ring goes on to the message box, which shows in narrow mode too.
	h.key("tab", "tab")
	if h.m.focus != focusBox || !layout(h.m).narrow {
		t.Fatalf("focus %v", h.m.focus)
	}
	h.key("tab")
	if h.m.focus != focusTree || !layout(h.m).showTree {
		t.Fatalf("back to the tree: focus %v", h.m.focus)
	}
	h.send(tea.WindowSizeMsg{Width: 59, Height: 24})
	if s := layout(h.m); !s.tooSmall || len(s.regions) != 1 {
		t.Fatalf("too small %+v", s)
	}
}

// G while the scrollback fetch is in flight: its late result must not lock the view.
func TestFollowDropsPendingScrollback(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.screen = make([]string, 50)
	for i := range h.screen {
		h.screen[i] = fmt.Sprintf("line %d", i)
	}
	h.fire("capture")
	h.key("tab")
	_, fetch := h.m.Update(keyMsg("up")) // the scrollback request, not run yet
	if h.m.cap.lockLines == 0 || h.m.cap.busy {
		t.Fatalf("request: lockLines %d busy %v", h.m.cap.lockLines, h.m.cap.busy)
	}
	h.key("G")
	if h.m.cap.lockLines != 0 || !h.m.cap.busy {
		t.Fatalf("follow: lockLines %d busy %v", h.m.cap.lockLines, h.m.cap.busy)
	}
	h.run(fetch)
	if h.m.prev.Locked {
		t.Fatal("a stale scrollback locked the view")
	}
	// A new request after that works.
	h.key("up")
	if !h.m.prev.Locked {
		t.Fatal("second request")
	}
}

// Every read job runs with a deadline under the TUI's context.
func TestJobsHaveDeadlines(t *testing.T) {
	h := newHarness(t, 120, 40)
	var seen []string
	check := func(name string, max time.Duration) func(context.Context) {
		return func(ctx context.Context) {
			dl, ok := ctx.Deadline()
			if !ok || time.Until(dl) > max {
				t.Errorf("%s: deadline %v %v", name, dl, ok)
			}
			seen = append(seen, name)
		}
	}
	d := &h.m.d
	load, changed, live, capture, text := d.load, d.changed, d.live, d.capture, d.text
	d.load = func(ctx context.Context, r data.Request) (*data.Snapshot, error) {
		check("load", loadTimeout)(ctx)
		return load(ctx, r)
	}
	d.changed = func(ctx context.Context) (bool, error) { check("watch", watchTimeout)(ctx); return changed(ctx) }
	d.live = func(ctx context.Context, rs []run.Run) map[string]tmux.Liveness {
		check("live", liveTimeout)(ctx)
		return live(ctx, rs)
	}
	d.capture = func(ctx context.Context, p string, n int) (tmux.PaneView, error) {
		check("capture", captureTimeout)(ctx)
		return capture(ctx, p, n)
	}
	d.text = func(ctx context.Context, a ...string) (string, error) {
		check("text", textTimeout)(ctx)
		return text(ctx, a...)
	}
	h.changed = true
	h.fire("") // watch (a change), liveness, clock, full reload, capture
	h.fire("load")
	h.key("2")
	for _, name := range []string{"watch", "load", "live", "capture", "text"} {
		if !slices.Contains(seen, name) {
			t.Errorf("%s did not run (%v)", name, seen)
		}
	}

	// The jobs end with the TUI.
	ctx, cancel := context.WithCancel(context.Background())
	m := newModel(ctx, h.m.d, "")
	cancel()
	jc, jcancel := m.job(time.Hour)
	defer jcancel()
	if jc.Err() == nil {
		t.Fatal("a job outlives the TUI's context")
	}
}
