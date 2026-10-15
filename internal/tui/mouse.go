package tui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

// mouseState is the click and drag state machine. Raw mouse messages arrive in order,
// so double clicks and drags are decided here, against the pure layout.
type mouseState struct {
	last struct {
		id   string // region kind and id of the last left click
		x, y int
		at   time.Time
	}
	dragging bool // the divider
}

func (m *Model) mouseMsg(msg tea.MouseMsg) tea.Cmd {
	ev := msg.Mouse()
	switch msg.(type) {
	case tea.MouseClickMsg:
		return m.click(ev)
	case tea.MouseMotionMsg:
		if m.mouse.dragging {
			m.divider = treeWidth(ev.X, m.w)
			m.clampAll()
		} else if m.hover {
			m.setHover(ev)
		}
	case tea.MouseReleaseMsg:
		m.mouse.dragging = false
	case tea.MouseWheelMsg:
		return m.wheel(ev)
	}
	return nil
}

// setHover highlights the tree row under the pointer (hover is opt-in).
func (m *Model) setHover(ev tea.Mouse) {
	m.tree.Hover = ""
	if m.dlg != nil || m.menu != nil || m.help {
		return
	}
	if r, ok := layout(m).hit(ev.X, ev.Y); ok && (r.kind == regRow || r.kind == regToggle) {
		m.tree.Hover = r.id
	}
}

func (m *Model) click(ev tea.Mouse) tea.Cmd {
	if m.help {
		m.help = false
		return nil
	}
	if m.menu != nil {
		return m.menuClick(ev)
	}
	switch ev.Button {
	case tea.MouseRight:
		return m.rightClick(ev)
	case tea.MouseLeft:
	default:
		return nil // the middle button is ignored
	}
	s := layout(m)
	r, ok := s.hit(ev.X, ev.Y)
	if !ok || (m.dlg != nil && r.kind != regDlgPart) {
		return nil // a dialog ignores clicks outside its fields and buttons
	}
	id := strconv.Itoa(int(r.kind)) + ":" + r.id
	now := m.d.now()
	double := m.mouse.last.id == id && now.Sub(m.mouse.last.at) <= doubleClick &&
		abs(ev.X-m.mouse.last.x) <= 1 && abs(ev.Y-m.mouse.last.y) <= 1
	m.mouse.last.id, m.mouse.last.x, m.mouse.last.y, m.mouse.last.at = id, ev.X, ev.Y, now
	if double {
		m.mouse.last.id = "" // a third click starts over
	}
	preview := r.kind == regPane && m.tab == tabs.Preview
	if m.typ.on {
		// Typing mode: a click inside the preview is not forwarded (a double click
		// attaches); a click outside leaves typing mode and then does what it does.
		if preview {
			if double {
				t, _ := m.target()
				return m.attachRun(t)
			}
			return nil
		}
		m.leaveTyping("", false)
	}
	switch r.kind {
	case regDlgPart:
		return m.dialogClick(r, ev, double)
	case regBox, regBoxText:
		m.focus = focusBox
		if r.kind == regBoxText {
			g := boxGeom(m)
			m.box.ed.Click(ev.X-g.textX, ev.Y-g.y-1, g.textW)
		}
		return m.syncPreview()
	case regBoxTo:
		m.focus = focusBox
		return m.open(m.recipientDialog())
	case regBoxKind:
		m.focus = focusBox
		if m.box.reply == 0 {
			m.box.kind = (m.box.kind + 1) % len(kinds)
		}
	case regBoxSend:
		m.focus = focusBox
		if m.boxBusy() {
			return nil
		}
		return m.send()
	case regToast:
		return m.setTab(tabs.Log)
	case regItemAct:
		m.focus = focusPane
		it, act, _ := strings.Cut(r.id, ":")
		i, _ := strconv.Atoi(it)
		k, _ := strconv.Atoi(act)
		items := m.items(m.tab)
		if i < len(items) && k < len(items[i].Acts) {
			m.selectItem(items, i)
			return m.itemAct(items[i].Acts[k])
		}
	case regDivider:
		m.mouse.dragging = true
	case regToggle:
		m.focus = focusTree
		m.tree.Select(r.id)
		on := true
		if row := m.tree.Selected(); row != nil {
			on = !row.Expanded
		}
		return tea.Batch(m.expand(r.id, on), m.selected())
	case regRow:
		return m.rowClicked(r.id, double)
	case regTree:
		m.focus = focusTree
	case regTab:
		n, _ := strconv.Atoi(r.id)
		m.focus = focusPane
		return m.setTab(tabs.All[n])
	case regPane:
		m.focus = focusPane
		if preview && m.prev.Target.Pane != "" {
			if double {
				t, _ := m.target()
				return m.attachRun(t)
			}
			return m.enterTyping()
		}
		if isItemTab(m.tab) && ev.Y >= s.body.Y {
			lines, _, _ := m.itemView(m.tab, false)
			off := max(0, min(m.scroll[m.tab], len(lines)-s.body.H))
			m.itemAtLine(off + ev.Y - s.body.Y)
		}
	case regTabs:
		m.focus = focusPane
	case regCounter:
		return m.counter(r.id)
	case regHint:
		n, _ := strconv.Atoi(r.id)
		if hs := hints(); n < len(hs) {
			// Like its key: with Needs or Inbox focused, a row action acts on the item.
			sc := scopeGlobal
			if m.paneFocused() {
				sc = scopePane
			}
			return m.do(hs[n].act, hs[n].arg, sc)
		}
	}
	return nil
}

// counter is a click on a header counter.
func (m *Model) counter(id string) tea.Cmd {
	switch id {
	case "needs":
		return m.setTab(tabs.Needs)
	case "inbox":
		return m.setTab(tabs.Inbox)
	case "deliver":
		for _, r := range m.tree.Rows {
			if r.Kind == tree.Deliver {
				m.focus = focusTree
				m.narrowPane = false
				m.tree.Select(r.Key)
				return m.selected()
			}
		}
	}
	return nil
}

// wheel scrolls only the region under the pointer.
func (m *Model) wheel(ev tea.Mouse) tea.Cmd {
	s := layout(m)
	r, ok := s.hit(ev.X, ev.Y)
	if !ok {
		return nil
	}
	up := ev.Button == tea.MouseWheelUp
	if m.help {
		if up {
			m.helpScroll(-wheelLines)
		} else {
			m.helpScroll(wheelLines)
		}
		return nil
	}
	if m.menu != nil {
		if r.kind == regMenu || r.kind == regMenuItem {
			d := 1
			if up {
				d = -1
			}
			m.menu.sel = max(0, min(m.menu.sel+d, len(m.menu.items)-1))
		}
		return nil
	}
	horizontal := ev.Button == tea.MouseWheelLeft || ev.Button == tea.MouseWheelRight || ev.Mod.Contains(tea.ModShift)
	n := wheelLines
	if up {
		n = -n
	}
	if m.dlg != nil {
		if r.kind == regDlgPart {
			i, _ := strconv.Atoi(r.id)
			_, _, hits := m.dialogView()
			if i < len(hits) {
				m.dlg.form.Wheel(hits[i], n, m.formWidth())
			}
		}
		return nil
	}
	switch r.kind {
	case regBoxText:
		g := boxGeom(m)
		m.box.ed.Scroll(n, g.textW, g.rows)
	case regTree, regRow, regToggle:
		if horizontal {
			return nil
		}
		if up {
			m.tree.Off -= wheelLines
		} else {
			m.tree.Off += wheelLines
		}
		m.tree.Clamp(s.tree.H)
	case regPane:
		if m.tab != tabs.Preview {
			n := wheelLines
			if up {
				n = -n
			}
			lines := len(m.tabLines(m.tab, s.body.W))
			m.scroll[m.tab] = max(0, min(m.scroll[m.tab]+n, lines-s.body.H))
			return nil
		}
		if horizontal {
			n := hscrollStep
			if ev.Button == tea.MouseWheelLeft || (ev.Button == tea.MouseWheelUp) {
				n = -n
			}
			m.prev.HScroll(n, s.body.W)
			return nil
		}
		if up {
			return m.scrollPreview(wheelLines)
		}
		return m.scrollPreview(-wheelLines)
	}
	return nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
