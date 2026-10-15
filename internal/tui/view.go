package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tui/style"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

// View renders the screen. The layout's blocks tile it, so without an overlay the
// frame is their lines side by side; with one, Lip Gloss layers compose them.
func (m *Model) View() tea.View {
	if !m.frameValid {
		m.frame, m.frameValid = m.render(), true
	}
	v := tea.NewView(m.frame)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	if m.hover {
		v.MouseMode = tea.MouseModeAllMotion
	}
	v.ReportFocus = true
	v.Cursor = m.typingCursor()
	// The terminal's tab title and its program status (OSC 7501): what needs the human.
	needs := tabs.NeedsCount(m.snap)
	v.WindowTitle = "otag"
	v.ProgramStatus = &tea.ProgramStatus{State: tea.ProgramStateIdle, App: "otag", Title: "otag"}
	if needs > 0 {
		v.WindowTitle = fmt.Sprintf("otag (%d)", needs)
		v.ProgramStatus = &tea.ProgramStatus{State: tea.ProgramStateBlocked, Kind: tea.ProgramStatusKindQuestion,
			App: "otag", Title: "otag", Message: fmt.Sprintf("%d need you", needs)}
	}
	return v
}

// typingCursor is the agent's cursor in typing mode, mapped into the preview (nil:
// not typing, hidden by the agent, or cropped out).
func (m *Model) typingCursor() *tea.Cursor {
	if !m.typ.on || m.tab != tabs.Preview || m.help || m.dlg != nil {
		return nil
	}
	s := layout(m)
	if s.tooSmall || s.body.H <= 0 || (s.narrow && s.showTree) {
		return nil
	}
	x, y, ok := m.prev.Cursor(s.body.W, s.body.H)
	if !ok {
		return nil
	}
	return tea.NewCursor(s.body.X+x, s.body.Y+y)
}

func (m *Model) render() string {
	if m.help || m.dlg != nil || m.menu != nil {
		return m.compose().Render()
	}
	bs := m.blocks()
	lines := make([]string, m.h)
	for y := range lines {
		var b strings.Builder
		for _, bl := range bs { // in x order within a row
			if y >= bl.rect.Y && y < bl.rect.Y+bl.rect.H {
				b.WriteString(bl.lines[y-bl.rect.Y])
			}
		}
		lines[y] = b.String()
	}
	return strings.Join(lines, "\n")
}

// block is the content of a layout block, fitted to its rectangle.
type block struct {
	kind  regionKind
	rect  Rect
	lines []string
}

// blocks renders every block of the layout (z 0), in reading order.
func (m *Model) blocks() []block {
	s := layout(m)
	th := m.theme
	var out []block
	put := func(k regionKind, lines []string) {
		if r, ok := s.find(k); ok {
			out = append(out, block{k, r.rect, strings.Split(style.Block(lines, r.rect.W, r.rect.H), "\n")})
		}
	}
	if s.tooSmall {
		put(regTooSmall, []string{fmt.Sprintf("terminal too small (%dx%d; otag tui needs %dx%d)", m.w, m.h, minWidth, minHeight)})
		return out
	}
	in := tree.NewInfo(m.snap, m.live, m.d.now(), th)
	put(regHeader, []string{m.headerLine()})
	if s.showTree {
		put(regTree, m.treeLines(s.tree.W, s.tree.H, in))
	}
	if r, ok := s.find(regDivider); ok {
		div := make([]string, r.rect.H)
		for i := range div {
			div[i] = th.Dim.Render(style.GlyphDivider)
		}
		put(regDivider, div)
	}
	if !s.narrow || !s.showTree {
		put(regTabs, []string{m.tabsLine()})
		put(regPane, m.paneLines(s, in))
	}
	put(regBox, m.boxLines(boxGeom(m)))
	put(regFooter, []string{m.footerLine()})
	return out
}

// compose places the blocks as Lip Gloss layers, with the help overlay or a dialog
// on top.
func (m *Model) compose() *lipgloss.Compositor {
	var layers []*lipgloss.Layer
	for _, b := range m.blocks() {
		layers = append(layers, lipgloss.NewLayer(strings.Join(b.lines, "\n")).X(b.rect.X).Y(b.rect.Y).ID(blockIDs[b.kind]))
	}
	if m.help {
		r, lines := m.helpBox()
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(r.W).Height(r.H).MaxHeight(r.H)
		layers = append(layers, lipgloss.NewLayer(box.Render(strings.Join(lines, "\n"))).
			X(r.X).Y(r.Y).Z(10).ID(blockIDs[regHelp]))
	}
	if m.dlg != nil {
		r, lines, _ := m.dialogView()
		body := style.Block(lines, r.W-4, r.H-2)
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Cyan).Padding(0, 1).
			Width(r.W).Height(r.H).MaxHeight(r.H)
		layers = append(layers, lipgloss.NewLayer(box.Render(body)).X(r.X).Y(r.Y).Z(20).ID(blockIDs[regDialog]))
	}
	if m.menu != nil {
		r, lines, _ := m.menuView()
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Cyan).Padding(0, 1).
			Width(r.W).Height(r.H).MaxHeight(r.H)
		layers = append(layers, lipgloss.NewLayer(box.Render(style.Block(lines, r.W-4, r.H-2))).X(r.X).Y(r.Y).Z(30).
			ID(blockIDs[regMenu]))
	}
	return lipgloss.NewCompositor(layers...)
}

// segment is a piece of the header; id names a clickable counter.
type segment struct{ id, text string }

func headerSegments(m *Model) []segment {
	th := m.theme
	sep := segment{text: th.Dim.Render(" │ ")}
	segs := []segment{{text: th.Bold.Render(" otag")}}
	if m.filtering || m.tree.Filter != "" {
		segs = append(segs, sep, segment{text: m.filterSegment()})
	}
	if m.snap == nil {
		if m.loadErr != "" {
			return append(segs, sep, segment{text: th.Bad.Render("load failed: " + m.loadErr)})
		}
		return append(segs, sep, segment{text: th.Dim.Render("loading…")})
	}
	if m.snap.ReadOnly {
		segs = append(segs, sep, segment{text: th.Bad.Render("READ-ONLY: the database is newer than this otag")})
	}
	needs := fmt.Sprintf("%s %d needs you", style.GlyphNeeds, tabs.NeedsCount(m.snap))
	if tabs.NeedsCount(m.snap) > 0 {
		needs = th.Warn.Render(needs)
	}
	segs = append(segs, sep, segment{"needs", needs}, sep,
		segment{"inbox", fmt.Sprintf("%s %d", style.GlyphInbox, tabs.InboxCount(m.snap))})
	if n := len(m.snap.Deliverable); n > 0 {
		segs = append(segs, sep, segment{"deliver", th.Accent.Render(fmt.Sprintf("%s %d to deliver", style.GlyphDeliver, n))})
	}
	daemon := "daemon " + th.Good.Render(style.GlyphRunning)
	if !m.snap.Daemon {
		daemon = "daemon " + th.Dim.Render(style.GlyphIdle)
	}
	segs = append(segs, sep, segment{text: daemon})
	if q := m.quotaSummary(); q != "" {
		segs = append(segs, sep, segment{text: q})
	}
	if m.loadErr != "" {
		segs = append(segs, sep, segment{text: th.Bad.Render("reload failed: " + m.loadErr)})
	}
	return segs
}

// quotaSummary is the account closest to its limit, from 50% on.
func (m *Model) quotaSummary() string {
	best, window, account := -1.0, "", ""
	for _, r := range m.snap.Quota {
		if e := r.Effective(); e != nil && !r.Stale && *e > best {
			best, window, account = *e, r.Window, r.Account
		}
	}
	cool := len(m.snap.Cooldown) > 0
	if best < tree.QuotaWarn && !cool {
		return ""
	}
	text := ""
	if best >= tree.QuotaWarn {
		text = fmt.Sprintf("%s %.0f%% (%s)", account, best, window)
	}
	if cool {
		c := m.snap.Cooldown[0]
		text = strings.TrimSpace(text + " " + c.Account + " cooldown")
	}
	if cool || best >= run.DefaultQuotaThreshold {
		return m.theme.Bad.Render(text)
	}
	return m.theme.Warn.Render(text)
}

func (m *Model) headerLine() string {
	var b strings.Builder
	for _, s := range headerSegments(m) {
		b.WriteString(s.text)
	}
	return b.String()
}

func (m *Model) treeLines(w, h int, in tree.Info) []string {
	if m.snap == nil {
		return nil
	}
	if len(m.tree.Rows) == 0 {
		return []string{m.theme.Dim.Render("no projects: add one to the registry, then otag sync")}
	}
	return m.tree.Render(w, h, m.treeFocused(), in)
}

func tabLabel(t tabs.Tab, m *Model) string {
	return fmt.Sprintf(" %d %s ", int(t)+1, tabs.Title(t, m.snap))
}

func (m *Model) tabsLine() string {
	var parts []string
	for _, t := range tabs.All {
		l := tabLabel(t, m)
		if t == m.tab {
			l = m.theme.TabActive.Render(l)
		} else {
			l = m.theme.Tab.Render(l)
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, m.theme.Dim.Render("│"))
}

// paneLines is the right pane: a title bar, then the tab's body.
func (m *Model) paneLines(s screen, in tree.Info) []string {
	w, h := s.body.W, s.body.H
	th := m.theme
	if m.tab == tabs.Preview {
		r, why := m.target()
		_, reason := m.previewable(r)
		title := th.Dim.Render(why)
		if r != nil {
			mode := th.Good.Render("FOLLOW")
			if m.prev.Locked {
				mode = th.Warn.Render(fmt.Sprintf("SCROLL ↑%d", m.prev.Scroll))
			}
			parts := []string{tree.RunGlyph(r, in) + " " + r.ID, r.Profile, r.Status}
			if m.prev.Target.Pane != "" {
				parts = append(parts, m.prev.Target.Pane, mode)
			}
			if m.prev.XOff > 0 {
				parts = append(parts, fmt.Sprintf("→%d", m.prev.XOff))
			}
			title = strings.Join(parts, th.Dim.Render(" · "))
		}
		if m.typ.on {
			title = th.Typing.Render(" TYPING → "+m.typ.run+" ") + " C-q / M-q / click outside to leave"
		}
		out := []string{title}
		switch {
		case m.prev.Target.Pane == "":
			if reason == "" {
				reason = why
			}
			out = append(out, "", th.Dim.Render("  no live pane: "+reason))
		case m.prev.Err != "" && !m.prev.Have:
			out = append(out, "", th.Bad.Render("  capture failed: "+m.prev.Err))
		case !m.prev.Have:
			out = append(out, "", th.Dim.Render("  capturing…"))
		default:
			out = append(out, m.prev.Render(w, h)...)
		}
		return out
	}
	lines := m.tabLines(m.tab, w)
	off := max(0, min(m.scroll[m.tab], len(lines)-h))
	title := th.Dim.Render(m.tabTitle())
	if len(lines) > h {
		title += th.Dim.Render(fmt.Sprintf(" · %d-%d of %d", off+1, min(off+h, len(lines)), len(lines)))
	}
	return append([]string{title}, lines[off:min(len(lines), off+h)]...)
}

func (m *Model) tabTitle() string {
	switch m.tab {
	case tabs.Details:
		if r := m.tree.Selected(); r != nil {
			return "details of " + strings.TrimPrefix(strings.SplitN(r.Key, ":", 2)[1], "deliver:")
		}
	case tabs.Needs:
		return "what waits for you (otag needs)"
	case tabs.Inbox:
		return "unread messages and notifications (peek: nothing is marked read)"
	case tabs.Stats:
		return "otag stats · otag quota (ctrl+r refreshes)"
	case tabs.Log:
		return "actions of this session"
	}
	return ""
}

// tabLines is the body of a text tab, wrapped at w.
func (m *Model) tabLines(t tabs.Tab, w int) []string {
	th := m.theme
	switch t {
	case tabs.Needs, tabs.Inbox:
		lines, _, _ := m.itemView(t, m.paneFocused() && m.dlg == nil)
		return lines
	case tabs.Log:
		if len(m.log) == 0 {
			return []string{th.Dim.Render("no actions yet")}
		}
		return m.log
	case tabs.Details, tabs.Stats:
		tv := m.text[t]
		switch {
		case tv.key == "" && t == tabs.Details:
			return []string{th.Dim.Render("select a project, task or run")}
		case tv.err != "":
			return append([]string{th.Bad.Render(tv.err)}, tabs.TextLines(tv.text, w)...)
		case tv.text == "" && tv.inFlight:
			return []string{th.Dim.Render("loading…")}
		}
		return tabs.TextLines(tv.text, w)
	}
	return nil
}

func (m *Model) footerLine() string {
	th := m.theme
	var parts []string
	for _, b := range hints() {
		parts = append(parts, hintLabel(b))
	}
	left := th.Footer.Render(strings.Join(parts, "  "))
	return style.Join(left, m.footerRight(), m.w)
}

// footerRight is the toast, else the actions in flight.
func (m *Model) footerRight() string {
	th := m.theme
	switch {
	case m.toast.text != "" && m.toast.bad:
		return th.Bad.Render(m.toast.text)
	case m.toast.text != "":
		return th.Good.Render(m.toast.text)
	case m.inflight > 0:
		return th.Warn.Render(fmt.Sprintf("… %d action(s) running", m.inflight))
	}
	return ""
}

// helpLines is the help overlay: the binding table by scope, then the notes, wrapped
// to w cells (a wrapped line hangs under its text).
func helpLines(w int) []string {
	titles := map[scope]string{scopeGlobal: "Everywhere", scopeTree: "Tree", scopePane: "Right pane (preview and tabs)",
		scopeAct: "Actions on the selected row (tree or right pane)"}
	var out []string
	para := func(indent int, hang int, text string) {
		for i, l := range strings.Split(ansi.Wrap(text, max(10, w-indent-hang), ""), "\n") {
			pad := indent
			if i > 0 {
				pad += hang
			}
			out = append(out, strings.Repeat(" ", pad)+l)
		}
	}
	last := scope(-1)
	for _, b := range bindings {
		if b.scope != last {
			if last != -1 {
				out = append(out, "")
			}
			out = append(out, titles[b.scope])
			last = b.scope
		}
		if h := b.Help(); h.Key != "" {
			// "  key         description", the description hanging under itself.
			desc := ansi.Wrap(h.Desc, max(10, w-15), "")
			for i, l := range strings.Split(desc, "\n") {
				if i == 0 {
					out = append(out, fmt.Sprintf("  %-12s %s", h.Key, l))
				} else {
					out = append(out, strings.Repeat(" ", 15)+l)
				}
			}
		}
	}
	out = append(out, "")
	para(0, 2, "Message box: Enter send · alt/shift+Enter, ctrl+j newline · ctrl+k kind · ctrl+t recipient · Esc back. "+
		"Dialogs: Tab moves · ctrl+s or a button submits · Esc.")
	para(0, 2, "Mouse: click selects, double click expands (a live run: attaches), the wheel scrolls what is under it, "+
		"drag the divider to resize; a click on the preview types into it. Right-click opens the context menu "+
		"(key o): each item shows its key, a disabled one says why. Esc, ? or a click closes this help.")
	out = append(out, "")
	para(0, 2, "Typing mode (i): every key goes to the agent, Esc and Esc Esc too; C-q, M-q or a click outside the "+
		"preview leave. Enter waits 150 ms after typed text (500 after a paste) so agents do not take it for part of "+
		"a paste. Not sent: clicks in the preview, image paste, super/Cmd keys; ctrl+shift+letter degrades; "+
		"shift+Enter only reaches agents that turned on extended keys (alt+Enter, ctrl+j or the message box work "+
		"everywhere); a wider pane is cropped.")
	para(0, 2, "Attach (Enter, double click): full screen in otag's tmux; back with M-q, C-q or a click on \"← otag\" in "+
		"its status bar (macOS: M-q needs Option as Meta). Inside your own tmux it opens as a popup. set -g @otag_ui "+
		"off (in otag's tmux.conf) keeps otag's bindings and status bar off its server.")
	return out
}
