package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/tui/tabs"
)

// The Needs and Inbox tabs are lists of items with inline action buttons: ↑↓ select
// an item, Enter runs its first action, a click on a button runs that one.

func isItemTab(t tabs.Tab) bool { return t == tabs.Needs || t == tabs.Inbox }

func (m *Model) items(t tabs.Tab) []tabs.Item {
	switch t {
	case tabs.Needs:
		return tabs.NeedsItems(m.snap, m.theme)
	case tabs.Inbox:
		return tabs.InboxItems(m.snap, m.theme)
	}
	return nil
}

// itemButton is an inline action's button in the lines of an item tab.
type itemButton struct{ line, x, w, item, act int }

// itemView renders an item tab: its lines, the buttons, and the line range of each
// item; focused marks the selected item as the pane's focus.
func (m *Model) itemView(t tabs.Tab, focused bool) ([]string, []itemButton, [][2]int) {
	th := m.theme
	items := m.items(t)
	cur := m.itemIndex(t, items)
	var lines []string
	var buttons []itemButton
	spans := make([][2]int, len(items))
	for i, it := range items {
		start := len(lines)
		marker := "  "
		if i == cur {
			marker = th.Dim.Render("› ")
			if focused {
				marker = th.Accent.Render("› ")
			}
		}
		if it.Head {
			lines = append(lines, it.Lines...)
		}
		for j, l := range it.Lines {
			if it.Head {
				break
			}
			if j == 0 {
				lines = append(lines, marker+l)
			} else {
				lines = append(lines, "  "+l)
			}
		}
		if !it.Head && (len(it.Acts) > 0 || it.Hint != "") {
			var b strings.Builder
			x := 4
			if len(it.Lines) == 0 {
				b.WriteString(marker + "  ")
			} else {
				b.WriteString("    ")
			}
			for k, a := range it.Acts {
				label := tabs.Button(a)
				w := ansi.StringWidth(label)
				buttons = append(buttons, itemButton{line: len(lines), x: x, w: w, item: i, act: k})
				b.WriteString(th.Accent.Render(label) + " ")
				x += w + 1
			}
			if it.Hint != "" {
				b.WriteString(th.Dim.Render("→ " + it.Hint))
			}
			lines = append(lines, b.String())
		}
		spans[i] = [2]int{start, len(lines)}
	}
	return lines, buttons, spans
}

// selectItem selects item i of the current tab, by its key: a reload that adds or
// removes items above it keeps it selected.
func (m *Model) selectItem(items []tabs.Item, i int) {
	m.item[m.tab], m.itemKey[m.tab] = i, items[i].Key
}

// adoptItems makes the item each item tab shows selected after a new snapshot its
// remembered one: when the selected item is gone, the fallback (the item now at its
// position) takes over its key, so later reloads keep that item instead of falling
// back by position again.
func (m *Model) adoptItems() {
	for _, t := range []tabs.Tab{tabs.Needs, tabs.Inbox} {
		items := m.items(t)
		if i := m.itemIndex(t, items); i >= 0 {
			m.item[t], m.itemKey[t] = i, items[i].Key
		}
	}
}

// itemIndex is the selected item of t: the one with the remembered key; when that is
// gone, the nearest selectable one at the remembered position (-1: none).
func (m *Model) itemIndex(t tabs.Tab, items []tabs.Item) int {
	if k := m.itemKey[t]; k != "" {
		for j := range items {
			if items[j].Key == k && !items[j].Head {
				return j
			}
		}
	}
	i := min(m.item[t], len(items)-1)
	for j := i; j >= 0 && j < len(items); j++ {
		if !items[j].Head {
			return j
		}
	}
	for j := i; j >= 0; j-- {
		if !items[j].Head {
			return j
		}
	}
	return -1
}

// moveItem selects the next (d > 0) or previous selectable item and scrolls to it.
func (m *Model) moveItem(d int) tea.Cmd {
	items := m.items(m.tab)
	i := m.itemIndex(m.tab, items)
	for j := i + d; j >= 0 && j < len(items); j += d {
		if !items[j].Head {
			i = j
			break
		}
	}
	if i < 0 {
		return nil
	}
	m.selectItem(items, i)
	_, _, spans := m.itemView(m.tab, false)
	h := max(1, m.bodyHeight())
	off := m.scroll[m.tab]
	if spans[i][0] < off {
		off = spans[i][0]
	}
	if spans[i][1] > off+h {
		off = spans[i][1] - h
	}
	if i > 0 && items[i-1].Head && spans[i][1]-spans[i-1][0] <= h { // its section title too
		off = min(off, spans[i-1][0])
	}
	m.scroll[m.tab] = max(0, off)
	return nil
}

// itemPrimary runs the selected item's first action.
func (m *Model) itemPrimary() tea.Cmd {
	items := m.items(m.tab)
	i := m.itemIndex(m.tab, items)
	if i < 0 || len(items[i].Acts) == 0 {
		return nil
	}
	return m.itemAct(items[i].Acts[0])
}

// itemAtLine selects the item shown on line (an index into the tab's lines).
func (m *Model) itemAtLine(line int) {
	items := m.items(m.tab)
	_, _, spans := m.itemView(m.tab, false)
	for i, sp := range spans {
		if line >= sp[0] && line < sp[1] && !items[i].Head {
			m.selectItem(items, i)
		}
	}
}
