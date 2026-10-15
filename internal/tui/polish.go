package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/tree"
)

// T4 polish: the tree filter, copy, expand all, the help overlay's
// scroll and the terminal reports (docs/TUI.md).

// debugf writes to the debug log (OTAG_TUI_DEBUG=1); it never gets message bodies or
// typed keys.
func (m *Model) debugf(format string, args ...any) {
	if m.d.debug != nil {
		m.d.debug(format, args...)
	}
}

// Filter.

// startFilter opens the filter prompt in the header; the tree shows the matches as
// the text changes.
func (m *Model) startFilter() tea.Cmd {
	m.filtering = true
	m.focus = focusTree
	m.narrowPane = false
	return nil
}

// clearFilter drops the filter and its prompt.
func (m *Model) clearFilter() tea.Cmd {
	if !m.filtering && m.tree.Filter == "" {
		return nil
	}
	m.filtering = false
	return m.setFilter("")
}

// setFilter changes the filter text and rebuilds the rows.
func (m *Model) setFilter(s string) tea.Cmd {
	m.tree.Filter = s
	if m.snap != nil {
		m.tree.Rebuild(m.snap)
	}
	return m.selected()
}

// filterKey edits the filter: text appends, backspace deletes, enter keeps the
// filter and leaves the prompt, esc drops it.
func (m *Model) filterKey(k tea.KeyPressMsg) tea.Cmd {
	switch s := k.String(); s {
	case "ctrl+c":
		return m.quit()
	case "esc":
		return m.clearFilter()
	case "enter":
		m.filtering = false
	case "backspace":
		if r := []rune(m.tree.Filter); len(r) > 0 {
			return m.setFilter(string(r[:len(r)-1]))
		}
	case "ctrl+u":
		return m.setFilter("")
	case "up", "down":
		return m.treeAction(map[string]action{"up": actUp, "down": actDown}[s])
	default:
		if k.Text != "" && k.Mod&^tea.ModShift == 0 {
			return m.setFilter(m.tree.Filter + k.Text)
		}
	}
	return nil
}

// filterSegment is the filter in the header: "/text", with a cursor while editing.
func (m *Model) filterSegment() string {
	th := m.theme
	s := "/" + m.tree.Filter
	if m.filtering {
		return th.Accent.Render(s) + th.Selected.Render(" ")
	}
	return th.Accent.Render(s)
}

// Copy.

// copyText is what `y` copies and what it is called: a run's attach command, else
// the id of the row.
func (m *Model) copyText() (string, string) {
	s := m.sel()
	switch {
	case s.row == nil:
		return "", ""
	case s.run != nil && s.row.Kind != tree.Task && s.row.Kind != tree.Earlier:
		if s.run.Session != nil && m.d.attachCmd != nil {
			return m.d.attachCmd(s.run.Session.TmuxSession, s.run.Session.TmuxPane), "the attach command of " + s.run.ID
		}
		return s.run.ID, s.run.ID
	case s.task != nil:
		return s.task.ID, s.task.ID
	case s.project != "":
		return s.project, s.project
	}
	return "", ""
}

// copy puts the id or attach command on the clipboard (OSC 52).
func (m *Model) copy() tea.Cmd {
	text, what := m.copyText()
	if text == "" {
		m.flash("copy: select a project, task or run", true)
		return nil
	}
	m.flash("copied "+what, false)
	return tea.SetClipboard(text)
}

// copyItem is `y` on Needs or Inbox: the id the selected item acts on, like its
// buttons.
func (m *Model) copyItem() tea.Cmd {
	items := m.items(m.tab)
	if i := m.itemIndex(m.tab, items); i >= 0 && len(items[i].Acts) > 0 {
		id := items[i].Acts[0].ID
		m.flash("copied "+id, false)
		return tea.SetClipboard(id)
	}
	m.flash("copy: this item has no id", true)
	return nil
}

// Expand all.

// expandAll expands every row of the selected row's project, children included.
func (m *Model) expandAll() tea.Cmd {
	project := m.sel().project
	if project == "" || m.snap == nil {
		m.flash("expand all: select a project", true)
		return nil
	}
	for pass := 0; pass < 8; pass++ {
		changed := false
		for _, r := range m.tree.Rows {
			if r.Project == project && r.CanExpand && !r.Expanded && !strings.HasPrefix(r.Key, "h:") &&
				m.tree.SetExpanded(r.Key, true) {
				changed = true
			}
		}
		if !changed {
			break
		}
		m.tree.Rebuild(m.snap)
	}
	return m.selected()
}

// Sync.

// syncRegistry runs `otag sync`: the TOML registry into the database.
func (m *Model) syncRegistry() tea.Cmd {
	if why := m.actWhy(actSync); why != "" {
		m.flash("sync: "+why, true)
		return nil
	}
	return m.fire(pending{args: []string{"sync"}})
}

// Add project.

// addable is the git repository the TUI was started in when no project uses it yet
// ("" = none).
func (m *Model) addable() string {
	if m.addDir == "" || m.snap == nil {
		return ""
	}
	for _, p := range m.snap.Projects {
		if filepath.Clean(p.Path) == filepath.Clean(m.addDir) {
			return ""
		}
	}
	return m.addDir
}

// addProject runs `otag project add <dir> --yes` for the TUI's repository.
func (m *Model) addProject() tea.Cmd {
	if why := m.actWhy(actAddProject); why != "" {
		m.flash("add project: "+why, true)
		return nil
	}
	return m.fire(pending{args: []string{"project", "add", m.addDir, "--yes"}})
}

// emptyLines is the tree without projects: how to set otag up or add one.
func (m *Model) emptyLines(w int) []string {
	var text string
	switch {
	case m.unconfigured:
		text = "otag is not set up yet.\n\nQuit (q) and run `otag init` (or just `otag`) in a terminal: a short wizard " +
			"finds your agents and writes the config."
	case m.addable() != "":
		text = "No projects yet.\n\nPress a to add " + setup.TildePath(m.addable(), m.home) + ", or run `otag project add` in a git repository."
	default:
		text = "No projects yet.\n\nRun `otag project add` in a git repository, then come back (ctrl+r reloads)."
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l == "" {
			out = append(out, "")
			continue
		}
		for _, wl := range strings.Split(ansi.Wrap(l, max(10, w-1), ""), "\n") {
			out = append(out, m.theme.Dim.Render(" "+wl))
		}
	}
	return out
}

// Help overlay.

// helpBox is the help overlay's rectangle and the lines it shows. A screen too short
// for the whole text scrolls it (helpOff): the last line then says where you are.
func (m *Model) helpBox() (Rect, []string) {
	hw := min(m.w-4, 78)
	all := helpLines(hw - 4)
	hh := min(m.h-2, len(all)+2)
	r := Rect{(m.w - hw) / 2, (m.h - hh) / 2, hw, hh}
	rows := hh - 2
	if len(all) <= rows {
		return r, all
	}
	rows-- // the position line
	m.helpOff = max(0, min(m.helpOff, len(all)-rows))
	lines := append([]string(nil), all[m.helpOff:m.helpOff+rows]...)
	pos := fmt.Sprintf("↑↓ pgup pgdn scroll · lines %d-%d of %d", m.helpOff+1, m.helpOff+rows, len(all))
	return r, append(lines, m.theme.Dim.Render(ansi.Truncate(pos, hw-4, "…")))
}

// helpScroll moves the help text by n lines (clamped by helpBox).
func (m *Model) helpScroll(n int) {
	m.helpOff = max(0, m.helpOff+n)
	m.helpBox()
}

// helpKey handles a key while the help overlay is open.
func (m *Model) helpKey(k tea.KeyPressMsg) tea.Cmd {
	page := max(1, m.h-5)
	switch k.String() {
	case "ctrl+c":
		return m.quit()
	case "?", "esc", "q", "enter":
		m.help = false
	case "up", "k":
		m.helpScroll(-1)
	case "down", "j":
		m.helpScroll(1)
	case "pgup":
		m.helpScroll(-page)
	case "pgdown", "space":
		m.helpScroll(page)
	case "home", "g":
		m.helpOff = 0
	case "end", "G":
		m.helpScroll(1 << 20)
	}
	return nil
}
