package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/tui/preview"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

// key dispatches a key press through the binding table.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	if m.typ.on {
		return m.typeKey(k)
	}
	if m.help {
		return m.helpKey(k)
	}
	if m.dlg != nil {
		return m.dialogKey(k)
	}
	if m.menu != nil {
		return m.menuKey(k)
	}
	if m.filtering {
		return m.filterKey(k)
	}
	if m.focus == focusBox {
		return m.boxKey(k)
	}
	sc := scopeTree
	if m.paneFocused() {
		sc = scopePane
	}
	b, ok := lookup(k, sc)
	if !ok {
		return nil
	}
	return m.do(b.act, b.arg, sc)
}

// paste inserts pasted text into the dialog or the message box.
func (m *Model) paste(s string) tea.Cmd {
	switch {
	case m.typ.on:
		return m.typePaste(s)
	case m.dlg != nil:
		m.dlg.form.Paste(s)
	case m.focus == focusBox:
		m.box.ed.Insert(s)
	}
	return nil
}

// treeFocused: the tree draws its selection as focused.
func (m *Model) treeFocused() bool {
	return m.dlg == nil && m.focus != focusBox && !m.paneFocused()
}

// paneFocused: keys go to the right pane.
func (m *Model) paneFocused() bool {
	if m.focus == focusBox {
		return false
	}
	if layout(m).narrow {
		return m.narrowPane
	}
	return m.focus == focusPane
}

// do performs an action; sc is the pane it applies to.
func (m *Model) do(act action, arg int, sc scope) tea.Cmd {
	switch act {
	case actQuit:
		return m.quit()
	case actHelp:
		m.help = true
	case actTab:
		return m.setTab(tabs.Tab(arg))
	case actNeeds:
		return m.setTab(tabs.Needs)
	case actReload:
		m.stats++
		return tea.Batch(m.requestLoad(), m.ensureText())
	case actRedraw:
		return tea.ClearScreen
	case actFocusNext:
		return m.focusRing(1)
	case actFocusPrev:
		return m.focusRing(-1)
	case actNarrower, actWider:
		step := dividerStep
		if act == actNarrower {
			step = -step
		}
		m.divider = treeWidth(treeWidth(m.divider, m.w)+step, m.w)
		m.clampAll()
	case actResetWidth:
		m.divider = 0
	case actType:
		return m.enterTyping()
	case actMenu:
		m.menuAtFocus()
	case actFilter:
		return m.startFilter()
	case actClearFilter:
		return m.clearFilter()
	case actCopy:
		if sc == scopePane && isItemTab(m.tab) {
			return m.copyItem()
		}
		return m.copy()
	case actExpandAll:
		return m.expandAll()
	case actSync:
		return m.syncRegistry()
	case actAddProject:
		return m.addProject()
	case actClosed:
		m.tree.ShowClosed = !m.tree.ShowClosed
		if m.snap != nil {
			m.tree.Rebuild(m.snap) // drop the closed rows now; a load adds them
		}
		return tea.Batch(m.requestLoad(), m.selected())
	default:
		if act >= actNew {
			if sc == scopePane && isItemTab(m.tab) {
				return m.actOnItem(act)
			}
			return m.action(act)
		}
		if sc == scopePane {
			return m.paneAction(act)
		}
		return m.treeAction(act)
	}
	return nil
}

func (m *Model) setTab(t tabs.Tab) tea.Cmd {
	m.tab = t
	if t != tabs.Preview {
		m.leaveTyping("", false)
	}
	if layout(m).narrow {
		m.narrowPane = true
	}
	return tea.Batch(m.syncPreview(), m.ensureText())
}

// selected follows a selection change: preview target, Details and the message
// box's recipient.
func (m *Model) selected() tea.Cmd {
	m.tree.EnsureVisible(layout(m).tree.H)
	m.followSelection()
	return tea.Batch(m.retarget(), m.ensureText())
}

func (m *Model) treeAction(act action) tea.Cmd {
	h := max(1, layout(m).tree.H)
	switch act {
	case actUp:
		m.tree.Move(-1)
	case actDown:
		m.tree.Move(1)
	case actPageUp:
		m.tree.Move(-h)
	case actPageDown:
		m.tree.Move(h)
	case actTop:
		m.tree.SelectIndex(0)
	case actBottom:
		m.tree.SelectIndex(len(m.tree.Rows) - 1)
	case actCollapse:
		r := m.tree.Selected()
		if r == nil {
			return nil
		}
		if !m.setExpanded(r.Key, false) {
			if p := m.tree.Parent(m.tree.Index()); p >= 0 {
				m.tree.SelectIndex(p)
			}
		}
	case actExpand:
		if r := m.tree.Selected(); r != nil {
			return tea.Batch(m.expand(r.Key, true), m.selected())
		}
	case actToggle, actEnter:
		// Enter attaches to a run, a coordinator or a task's live run; elsewhere it
		// toggles like space.
		r := m.tree.Selected()
		if act == actEnter {
			if rn := m.attachable(r); rn != nil {
				return m.attachRun(rn)
			}
			if r != nil && (r.Kind == tree.Run || r.Kind == tree.Coord) {
				return m.attachRun(r.Run) // says why not
			}
		}
		if r != nil {
			return tea.Batch(m.expand(r.Key, !r.Expanded), m.selected())
		}
	default:
		return nil
	}
	return m.selected()
}

// setExpanded expands or collapses key and rebuilds the rows.
func (m *Model) setExpanded(key string, on bool) bool {
	if !m.tree.SetExpanded(key, on) {
		return false
	}
	m.tree.Rebuild(m.snap)
	return true
}

// expand is setExpanded plus the load an "earlier runs" row needs.
func (m *Model) expand(key string, on bool) tea.Cmd {
	if !m.setExpanded(key, on) {
		return nil
	}
	if on && len(key) > 2 && key[:2] == "h:" {
		return m.requestLoad()
	}
	return nil
}

func (m *Model) paneAction(act action) tea.Cmd {
	s := layout(m)
	h := max(1, s.body.H)
	if m.tab == tabs.Preview {
		switch act {
		case actUp:
			return m.scrollPreview(1)
		case actDown:
			return m.scrollPreview(-1)
		case actPageUp:
			return m.scrollPreview(h)
		case actPageDown:
			return m.scrollPreview(-h)
		case actTop:
			return m.scrollPreview(preview.HistoryLines + h)
		case actFollow:
			return m.follow()
		case actEnter:
			r, _ := m.target()
			return m.attachRun(r)
		case actLeft:
			m.prev.HScroll(-hscrollStep, s.body.W)
		case actRight:
			m.prev.HScroll(hscrollStep, s.body.W)
		}
		return nil
	}
	if isItemTab(m.tab) {
		switch act {
		case actUp:
			return m.moveItem(-1)
		case actDown:
			return m.moveItem(1)
		case actEnter:
			return m.itemPrimary()
		}
	}
	n := len(m.tabLines(m.tab, s.body.W))
	off := m.scroll[m.tab]
	switch act {
	case actUp:
		off--
	case actDown:
		off++
	case actPageUp:
		off -= h
	case actPageDown:
		off += h
	case actTop:
		off = 0
	case actFollow:
		off = n
	}
	m.scroll[m.tab] = max(0, min(off, n-h))
	return nil
}

// scrollPreview scrolls the preview n lines up (negative: down). The first scroll up
// fetches the scrollback once and freezes the view (scroll-lock); reaching the bottom
// follows the live pane again.
func (m *Model) scrollPreview(n int) tea.Cmd {
	if m.prev.Target.Pane == "" {
		return nil
	}
	if !m.prev.Locked {
		if n <= 0 || m.cap.lockLines > 0 {
			return nil
		}
		m.cap.lockLines = n
		m.cap.lockGen++
		cmd := m.capture(0, preview.HistoryLines)
		m.syncPreview() // live captures pause
		return cmd
	}
	m.prev.ScrollBy(n, m.bodyHeight())
	if n < 0 && m.prev.Scroll == 0 {
		return m.follow()
	}
	return nil
}

// rowClicked selects a row; a double click acts like Enter: it attaches to a run, a
// coordinator or a task's live run, and toggles the other rows.
func (m *Model) rowClicked(key string, double bool) tea.Cmd {
	m.focus = focusTree
	if !m.tree.Select(key) {
		return nil
	}
	if double {
		sel := m.selected()
		return tea.Batch(sel, m.treeAction(actEnter))
	}
	return m.selected()
}
