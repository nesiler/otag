package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tui/form"
	"github.com/nesiler/otag/internal/tui/style"
	"github.com/nesiler/otag/internal/tui/tree"
)

// The message box (path 1 of docs/TUI.md): `otag send` or, in reply mode, `otag reply`,
// with phase 4's delivery and its proof. It never types into a pane.

// boxMaxRows bounds the text area.
const boxMaxRows = 6

// kinds are the message kinds ctrl+k cycles through.
var kinds = []string{msg.KindInfo, msg.KindCommand, msg.KindQuestion}

// recipient is the box's addressee: a run id, a task id, coord or human.
type recipient struct{ to, project, label string }

type msgbox struct {
	ed    form.Editor
	to    recipient
	kind  int
	reply int64 // reply mode: the open question it answers...
	// replyProject is the question's project (--project of the reply).
	replyProject string
	pinned       bool   // chosen by hand: the recipient no longer follows the selection
	sending      string // the busy key of the box's send in flight ("" = none)
}

func (b *msgbox) setTo(r recipient) { b.to, b.reply = r, 0 }

// boxBusy reports whether the box's last send is still in flight.
func (m *Model) boxBusy() bool { return m.box.sending != "" && m.busy[m.box.sending] }

// selRecipient is the recipient the selected row suggests.
func (m *Model) selRecipient() recipient {
	s := m.sel()
	switch {
	case s.row == nil:
		return recipient{}
	case s.row.Kind == tree.Coord:
		return recipient{to: "coord", project: s.project, label: "coord"}
	case s.row.Kind == tree.Run && s.run.Status != run.Ended:
		return recipient{to: s.run.ID, project: s.project, label: s.run.ID}
	case s.task != nil:
		return recipient{to: s.task.ID, project: s.project, label: s.task.ID}
	case s.project != "":
		return recipient{to: "coord", project: s.project, label: "coord"}
	}
	return recipient{}
}

// followSelection points an idle box at the selection.
func (m *Model) followSelection() {
	if !m.box.pinned && m.focus != focusBox && m.box.reply == 0 {
		m.box.to = m.selRecipient()
	}
}

// focusBox gives the box the keyboard; fromSel re-targets it at the selection.
func (m *Model) focusBox(fromSel bool) tea.Cmd {
	if fromSel {
		m.box.pinned, m.box.reply = false, 0
		m.box.to = m.selRecipient()
	}
	m.focus = focusBox
	return m.syncPreview()
}

// replyBox puts the box in reply mode for question id.
func (m *Model) replyBox(id int64, project string) tea.Cmd {
	m.box.reply, m.box.replyProject = id, project
	m.focus = focusBox
	return nil
}

// send sends the box's text: reply ID -- text, or send --project P --kind K -- TO text.
func (m *Model) send() tea.Cmd {
	b := &m.box
	text := b.ed.String()
	if strings.TrimSpace(text) == "" {
		m.flash("type a message first", true)
		return nil
	}
	var p pending
	reply := b.reply
	switch {
	case reply > 0:
		p = pending{project: b.replyProject, args: []string{"reply", strconv.FormatInt(reply, 10), "--", text}}
	case b.to.to == "":
		m.flash("no recipient: select a run, task or project (ctrl+t picks one)", true)
		return nil
	default:
		p = pending{args: []string{"send", "--project", b.to.project, "--kind", kinds[b.kind], "--", b.to.to, text}}
	}
	// Once sent, the box empties and leaves reply mode, unless the user changed them
	// since (typed on, or chose another question to answer).
	p.then = func(Outcome) tea.Cmd {
		if b.ed.String() == text {
			b.ed.Set("")
		}
		if b.reply == reply {
			b.reply = 0
		}
		return nil
	}
	cmd := m.fire(p)
	if cmd != nil {
		b.sending = p.key()
	}
	return cmd
}

// boxKey handles a key while the box has the keyboard.
func (m *Model) boxKey(k tea.KeyPressMsg) tea.Cmd {
	g := boxGeom(m)
	switch k.String() {
	case "ctrl+c":
		return m.quit()
	case "esc":
		m.focus = focusTree
		return nil
	case "tab":
		return m.focusRing(1)
	case "shift+tab":
		return m.focusRing(-1)
	case "enter":
		if m.boxBusy() {
			m.flash("still sending…", false)
			return nil
		}
		return m.send()
	case "alt+enter", "shift+enter", "ctrl+j":
		m.box.ed.Insert("\n")
		return nil
	case "ctrl+k":
		if m.box.reply == 0 {
			m.box.kind = (m.box.kind + 1) % len(kinds)
		}
		return nil
	case "ctrl+t":
		return m.open(m.recipientDialog())
	case "shift+f10", "menu":
		m.openMenu(m.boxMenu, g.textX, g.y)
		return nil
	}
	m.box.ed.Key(k, g.textW)
	return nil
}

// geom is the message box's geometry: a separator row, then rows of text with the
// recipient chip, the kind chip and [Send] on the first.
type geom struct {
	y, h                int // the block: separator plus text rows
	chip, kind          string
	chipW, kindW        int
	textX, textW, sendX int
	rows                int
}

const sendLabel = "[Send]"

func boxGeom(m *Model) geom {
	b := &m.box
	var g geom
	if b.reply > 0 {
		g.chip = fmt.Sprintf(" reply #%d → ", b.reply)
	} else {
		to := b.to.label
		if to == "" {
			to = "—"
		}
		g.chip = " to: " + to + " ▾ "
		g.kind = " " + kinds[b.kind] + " ▾ "
	}
	g.chipW, g.kindW = ansi.StringWidth(g.chip), ansi.StringWidth(g.kind)
	g.textX = g.chipW + g.kindW + 1
	g.sendX = m.w - ansi.StringWidth(sendLabel)
	g.textW = max(4, g.sendX-1-g.textX)
	g.rows = max(1, min(boxMaxRows, b.ed.Height(g.textW)))
	g.h = g.rows + 1
	g.y = m.h - 1 - g.h
	return g
}

// boxLines renders the message box block.
func (m *Model) boxLines(g geom) []string {
	th := m.theme
	b := &m.box
	focused := m.focus == focusBox && m.dlg == nil
	title := " message "
	if b.reply > 0 {
		title = " reply "
	}
	sepStyle := th.Dim
	if focused {
		sepStyle = th.Accent
	}
	out := []string{sepStyle.Render("──" + title + strings.Repeat("─", max(0, m.w-2-len(title))))}
	var cursor func(string) string
	placeholder := "m: message " + b.to.label + " · r: reply to a question"
	if focused {
		cursor = func(s string) string { return th.Selected.Render(s) }
		placeholder = "Enter send · Alt+Enter newline · ctrl+k kind · ctrl+t recipient · Esc back"
	}
	rows := b.ed.View(g.textW, g.rows, cursor, placeholder, th.Dim.Render)
	send := sendLabel
	if m.boxBusy() {
		send = "[ … ]"
	}
	for i, r := range rows {
		var l string
		if i == 0 {
			chip := th.Accent.Render(g.chip)
			if b.reply > 0 {
				chip = th.Warn.Render(g.chip)
			}
			l = chip + th.Dim.Render(g.kind) + " " + style.Fit(r, g.textW) + " " + th.Accent.Render(send)
		} else {
			l = strings.Repeat(" ", g.textX) + r
		}
		out = append(out, l)
	}
	return out
}

// focusRing moves the focus d steps along tree → right pane → message box.
func (m *Model) focusRing(d int) tea.Cmd {
	ring := []focus{focusTree, focusPane, focusBox}
	i := 0
	for j, f := range ring {
		if f == m.focus {
			i = j
		}
	}
	m.focus = ring[(i+d+len(ring))%len(ring)]
	if layout(m).narrow && m.focus != focusBox {
		m.narrowPane = m.focus == focusPane
	}
	return m.syncPreview()
}
