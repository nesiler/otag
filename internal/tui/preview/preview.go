// Package preview holds the TUI's view of one agent pane: the last capture,
// sanitized to text and SGR colours, cropped to the preview's size, with a live
// (follow) mode and a scroll-locked history mode.
package preview

import (
	"hash/fnv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/tmux"
)

// HistoryLines is how much scrollback a scroll-lock fetches.
const HistoryLines = 2000

// Target is the pane a preview shows.
type Target struct {
	RunID string
	Pane  string
}

// Model is the preview state. The scheduling of captures (rate, one in flight) lives
// in the TUI's root model; this one only holds and renders content.
type Model struct {
	Target Target
	View   tmux.PaneView
	Lines  []string // sanitized screen rows of the last live capture
	Have   bool     // a capture of Target arrived
	Err    string
	hash   uint64
	// Scroll-lock: Locked freezes the content at History (scrollback + the screen at
	// that moment); Scroll counts lines up from the bottom.
	Locked  bool
	History []string
	Scroll  int
	XOff    int
}

// SetTarget points the preview at t; a new target drops the content.
func (m *Model) SetTarget(t Target) bool {
	if t == m.Target {
		return false
	}
	*m = Model{Target: t}
	return true
}

// Set stores a live capture. It reports whether the content changed (an unchanged
// capture needs no re-render).
func (m *Model) Set(v tmux.PaneView) bool {
	h := fnv.New64a()
	for _, l := range v.Lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	sum := h.Sum64()
	// Everything else of the view but the text: geometry, and the cursor, which decides
	// the used part of the screen (and, in typing mode, where the cursor is drawn).
	same := v.Width == m.View.Width && v.Height == m.View.Height && v.Dead == m.View.Dead &&
		v.CursorOn == m.View.CursorOn && v.CursorX == m.View.CursorX && v.CursorY == m.View.CursorY &&
		v.AltScreen == m.View.AltScreen && v.InMode == m.View.InMode
	m.View, m.Have, m.Err = v, true, ""
	if sum == m.hash && same && m.Lines != nil {
		return false
	}
	m.hash = sum
	m.Lines = sanitizeAll(v.Lines)
	return true
}

// Lock enters scroll-lock with the fetched scrollback (the current screen follows
// it) and scrolls up n lines in a window of h rows.
func (m *Model) Lock(history tmux.PaneView, n, h int) {
	m.Locked = true
	m.History = append(sanitizeAll(history.Lines), m.Lines[:m.used()]...)
	m.Scroll = 0
	m.ScrollBy(n, h)
}

// Follow leaves scroll-lock: live updates resume.
func (m *Model) Follow() {
	m.Locked, m.History, m.Scroll = false, nil, 0
}

// ScrollBy moves the scroll-lock view up (positive) by n lines within a window of h
// rows.
func (m *Model) ScrollBy(n, h int) {
	if !m.Locked {
		return
	}
	m.Scroll = max(0, min(m.Scroll+n, len(m.History)-h))
}

// HScroll moves the horizontal offset by n cells within a window of w cells.
func (m *Model) HScroll(n, w int) {
	widest := 0
	for _, l := range m.content() {
		widest = max(widest, ansi.StringWidth(l))
	}
	m.XOff = max(0, min(m.XOff+n, widest-w))
}

func (m *Model) content() []string {
	if m.Locked {
		return m.History
	}
	return m.Lines
}

// Render returns the preview body for w x h cells, cropped at XOff: the scroll-lock
// window, or the bottom h rows of the screen's used part (down to its last text or the
// cursor, whichever is lower), since agents keep their input at the bottom.
func (m *Model) Render(w, h int) []string {
	lines := m.content()
	end := len(lines)
	if m.Locked {
		end -= m.Scroll
	} else {
		end = m.used()
	}
	start := max(0, end-h)
	out := make([]string, 0, h)
	for _, l := range lines[start:max(start, end)] {
		out = append(out, ansi.Cut(l, m.XOff, m.XOff+w))
	}
	return out
}

// Cursor is where the pane's cursor sits in Render(w, h) (column, row), when the live
// view shows it: the application shows it (cursor_flag) and it is not cropped out.
func (m *Model) Cursor(w, h int) (x, y int, ok bool) {
	if m.Locked || !m.Have || !m.View.CursorOn || h <= 0 {
		return 0, 0, false
	}
	end := m.used()
	start := max(0, end-h)
	x, y = m.View.CursorX-m.XOff, m.View.CursorY-start
	if x < 0 || x >= w || y < 0 || y >= end-start {
		return 0, 0, false
	}
	return x, y, true
}

// used is the number of screen rows down to the last one with text or the cursor.
func (m *Model) used() int {
	n := 0
	if m.View.CursorOn {
		n = m.View.CursorY + 1
	}
	for i := len(m.Lines) - 1; i >= n; i-- {
		if strings.TrimSpace(ansi.Strip(m.Lines[i])) != "" {
			n = i + 1
			break
		}
	}
	return min(n, len(m.Lines))
}

func sanitizeAll(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = Sanitize(l)
	}
	return out
}

// Sanitize keeps the printable text and the SGR (colour and attribute) sequences of
// a captured line; every other escape or control (OSC, cursor movement, BEL, ...) is
// dropped, so a pane can never move the TUI's cursor or retitle its terminal.
func Sanitize(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }) {
		return s
	}
	var b strings.Builder
	var state byte
	for len(s) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(s, state, nil)
		state = newState
		switch {
		case width > 0:
			b.WriteString(seq)
		case isSGR(seq):
			b.WriteString(seq)
		case seq == "\t":
			b.WriteString("    ")
		case len(seq) > 0 && seq[0] >= 0xc0:
			if r, _ := utf8.DecodeRuneInString(seq); r >= 0xa0 {
				b.WriteString(seq) // zero-width text (joiners, combining marks); not a C1 control
			}
		}
		s = s[n:]
	}
	return b.String()
}

// isSGR: CSI [params] m, with only digits, ";" and ":" as parameters.
func isSGR(seq string) bool {
	params, ok := strings.CutPrefix(seq, "\x1b[")
	if !ok || !strings.HasSuffix(params, "m") {
		return false
	}
	return strings.Trim(params[:len(params)-1], "0123456789;:") == ""
}
