package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tui/style"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

// Context menus (docs/TUI.md "Context menu"). Every item runs the same code as its
// key or button: m.do for a key, itemAct for an inline button, boxKey for a key of the
// message box. A disabled item shows why and does nothing when chosen.

// menuItem is one line of a menu.
type menuItem struct {
	label string
	key   string // the key that does the same ("" = none)
	why   string // not "": the item is disabled, and why
	run   func() tea.Cmd
}

// menu is the open context menu, anchored at a cell.
type menu struct {
	items  []menuItem
	build  func() []menuItem // makes the items again for a new snapshot
	target string            // menuTarget when it opened
	sel    int
	off    int // first item shown
	x, y   int // the anchor: the menu opens below-right of it, kept on screen
}

// Menu size limits.
const (
	menuMinWidth = 24
	menuMaxWidth = 76
)

// act is a menu item that does what the key of act does in scope sc.
func (m *Model) act(label string, act action, arg int, sc scope) menuItem {
	return menuItem{label: label, key: keyFor(act, arg), why: m.actWhy(act), run: func() tea.Cmd { return m.do(act, arg, sc) }}
}

// menuTarget names what a menu is about: the selected row, the shown tab and its
// selected item. A snapshot that changes it (the item is gone, another took its place)
// closes the menu: it must not act on a target other than what the key acts on now.
func (m *Model) menuTarget() string {
	return fmt.Sprintf("%s|%d|%s", m.tree.Cursor(), m.tab, m.itemKey[m.tab])
}

// refreshMenu follows a new snapshot: a menu whose target changed closes, another is
// rebuilt (its items' availability may have changed), keeping its selected item.
func (m *Model) refreshMenu() {
	mn := m.menu
	if mn == nil {
		return
	}
	items := mn.build()
	if m.menuTarget() != mn.target || len(items) == 0 {
		m.menu = nil
		return
	}
	label := mn.items[min(mn.sel, len(mn.items)-1)].label
	mn.items = items
	mn.sel = min(mn.sel, len(items)-1)
	for i, it := range items {
		if it.label == label {
			mn.sel = i
		}
	}
}

// openMenu shows the menu build makes at (x, y); an empty one opens nothing.
func (m *Model) openMenu(build func() []menuItem, x, y int) {
	items := build()
	if len(items) == 0 || m.dlg != nil || m.typ.on {
		return
	}
	m.menu = &menu{items: items, build: build, target: m.menuTarget(), x: x, y: y}
	for i, it := range items {
		if it.why == "" {
			m.menu.sel = i
			break
		}
	}
	m.debugf("menu open items=%d", len(items))
}

// actWhy is the reason the key of act would not call the CLI now ("" = it would).
// It repeats exactly what the key checks before it calls (cannotAct, then the target's
// kind) and nothing of the backend's own rules: the CLI's error is the toast, as when the
// key is pressed.
func (m *Model) actWhy(act action) string {
	switch act {
	case actNew, actEdit, actCancel, actReady, actBlock, actStart, actStop, actPark, actResume, actCoord, actReply,
		actMessage, actReview, actDeliver, actAckAll, actSync, actAddProject, actPush, actReopen:
		if why := m.cannotAct(); why != "" {
			return why
		}
	}
	if act == actAddProject && m.addable() == "" {
		return "otag tui was not started in a git repository that is not a project yet"
	}
	switch act {
	case actNew, actCoord, actEdit, actCancel, actReady, actBlock, actStart, actStop, actPark, actResume, actReply,
		actReview, actDeliver, actType, actEnter, actPush, actReopen:
	default:
		return ""
	}
	s := m.sel()
	switch act {
	case actNew, actCoord:
		if s.project == "" {
			return "select a project"
		}
	case actEdit, actCancel, actReady, actBlock, actStart, actResume:
		if s.task == nil {
			return "select a task"
		}
	case actStop, actPark:
		if s.run == nil || s.run.Status == run.Ended {
			return "select a live run"
		}
	case actReply:
		var taskID, runID string
		if s.task != nil {
			taskID = s.task.ID
		}
		if s.run != nil {
			runID = s.run.ID
		}
		if m.openAsk(taskID, runID) == nil {
			return "no open question here"
		}
	case actReview:
		t := s.task
		if t != nil && t.Status != task.Review {
			if of := m.snap.ReviewOf[t.ID]; of != "" {
				t = m.findTask(of)
			}
		}
		if t == nil || t.Status != task.Review {
			return "select a task in review"
		}
	case actDeliver:
		if s.task == nil && (s.row == nil || s.row.Kind != tree.Deliver) {
			return "select a done task"
		}
	case actPush:
		if s.task == nil || s.task.Branch == "" {
			return "select a task that has a branch (it gets one with its first run)"
		}
	case actReopen:
		if s.task == nil || s.task.Status != task.Done {
			return "select a done task"
		}
	case actType, actEnter:
		r, why := m.target()
		if r == nil {
			return why
		}
		if pane, why := m.previewable(r); pane == "" {
			if why == "" {
				why = "no live pane"
			}
			return why
		}
	}
	return ""
}

// rowMenu is the menu of the selected tree row.
func (m *Model) rowMenu() []menuItem {
	r := m.tree.Selected()
	if r == nil || m.snap == nil {
		return m.appMenu()
	}
	t := scopeTree
	it := func(label string, act action) menuItem { return m.act(label, act, 0, t) }
	toggle := "Expand"
	if r.Expanded {
		toggle = "Collapse"
	}
	closed := "Show closed tasks"
	if m.tree.ShowClosed {
		closed = "Hide closed tasks"
	}
	details := m.act("Details", actTab, int(tabs.Details), t)
	switch r.Kind {
	case tree.Project:
		coord := "Coordinator: start…"
		if m.coordOf(r.Project) != nil {
			coord = "Coordinator: attach"
		}
		return []menuItem{it("New task…", actNew), it(coord, actCoord), it("Message the coordinator", actMessage),
			it("Expand all", actExpandAll), it(toggle, actToggle), it(closed, actClosed), details,
			m.act("Stats", actTab, int(tabs.Stats), t), it("Sync registry", actSync), it("Filter…", actFilter)}
	case tree.Deliver:
		return []menuItem{it("Deliver…", actDeliver), it(toggle, actToggle), details}
	case tree.Task:
		return []menuItem{it("Start run…", actStart), it("Attach", actEnter), it("Type into pane", actType), it("Message task", actMessage),
			it("Reply", actReply), it("Review…", actReview), it("Deliver…", actDeliver), it("Push…", actPush),
			it("Reopen…", actReopen), it("Edit…", actEdit),
			it("Mark ready", actReady), it("Block…", actBlock), it("Cancel…", actCancel), it("Resume", actResume),
			details, it("Copy id", actCopy)}
	case tree.Run, tree.Coord:
		msg := "Message run"
		if r.Kind == tree.Coord {
			msg = "Message the coordinator"
		}
		return []menuItem{it("Attach", actEnter), it("Type into pane", actType), it(msg, actMessage), it("Reply", actReply),
			it("Stop…", actStop), it("Park", actPark), it("Resume", actResume), details,
			it("Copy attach command", actCopy)}
	case tree.Earlier:
		return []menuItem{it(toggle+" earlier runs", actToggle)}
	}
	return m.appMenu()
}

// appMenu is the menu of the places that are not about a row: the header, the footer,
// the empty part of the tree.
func (m *Model) appMenu() []menuItem {
	closed := "Show closed tasks"
	if m.tree.ShowClosed {
		closed = "Hide closed tasks"
	}
	g := scopeGlobal
	items := []menuItem{m.act("Filter…", actFilter, 0, g), m.act(closed, actClosed, 0, g), m.act("Reload", actReload, 0, g),
		m.act("Redraw", actRedraw, 0, g), m.act("Help", actHelp, 0, g), m.act("Quit", actQuit, 0, g)}
	if dir := m.addable(); dir != "" {
		items = append([]menuItem{m.act("Add project "+filepath.Base(dir), actAddProject, 0, g)}, items...)
	}
	return items
}

// dividerMenu is the menu of the divider.
func (m *Model) dividerMenu() []menuItem {
	g := scopeGlobal
	return []menuItem{m.act("Narrower tree", actNarrower, 0, g), m.act("Wider tree", actWider, 0, g),
		m.act("Default width", actResetWidth, 0, g)}
}

// tabsMenu lists the tabs.
func (m *Model) tabsMenu() []menuItem {
	var out []menuItem
	for _, t := range tabs.All {
		label := tabs.Title(t, m.snap)
		if t == m.tab {
			label += " (shown)"
		}
		out = append(out, m.act(label, actTab, int(t), scopeGlobal))
	}
	return out
}

// paneMenu is the menu of the right pane for the shown tab: the preview, the item of
// Needs or Inbox that is selected, or a text tab.
func (m *Model) paneMenu() []menuItem {
	p := scopePane
	if m.tab == tabs.Preview {
		return []menuItem{m.act("Attach", actEnter, 0, p), m.act("Type into pane", actType, 0, p),
			m.act("Follow live", actFollow, 0, p),
			m.act("Top of the history", actTop, 0, p), m.act("Message", actMessage, 0, p),
			m.act("Details", actTab, int(tabs.Details), p), m.act("Copy attach command", actCopy, 0, p)}
	}
	if isItemTab(m.tab) {
		items := m.items(m.tab)
		if i := m.itemIndex(m.tab, items); i >= 0 {
			var out []menuItem
			for _, a := range items[i].Acts {
				out = append(out, menuItem{label: a.Label, key: itemKey(a), why: m.cannotAct(),
					run: func() tea.Cmd { return m.itemAct(a) }})
			}
			return append(out, m.act("Reload", actReload, 0, scopeGlobal))
		}
	}
	items := []menuItem{m.act("Top", actTop, 0, p), m.act("Bottom", actFollow, 0, p)}
	if m.tab == tabs.Stats {
		items = append(items, m.act("Refresh", actReload, 0, scopeGlobal))
	}
	return items
}

// itemKey is the key that runs an inline action of Needs or Inbox ("" = only the
// button, or enter when it is the item's first).
func itemKey(a tabs.Act) string {
	switch a.Kind {
	case tabs.ActReady:
		return "R"
	case tabs.ActResume:
		return "u"
	case tabs.ActReply:
		return "r"
	case tabs.ActStop:
		return "S"
	case tabs.ActMessage:
		return "m"
	case tabs.ActAckAll:
		return "A"
	}
	return ""
}

// boxMenu is the menu of the message box; its items press the box's own keys.
func (m *Model) boxMenu() []menuItem {
	press := func(label, key string, code rune, mod tea.KeyMod) menuItem {
		return menuItem{label: label, key: key, run: func() tea.Cmd {
			m.focus = focusBox
			return m.boxKey(tea.KeyPressMsg{Code: code, Mod: mod})
		}}
	}
	kind := press("Next kind", "ctrl+k", 'k', tea.ModCtrl)
	if m.box.reply > 0 {
		kind.why = "a reply has no kind"
	}
	send := press("Send", "enter", tea.KeyEnter, 0)
	if m.boxBusy() {
		send.why = "still sending…"
	} else if why := m.cannotAct(); why != "" {
		send.why = why
	}
	return []menuItem{send, press("Recipient…", "ctrl+t", 't', tea.ModCtrl), kind, press("Back to the tree", "esc", tea.KeyEscape, 0)}
}

// menuAtFocus opens the menu of what has the focus (the keys o, shift+F10, menu).
func (m *Model) menuAtFocus() {
	s := layout(m)
	switch {
	case s.tooSmall:
	case m.paneFocused():
		m.openMenu(m.paneMenu, s.body.X+2, s.body.Y+1)
	default:
		x, y := s.tree.X+2, s.tree.Y
		if r := m.tree.Selected(); r != nil {
			x += 2 * r.Depth
			y += m.tree.Index() - m.tree.VisibleOff(s.tree.H) + 1
		}
		m.openMenu(m.rowMenu, x, y)
	}
}

// rightClick opens the menu of the region under the pointer; a row or an item is
// selected first, like a left click does.
func (m *Model) rightClick(ev tea.Mouse) tea.Cmd {
	if m.dlg != nil {
		return nil
	}
	s := layout(m)
	r, ok := s.hit(ev.X, ev.Y)
	if !ok || s.tooSmall {
		return nil
	}
	if m.typ.on {
		if r.kind == regPane && m.tab == tabs.Preview {
			return nil // not forwarded to the agent, and no menu over it
		}
		m.leaveTyping("", false)
	}
	var build func() []menuItem
	var cmd tea.Cmd
	switch r.kind {
	case regRow, regToggle:
		m.focus = focusTree
		if m.tree.Select(r.id) {
			cmd = m.selected()
		}
		build = m.rowMenu
	case regTree:
		m.focus = focusTree
		build = m.appMenu
	case regTab, regTabs:
		build = m.tabsMenu
	case regItemAct:
		m.focus = focusPane
		it, _, _ := strings.Cut(r.id, ":")
		if i, err := strconv.Atoi(it); err == nil {
			if all := m.items(m.tab); i < len(all) {
				m.selectItem(all, i)
			}
		}
		build = m.paneMenu
	case regPane:
		m.focus = focusPane
		if isItemTab(m.tab) && ev.Y >= s.body.Y {
			lines, _, _ := m.itemView(m.tab, false)
			off := max(0, min(m.scroll[m.tab], len(lines)-s.body.H))
			m.itemAtLine(off + ev.Y - s.body.Y)
		}
		build = m.paneMenu
	case regDivider:
		build = m.dividerMenu
	case regBox, regBoxText, regBoxTo, regBoxKind, regBoxSend:
		m.focus = focusBox
		build = m.boxMenu
	default: // the header, its counters, the footer, its buttons, the toast
		build = m.appMenu
	}
	m.openMenu(build, ev.X, ev.Y)
	return cmd
}

// menuClick handles a click while a menu is open: an item runs, the border does
// nothing, a click outside closes it (a right click there opens that place's menu).
func (m *Model) menuClick(ev tea.Mouse) tea.Cmd {
	s := layout(m)
	if r, ok := s.hit(ev.X, ev.Y); ok {
		switch r.kind {
		case regMenuItem:
			i, _ := strconv.Atoi(r.id)
			return m.menuRun(i)
		case regMenu:
			return nil
		}
	}
	m.menu = nil
	if ev.Button == tea.MouseRight {
		return m.rightClick(ev)
	}
	return nil
}

// menuRun chooses item i: a disabled one says why and keeps the menu open.
func (m *Model) menuRun(i int) tea.Cmd {
	it := m.menu.items[i]
	if it.why != "" {
		m.flash(it.label+": "+it.why, true)
		return nil
	}
	m.menu = nil
	m.debugf("menu run %q", it.label)
	return it.run()
}

// menuKey handles a key while a menu is open.
func (m *Model) menuKey(k tea.KeyPressMsg) tea.Cmd {
	mn := m.menu
	move := func(d int) {
		mn.sel = max(0, min(mn.sel+d, len(mn.items)-1))
	}
	switch k.String() {
	case "ctrl+c":
		return m.quit()
	case "esc", "q", "o", "shift+f10", "menu":
		m.menu = nil
	case "up", "k":
		move(-1)
	case "down", "j":
		move(1)
	case "pgup":
		move(-5)
	case "pgdown":
		move(5)
	case "home", "g":
		mn.sel = 0
	case "end", "G":
		mn.sel = len(mn.items) - 1
	case "enter", "space":
		return m.menuRun(mn.sel)
	}
	return nil
}

// menuView is the open menu's box and its visible lines; first is the index of the
// first item shown.
func (m *Model) menuView() (r Rect, lines []string, first int) {
	mn := m.menu
	labelW, keyW, whyW := 0, 0, 0
	for _, it := range mn.items {
		labelW = max(labelW, ansi.StringWidth(it.label))
		keyW = max(keyW, ansi.StringWidth(it.key))
		if it.why != "" {
			whyW = max(whyW, ansi.StringWidth(it.why)+4)
		}
	}
	iw := min(max(menuMinWidth, labelW+2+keyW+whyW), min(menuMaxWidth, m.w-6))
	w := iw + 4
	h := min(m.h-2, len(mn.items)+2)
	rows := h - 2
	mn.off = max(0, min(mn.off, len(mn.items)-rows))
	if mn.sel < mn.off {
		mn.off = mn.sel
	}
	if mn.sel >= mn.off+rows {
		mn.off = mn.sel - rows + 1
	}
	r = Rect{max(0, min(mn.x, m.w-w)), max(0, min(mn.y, m.h-h)), w, h}
	th := m.theme
	for i := mn.off; i < len(mn.items) && i-mn.off < rows; i++ {
		it := mn.items[i]
		text := style.Fit(it.label, labelW) + "  " + style.Fit(it.key, keyW)
		if it.why != "" {
			text += "  — " + it.why
		}
		text = style.Fit(text, iw)
		switch {
		case i == mn.sel:
			text = th.Selected.Render(ansi.Strip(text))
		case it.why != "":
			text = th.Dim.Render(text)
		}
		lines = append(lines, text)
	}
	return r, lines, mn.off
}
