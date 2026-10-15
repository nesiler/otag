package form

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/tui/style"
)

func key(s string) tea.KeyPressMsg {
	named := map[string]rune{"left": tea.KeyLeft, "right": tea.KeyRight, "up": tea.KeyUp, "down": tea.KeyDown,
		"home": tea.KeyHome, "end": tea.KeyEnd, "enter": tea.KeyEnter, "tab": tea.KeyTab, "esc": tea.KeyEscape,
		"backspace": tea.KeyBackspace, "space": tea.KeySpace}
	var mod tea.KeyMod
	for _, p := range []struct {
		prefix string
		mod    tea.KeyMod
	}{{"ctrl+", tea.ModCtrl}, {"shift+", tea.ModShift}, {"alt+", tea.ModAlt}} {
		if rest, ok := strings.CutPrefix(s, p.prefix); ok && len(rest) > 0 {
			s, mod = rest, mod|p.mod
		}
	}
	if c, ok := named[s]; ok {
		k := tea.KeyPressMsg{Code: c, Mod: mod}
		if c == tea.KeySpace && mod == 0 {
			k.Text = " "
		}
		return k
	}
	r := []rune(s)[0]
	if mod != 0 {
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	return tea.KeyPressMsg{Code: r, Text: s}
}

func TestEditor(t *testing.T) {
	var e Editor
	e.Multi = true
	e.Insert("héllo wörld\tx\x07")
	if e.String() != "héllo wörld x" || e.Pos != 13 {
		t.Fatalf("%q %d", e.String(), e.Pos)
	}
	for _, k := range []string{"ctrl+w", "backspace", "home", "right", "delete"} {
		e.Key(key(k), 20)
	}
	if e.String() != "hllo wörld" || e.Pos != 1 {
		t.Fatalf("%q %d", e.String(), e.Pos)
	}
	// Wrapping keeps a cell for the cursor; up and down move between visual rows.
	e.Set("abcdefgh\nij")
	if rows := e.rows(5); len(rows) != 3 || e.Height(5) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	e.Key(key("up"), 5)
	if e.Pos != 6 { // from column 2 of "ij" to column 2 of "efgh": "g"
		t.Fatalf("up: pos %d", e.Pos)
	}
	v := e.View(5, 2, func(s string) string { return "[" + s + "]" }, "", nil)
	if !slices.Equal(v, []string{"abcd", "ef[g]h"}) {
		t.Fatalf("view %q", v)
	}
	e.Click(1, 1, 5) // row 2 of the view (Off 0): "efgh", column 1
	if e.Pos != 5 {
		t.Fatalf("click: pos %d", e.Pos)
	}
	// A single-line editor turns newlines into spaces and ignores up and down.
	var s Editor
	s.Insert("a\nb")
	if s.String() != "a b" || s.Key(key("up"), 10) {
		t.Fatalf("single line %q", s.String())
	}
	// Placeholders.
	var p Editor
	if v := p.View(10, 1, nil, "type here", func(s ...string) string { return s[0] }); v[0] != "type here" {
		t.Fatalf("placeholder %q", v)
	}
}

func newTestForm() *Form {
	title := &Field{Kind: Text, ID: "title", Label: "Title", Required: true}
	body := &Field{Kind: Area, ID: "body", Label: "Body", Height: 2}
	body.Ed.Multi = true
	return New("Test", []Button{{ID: "ok", Label: "OK"}, {ID: "cancel", Label: "Cancel", Cancel: true}},
		title, body,
		&Field{Kind: Select, ID: "kind", Label: "Kind", Opts: []Option{{Value: "a", Label: "A"}, {Value: "b", Label: "B",
			Disabled: "no"}, {Value: "c", Label: "C"}}},
		&Field{Kind: Check, ID: "draft", Label: "draft"},
		&Field{Kind: List, ID: "pick", Label: "Pick", Height: 2, Opts: []Option{{Value: "x", Disabled: "gone"},
			{Value: "y"}, {Value: "z"}}})
}

func TestFormKeys(t *testing.T) {
	f := newTestForm()
	if ev, _ := f.Key(key("ctrl+s"), 40); ev != None || f.Err != "title is required" {
		t.Fatalf("required: %v %q", ev, f.Err)
	}
	for _, r := range "Hi" {
		f.Key(key(string(r)), 40)
	}
	f.Key(key("enter"), 40) // to the body
	f.Key(key("x"), 40)
	f.Key(key("enter"), 40) // a newline in an area
	f.Key(key("y"), 40)
	f.Key(key("tab"), 40)
	f.Key(key("right"), 40) // A → C: B is disabled
	f.Key(key("tab"), 40)
	f.Key(key("space"), 40)
	f.Key(key("tab"), 40)
	if f.Get("pick") != "y" { // the disabled first option was skipped
		t.Fatalf("pick %q", f.Get("pick"))
	}
	f.Key(key("down"), 40)
	if ev, id := f.Key(key("enter"), 40); ev != Submit || id != "ok" {
		t.Fatalf("enter on a list: %v %q", ev, id)
	}
	if f.Get("title") != "Hi" || f.Get("body") != "x\ny" || f.Get("kind") != "c" || f.Get("draft") != "true" ||
		f.Get("pick") != "z" {
		t.Fatalf("values %q %q %q %q %q", f.Get("title"), f.Get("body"), f.Get("kind"), f.Get("draft"), f.Get("pick"))
	}
	f.Key(key("tab"), 40) // the OK button, then Cancel
	f.Key(key("tab"), 40)
	if ev, id := f.Key(key("enter"), 40); ev != Cancel || id != "cancel" {
		t.Fatalf("cancel button: %v %q", ev, id)
	}
	if ev, _ := f.Key(key("esc"), 40); ev != Cancel {
		t.Fatal("esc")
	}
}

func TestFormView(t *testing.T) {
	f := newTestForm()
	f.Err = "BOOM: it failed"
	lines, hits := f.View(40, style.Default)
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > 40 {
			t.Errorf("line %d is %d cells", i, w)
		}
	}
	text := ansi.Strip(strings.Join(lines, "\n"))
	for _, want := range []string{"Test", "Title:", "‹ A ›", "[ ] draft", "Pick", "gone", "BOOM: it failed", "[ OK ]  [ Cancel ]"} {
		if !strings.Contains(text, want) {
			t.Errorf("view lacks %q:\n%s", want, text)
		}
	}
	// A click on each part: the checkbox toggles, the select cycles, a list item is
	// chosen (a disabled one is not), a button submits.
	find := func(field string, opt int, button string) Hit {
		for _, h := range hits {
			if button != "" && h.Button >= 0 && f.Buttons[h.Button].ID == button {
				return h
			}
			if field != "" && h.Field >= 0 && f.Fields[h.Field].ID == field && (opt < 0 || h.Opt == opt) {
				return h
			}
		}
		t.Fatalf("no hit %s %d %s", field, opt, button)
		return Hit{}
	}
	h := find("draft", -1, "")
	f.Click(h, h.X, h.Y, false, 40)
	h = find("kind", -1, "")
	f.Click(h, h.X, h.Y, false, 40)
	h = find("pick", 0, "")
	f.Click(h, h.X, h.Y, false, 40)
	if f.Get("draft") != "true" || f.Get("kind") != "c" || f.Get("pick") != "y" {
		t.Fatalf("clicks: %q %q %q", f.Get("draft"), f.Get("kind"), f.Get("pick"))
	}
	f.Wheel(find("pick", 1, ""), 1, 40) // z shows after a scroll
	_, hits = f.View(40, style.Default)
	h = find("pick", 2, "")
	if ev, _ := f.Click(h, h.X, h.Y, true, 40); ev != None || f.Get("pick") != "z" { // title still empty
		t.Fatalf("double click: %v %q", ev, f.Err)
	}
	h = find("title", -1, "")
	f.Click(h, h.X, h.Y, false, 40)
	f.Paste("pasted\ntitle")
	h = find("", -1, "ok")
	if ev, id := f.Click(h, h.X, h.Y, false, 40); ev != Submit || id != "ok" || f.Get("title") != "pasted title" {
		t.Fatalf("button: %v %q %q", ev, id, f.Get("title"))
	}
}
