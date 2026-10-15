package tui

import (
	"strconv"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

func TestClickSelects(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("tab")
	x, y := h.at(regRow, "t:al-13")
	h.click(x, y)
	if h.selected() != "t:al-13" || h.m.focus != focusTree {
		t.Fatalf("selected %s focus %d", h.selected(), h.m.focus)
	}
	// A click on the arrow toggles the row.
	x, y = h.at(regToggle, "t:al-12")
	h.click(x, y)
	if h.selected() != "t:al-12" || h.m.tree.Selected().Expanded {
		t.Fatalf("toggle: %s %v", h.selected(), h.m.tree.Selected().Expanded)
	}
	h.click(x, y)
	if !h.m.tree.Selected().Expanded {
		t.Fatal("toggle back")
	}
}

func TestDoubleClick(t *testing.T) {
	h := newHarness(t, 120, 40)
	x, y := h.at(regRow, "p:beta")
	h.click(x, y)
	h.now = h.now.Add(300 * time.Millisecond)
	h.click(x+1, y) // within 400 ms and one cell: a double click
	if h.m.tree.Selected().Expanded {
		t.Fatal("double click did not collapse")
	}
	// A third click starts over; too slow is two single clicks.
	h.now = h.now.Add(100 * time.Millisecond)
	h.click(x, y)
	h.now = h.now.Add(500 * time.Millisecond)
	h.click(x, y)
	if h.m.tree.Selected().Expanded {
		t.Fatal("slow clicks toggled")
	}
	// Two clicks far apart are not a double click.
	h.now = h.now.Add(time.Second)
	h.click(x, y)
	h.now = h.now.Add(100 * time.Millisecond)
	h.click(x+5, y)
	if h.m.tree.Selected().Expanded {
		t.Fatal("distant clicks toggled")
	}
	// A double click on a run row attaches instead of toggling (TestAttach).
	h.selectRow("p:alpha")
	x, y = h.at(regRow, "r:al-12-r2")
	h.click(x, y)
	h.click(x, y)
	if h.selected() != "r:al-12-r2" {
		t.Fatalf("selected %s", h.selected())
	}
}

func TestClickRegions(t *testing.T) {
	h := newHarness(t, 120, 40)
	x, y := h.at(regTab, strconv.Itoa(int(tabs.Stats)))
	h.click(x, y)
	if h.m.tab != tabs.Stats || h.m.focus != focusPane {
		t.Fatalf("tab click: %d %d", h.m.tab, h.m.focus)
	}
	x, y = h.at(regCounter, "needs")
	h.click(x, y)
	if h.m.tab != tabs.Needs {
		t.Fatal("needs counter")
	}
	x, y = h.at(regCounter, "inbox")
	h.click(x, y)
	if h.m.tab != tabs.Inbox {
		t.Fatal("inbox counter")
	}
	x, y = h.at(regCounter, "deliver")
	h.click(x, y)
	if h.selected() != "g:deliver:alpha" || h.m.focus != focusTree {
		t.Fatalf("deliver counter: %s", h.selected())
	}
	// Footer buttons run their binding; any click closes the help.
	for i, b := range hints() {
		if b.act == actHelp {
			x, y = h.at(regHint, strconv.Itoa(i))
		}
	}
	h.click(x, y)
	if !h.m.help {
		t.Fatal("help button")
	}
	h.click(0, 0)
	if h.m.help {
		t.Fatal("click did not close the help")
	}
	// The middle button does nothing (the right one opens a menu: TestMenu).
	sel := h.selected()
	x, y = h.at(regRow, "t:al-13")
	h.send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseMiddle})
	if h.selected() != sel || h.m.menu != nil {
		t.Fatal("middle click did something")
	}
}

func TestWheelRouting(t *testing.T) {
	h := newHarness(t, 120, 18) // 14 tree rows (below them the message box) for 15 rows of content
	x, y := h.at(regRow, "p:alpha")
	h.send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	if h.m.tree.Off != 1 || h.selected() != "p:alpha" {
		t.Fatalf("tree wheel: off %d selected %s", h.m.tree.Off, h.selected())
	}
	h.send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
	if h.m.tree.Off != 0 {
		t.Fatalf("tree wheel up: off %d", h.m.tree.Off)
	}
	// Over the preview: scroll-lock, the tree does not move.
	h.capture = nil
	x, y = h.at(regPane, "")
	h.send(tea.MouseWheelMsg{X: x, Y: y + 2, Button: tea.MouseWheelUp})
	if len(h.capture) != 1 || h.capture[0] != "h:%1" || h.m.tree.Off != 0 {
		t.Fatalf("pane wheel: captures %v off %d", h.capture, h.m.tree.Off)
	}
	// Over a text tab: its scroll.
	h.key("3")
	h.m.scroll[tabs.Needs] = 0
	h.send(tea.MouseWheelMsg{X: x, Y: y + 2, Button: tea.MouseWheelDown})
	if h.m.scroll[tabs.Needs] != 0 { // Needs fits: nothing to scroll
		t.Fatalf("needs scroll %d", h.m.scroll[tabs.Needs])
	}
}

func TestDividerDrag(t *testing.T) {
	h := newHarness(t, 120, 40)
	x, y := h.at(regDivider, "")
	h.send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.send(tea.MouseMotionMsg{X: 60, Y: y, Button: tea.MouseLeft})
	if h.m.divider != 60 {
		t.Fatalf("drag to 60: %d", h.m.divider)
	}
	h.send(tea.MouseMotionMsg{X: 5, Y: y, Button: tea.MouseLeft})
	if h.m.divider != minTree {
		t.Fatalf("drag to 5: %d", h.m.divider)
	}
	h.send(tea.MouseMotionMsg{X: 119, Y: y, Button: tea.MouseLeft})
	if h.m.divider != 120-minPane-1 {
		t.Fatalf("drag to 119: %d", h.m.divider)
	}
	h.send(tea.MouseReleaseMsg{X: 119, Y: y, Button: tea.MouseLeft})
	h.send(tea.MouseMotionMsg{X: 50, Y: y})
	if h.m.divider != 120-minPane-1 {
		t.Fatalf("moved after release: %d", h.m.divider)
	}
	if r, _ := layout(h.m).find(regDivider); r.rect.X != 89 {
		t.Fatalf("divider at %d", r.rect.X)
	}
}

func TestLayoutHit(t *testing.T) {
	h := newHarness(t, 120, 40)
	s := layout(h.m)
	// The blocks tile the screen: every cell is in exactly one.
	for y := range 40 {
		for x := range 120 {
			n := 0
			for _, r := range s.regions {
				if r.z == 0 && r.rect.Contains(x, y) {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("cell %d,%d in %d blocks", x, y, n)
			}
		}
	}
	// Topmost first: the arrow over its row, the row over the tree.
	tx, ty := h.at(regToggle, "p:alpha")
	if r, _ := s.hit(tx, ty); r.kind != regToggle {
		t.Fatalf("toggle hit %+v", r)
	}
	if r, _ := s.hit(tx+10, ty); r.kind != regRow || r.id != "p:alpha" {
		t.Fatalf("row hit %+v", r)
	}
	if r, _ := s.hit(5, 35); r.kind != regTree {
		t.Fatalf("empty tree hit %+v", r)
	}
	// The help overlay covers everything under it.
	h.key("?")
	s = layout(h.m)
	if r, _ := s.hit(60, 20); r.kind != regHelp {
		t.Fatalf("help hit %+v", r)
	}
}

// A row nested deeper than the tree is wide has its arrow outside the tree: no toggle
// region may reach over the divider or the preview.
func TestToggleClipped(t *testing.T) {
	h := newHarness(t, 120, 40)
	tw := treeWidth(0, 120)
	h.m.tree.Rows = append(h.m.tree.Rows,
		tree.Row{Key: "t:edge", Kind: tree.Task, Depth: (tw - 1) / 2, CanExpand: true}, // arrow half inside
		tree.Row{Key: "t:deep", Kind: tree.Task, Depth: 40, CanExpand: true})           // arrow far outside
	s := layout(h.m)
	toggles := map[string]Rect{}
	for _, r := range s.regions {
		if r.kind == regToggle {
			toggles[r.id] = r.rect
			if r.rect.X+r.rect.W > s.tree.X+s.tree.W || r.rect.W <= 0 {
				t.Errorf("toggle %s at %+v outside the tree %+v", r.id, r.rect, s.tree)
			}
		}
	}
	if r, ok := toggles["t:edge"]; !ok || r.W != 1 || r.X != tw-1 {
		t.Errorf("edge toggle %+v %v", r, ok)
	}
	if _, ok := toggles["t:deep"]; ok {
		t.Error("deep toggle kept")
	}
	y := s.tree.Y + len(h.m.tree.Rows) - 1
	if r, _ := s.hit(tw, y); r.kind != regDivider {
		t.Errorf("divider hit %+v", r)
	}
	if got := (Rect{5, 5, 4, 4}).Intersect(Rect{7, 0, 10, 6}); got != (Rect{7, 5, 2, 1}) {
		t.Errorf("intersect %+v", got)
	}
}
