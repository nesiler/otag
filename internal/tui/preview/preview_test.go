package preview

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/tmux"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                           "plain text",
		"\x1b[31mred\x1b[0m \x1b[1;38;5;208mx": "\x1b[31mred\x1b[0m \x1b[1;38;5;208mx",
		"\x1b[38:2::1:2:3mtrue":                "\x1b[38:2::1:2:3mtrue",
		"a\x1b]0;title\x07b":                   "ab",          // OSC: retitle
		"a\x1b]8;;http://x\x1b\\link":          "alink",       // OSC 8 hyperlink
		"a\x1b[2Jb\x1b[10;5Hc\x1b[?25l":        "abc",         // erase, move, private mode
		"a\x1b[>4;2mb":                         "ab",          // a private-prefixed "m"
		"bell\x07\r":                           "bell",        // controls
		"tab\there":                            "tab    here", // tabs as spaces
		"geniş 中文 é":                          "geniş 中文 é", // wide and combining runes
		"c1\u009b31mx":                         "c131mx",      // U+009B is no CSI in UTF-8 text
		"c1\u0085x":                            "c1x",         // a C1 control
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func view(lines ...string) tmux.PaneView {
	return tmux.PaneView{Width: 20, Height: len(lines), Lines: lines}
}

func TestSetAndRender(t *testing.T) {
	var m Model
	if !m.SetTarget(Target{RunID: "r", Pane: "%1"}) || m.SetTarget(Target{RunID: "r", Pane: "%1"}) {
		t.Fatal("SetTarget")
	}
	if !m.Set(view("one", "two", "", "")) {
		t.Fatal("first capture: no change")
	}
	if m.Set(view("one", "two", "", "")) {
		t.Fatal("same capture: change")
	}
	// The used part of the screen ends at its last text: blank rows below are not shown.
	if got := m.Render(10, 5); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("render %q", got)
	}
	// The cursor below the text counts as used: a cursor-only change is a change.
	v := view("one", "two", "", "")
	v.CursorOn, v.CursorY = true, 3
	if !m.Set(v) {
		t.Fatal("a moved cursor reported unchanged")
	}
	if got := m.Render(10, 2); !slices.Equal(got, []string{"", ""}) {
		t.Fatalf("render with the cursor at the bottom %q", got)
	}
	v.CursorX = 2
	if !m.Set(v) || m.Set(v) {
		t.Fatal("cursor column change")
	}
	// Bottom rows when the pane is taller than the preview; crop at XOff, wide runes
	// kept whole.
	m.Set(view("a", "b", "中文中文abc", "d"))
	if got := m.Render(10, 2); !slices.Equal(got, []string{"中文中文ab", "d"}) {
		t.Fatalf("bottom rows %q", got)
	}
	m.HScroll(2, 4)
	if got := m.Render(4, 2); m.XOff != 2 || ansi.Strip(got[0]) != "文中" {
		t.Fatalf("xoff %d: %q", m.XOff, got)
	}
	m.HScroll(100, 4) // clamped: the widest line is 11 cells
	if m.XOff != 7 {
		t.Fatalf("xoff %d", m.XOff)
	}
	if !m.SetTarget(Target{RunID: "s", Pane: "%2"}) || m.Have || m.Lines != nil || m.XOff != 0 {
		t.Fatalf("new target keeps content: %+v", m)
	}
}

func TestCursor(t *testing.T) {
	var m Model
	m.SetTarget(Target{Pane: "%1"})
	v := view("a", "b", "> hi", "", "")
	v.CursorOn, v.CursorX, v.CursorY = true, 4, 2
	m.Set(v)
	// Three used rows in a body of two: rows 1-2 show, the cursor is on the second.
	if x, y, ok := m.Cursor(10, 2); !ok || x != 4 || y != 1 {
		t.Fatalf("cursor %d,%d %v", x, y, ok)
	}
	if x, y, ok := m.Cursor(10, 5); !ok || x != 4 || y != 2 {
		t.Fatalf("tall body: %d,%d %v", x, y, ok)
	}
	if _, _, ok := m.Cursor(4, 2); ok {
		t.Fatal("a cursor right of the body shows")
	}
	m.XOff = 2
	if x, _, ok := m.Cursor(4, 2); !ok || x != 2 {
		t.Fatalf("h-scrolled: %d %v", x, ok)
	}
	m.XOff = 5
	if _, _, ok := m.Cursor(4, 2); ok {
		t.Fatal("a cursor left of the crop shows")
	}
	m.XOff = 0
	v.CursorOn = false
	m.Set(v)
	if _, _, ok := m.Cursor(10, 2); ok {
		t.Fatal("a hidden cursor shows")
	}
}

func TestScrollLock(t *testing.T) {
	var m Model
	m.SetTarget(Target{Pane: "%1"})
	m.Set(view("s1", "s2", "s3", ""))
	m.Lock(tmux.PaneView{Lines: []string{"h1", "h2", "h3"}}, 1, 2)
	if !m.Locked || !slices.Equal(m.History, []string{"h1", "h2", "h3", "s1", "s2", "s3"}) {
		t.Fatalf("locked %v %q", m.Locked, m.History)
	}
	if got := m.Render(5, 2); !slices.Equal(got, []string{"s1", "s2"}) {
		t.Fatalf("scrolled 1 %q", got)
	}
	m.ScrollBy(100, 2) // clamped at the top
	if got := m.Render(5, 2); m.Scroll != 4 || !slices.Equal(got, []string{"h1", "h2"}) {
		t.Fatalf("top %d %q", m.Scroll, got)
	}
	// Live captures do not move a locked view.
	m.Set(view("new", "", "", ""))
	if got := m.Render(5, 2); !slices.Equal(got, []string{"h1", "h2"}) {
		t.Fatalf("locked view moved %q", got)
	}
	m.Follow()
	if got := m.Render(5, 2); m.Locked || !slices.Equal(got, []string{"new"}) {
		t.Fatalf("follow %q", got)
	}
	if strings.Join(m.History, "") != "" {
		t.Fatal("history kept")
	}
}
