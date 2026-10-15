// Package form is the TUI's dialog kit: text fields, text areas, selects, checkboxes,
// pick lists, read-only notes and buttons. Tab / shift+Tab move between them, ctrl+s
// or Enter on a button submits, Esc cancels. Every field, list item and button is a
// hit rectangle, so the root model routes clicks to it through its pure layout.
//
// A form never runs anything: Key and Click return an Event, and the dialog that owns
// the form builds the CLI arguments from its values.
package form

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/tui/style"
)

// Kind is a field kind.
type Kind int

// Field kinds.
const (
	Text   Kind = iota // one line
	Area               // several lines (Enter is a newline)
	Select             // one of Opts, cycled with ← → or space
	Check              // a checkbox
	List               // a pick list of Opts, Height rows
	Note               // read-only text, Height rows at most
)

// Option is an item of a Select or a List. A disabled one says why and cannot be
// chosen.
type Option struct {
	Value, Label, Detail, Disabled string
}

// Field is one row (or block of rows) of a form.
type Field struct {
	Kind     Kind
	ID       string
	Label    string
	Required bool
	Ed       Editor // Text, Area
	Height   int    // Area, List, Note: rows shown
	Opts     []Option
	Sel      int    // Select, List
	On       bool   // Check
	Text     string // Note
	Off      int    // List, Note: first row shown
	Keep     bool   // List: keep a disabled preselection (submitting it says why)
	// Placeholder shows, dim, in an empty Text or Area ("" = a blank line).
	Placeholder string
}

// Value is the field's value: the text, the chosen option, or "true" / "".
func (f *Field) Value() string {
	switch f.Kind {
	case Text, Area:
		return f.Ed.String()
	case Select, List:
		if f.Sel >= 0 && f.Sel < len(f.Opts) {
			return f.Opts[f.Sel].Value
		}
	case Check:
		if f.On {
			return "true"
		}
	case Note:
		return f.Text
	}
	return ""
}

func (f *Field) focusable() bool { return f.Kind != Note }

// Button is a button; Cancel buttons cancel without checks.
type Button struct {
	ID, Label string
	Cancel    bool
}

// Form is a dialog's content.
type Form struct {
	Title   string
	Fields  []*Field
	Buttons []Button
	Focus   int    // index into the focusables: the fields that can focus, then the buttons
	Err     string // a failed check or the CLI's error, shown above the buttons
	Status  string // "working…" while the action runs
}

// Event is what a key or click asks of the form's owner.
type Event int

// Events.
const (
	None   Event = iota
	Submit       // with the button's ID
	Cancel
)

// New returns a form with the focus on its first field.
func New(title string, buttons []Button, fields ...*Field) *Form {
	f := &Form{Title: title, Fields: fields, Buttons: buttons}
	for _, fl := range fields {
		if fl.Kind != List || len(fl.Opts) == 0 {
			continue
		}
		fl.Sel = min(max(fl.Sel, 0), len(fl.Opts)-1)
		if fl.Opts[fl.Sel].Disabled != "" && !fl.Keep { // the first one that can be chosen
			fl.Sel = -1
			fl.move(1)
			fl.Sel = max(fl.Sel, 0)
		}
		fl.show()
	}
	return f
}

// Field returns the field with id (nil: none).
func (f *Form) Field(id string) *Field {
	for _, fl := range f.Fields {
		if fl.ID == id {
			return fl
		}
	}
	return nil
}

// Get returns the value of field id.
func (f *Form) Get(id string) string {
	if fl := f.Field(id); fl != nil {
		return fl.Value()
	}
	return ""
}

// focusables lists the focus targets: field indexes, then -1-i for button i.
func (f *Form) focusables() []int {
	var out []int
	for i, fl := range f.Fields {
		if fl.focusable() {
			out = append(out, i)
		}
	}
	for i := range f.Buttons {
		out = append(out, -1-i)
	}
	return out
}

// current returns the focused field (nil on a button) and button (-1 on a field).
func (f *Form) current() (*Field, int) {
	fs := f.focusables()
	if len(fs) == 0 {
		return nil, -1
	}
	f.Focus = min(max(f.Focus, 0), len(fs)-1)
	if t := fs[f.Focus]; t >= 0 {
		return f.Fields[t], -1
	}
	return nil, -1 - fs[f.Focus]
}

// FocusField moves the focus to field id.
func (f *Form) FocusField(id string) {
	for i, t := range f.focusables() {
		if t >= 0 && f.Fields[t].ID == id {
			f.Focus = i
		}
	}
}

// FocusButton moves the focus to button id.
func (f *Form) FocusButton(id string) {
	for i, t := range f.focusables() {
		if t < 0 && f.Buttons[-1-t].ID == id {
			f.Focus = i
		}
	}
}

func (f *Form) next(d int) {
	n := len(f.focusables())
	if n > 0 {
		f.Focus = (f.Focus + d + n) % n
	}
}

// defaultButton is the first button that does not cancel.
func (f *Form) defaultButton() string {
	for _, b := range f.Buttons {
		if !b.Cancel {
			return b.ID
		}
	}
	return ""
}

// submit checks the required fields; Cancel buttons skip the checks.
func (f *Form) submit(id string) (Event, string) {
	for _, b := range f.Buttons {
		if b.ID == id && b.Cancel {
			return Cancel, id
		}
	}
	for _, fl := range f.Fields {
		switch {
		case fl.Required && (fl.Kind == Text || fl.Kind == Area) && strings.TrimSpace(fl.Ed.String()) == "":
			f.Err = strings.ToLower(fl.Label) + " is required"
		case fl.Kind == List && (fl.Sel < 0 || fl.Sel >= len(fl.Opts)):
			f.Err = "choose " + strings.ToLower(fl.Label)
		case fl.Kind == List && fl.Opts[fl.Sel].Disabled != "":
			f.Err = fl.Opts[fl.Sel].Value + " is not available: " + fl.Opts[fl.Sel].Disabled
		default:
			continue
		}
		f.FocusField(fl.ID)
		return None, ""
	}
	return Submit, id
}

// Key handles a key press. Width is the form's content width (Area rows wrap there).
func (f *Form) Key(k tea.KeyPressMsg, width int) (Event, string) {
	fl, btn := f.current()
	s := k.String()
	switch s {
	case "esc":
		return Cancel, ""
	case "ctrl+s":
		return f.submit(f.defaultButton())
	case "tab":
		f.next(1)
		return None, ""
	case "shift+tab":
		f.next(-1)
		return None, ""
	}
	if fl == nil {
		switch s {
		case "enter", "space":
			if btn >= 0 && btn < len(f.Buttons) {
				return f.submit(f.Buttons[btn].ID)
			}
		case "left", "up", "h", "k":
			f.next(-1)
		case "right", "down", "l", "j":
			f.next(1)
		}
		return None, ""
	}
	switch fl.Kind {
	case Text:
		switch s {
		case "enter", "down":
			f.next(1)
			return None, ""
		case "up":
			f.next(-1)
			return None, ""
		}
		fl.Ed.Key(k, f.editorWidth(fl, width))
	case Area:
		switch s {
		case "enter", "alt+enter", "shift+enter", "ctrl+j":
			fl.Ed.Insert("\n")
			return None, ""
		}
		if !fl.Ed.Key(k, f.editorWidth(fl, width)) {
			switch s {
			case "up":
				f.next(-1)
			case "down":
				f.next(1)
			}
		}
	case Select:
		switch s {
		case "left", "h":
			fl.move(-1)
		case "right", "l", "space":
			fl.move(1)
		case "enter", "down":
			f.next(1)
		case "up":
			f.next(-1)
		}
	case Check:
		switch s {
		case "space", "x":
			fl.On = !fl.On
		case "enter", "down":
			f.next(1)
		case "up":
			f.next(-1)
		}
	case List:
		switch s {
		case "up", "k":
			fl.move(-1)
		case "down", "j":
			fl.move(1)
		case "home", "g":
			fl.Sel = len(fl.Opts)
			fl.move(1)
		case "end", "G":
			fl.Sel = -1
			fl.move(-1)
		case "enter":
			return f.submit(f.defaultButton())
		}
		fl.show()
	}
	return None, ""
}

// Paste inserts pasted text into the focused text field.
func (f *Form) Paste(s string) {
	if fl, _ := f.current(); fl != nil && (fl.Kind == Text || fl.Kind == Area) {
		fl.Ed.Insert(s)
	}
}

// move moves a Select or List choice by d, skipping disabled options (a Select
// wraps around).
func (fl *Field) move(d int) {
	n := len(fl.Opts)
	if n == 0 {
		return
	}
	i := fl.Sel
	for range n {
		i += d
		if fl.Kind == Select {
			i = (i + n) % n
		}
		if i < 0 || i >= n {
			return
		}
		if fl.Opts[i].Disabled == "" {
			fl.Sel = i
			return
		}
	}
}

// show scrolls a List so its choice is visible.
func (fl *Field) show() {
	h := max(1, fl.Height)
	if fl.Sel < fl.Off {
		fl.Off = fl.Sel
	}
	if fl.Sel >= fl.Off+h {
		fl.Off = fl.Sel - h + 1
	}
	fl.Off = max(0, min(fl.Off, len(fl.Opts)-h))
}

// Hit is a clickable rectangle of the form's view, relative to its top-left cell.
type Hit struct {
	X, Y, W, H int
	Field      int // field index, -1 for a button
	Button     int // button index, -1 for a field
	Opt        int // List: option index; -1 otherwise
}

// labelWidth is the width of the label column of Text and Select rows.
func (f *Form) labelWidth() int {
	w := 0
	for _, fl := range f.Fields {
		if fl.Kind == Text || fl.Kind == Select || fl.Kind == Area {
			w = max(w, ansi.StringWidth(fl.Label)+2)
		}
	}
	return w
}

func (f *Form) editorWidth(fl *Field, width int) int {
	return max(4, width-f.labelWidth())
}

// View renders the form width cells wide and returns its lines and hit rectangles.
func (f *Form) View(width int, th style.Theme) ([]string, []Hit) {
	var lines []string
	var hits []Hit
	cur, curBtn := f.current()
	lw := f.labelWidth()
	cursor := func(s string) string { return th.Selected.Render(s) }
	label := func(fl *Field, focused bool) string {
		l := style.Fit(fl.Label+":", lw)
		if focused {
			return th.Accent.Render(l)
		}
		return th.Dim.Render(l)
	}
	if f.Title != "" {
		lines = append(lines, th.Bold.Render(ansi.Truncate(f.Title, width, "…")), "")
	}
	for i, fl := range f.Fields {
		focused := fl == cur
		y := len(lines)
		switch fl.Kind {
		case Note:
			text := wrap(fl.Text, width)
			h := len(text)
			if fl.Height > 0 && h > fl.Height {
				h = fl.Height
				fl.Off = max(0, min(fl.Off, len(text)-h))
				text = text[fl.Off : fl.Off+h]
			}
			if fl.Label != "" {
				lines = append(lines, th.Dim.Render(fl.Label))
				y++
			}
			lines = append(lines, text...)
			hits = append(hits, Hit{0, y, width, h, i, -1, -1})
			if i < len(f.Fields)-1 {
				lines = append(lines, "")
			}
		case Text, Area:
			ew := f.editorWidth(fl, width)
			h := 1
			if fl.Kind == Area {
				h = max(1, fl.Height)
			}
			var c func(string) string
			if focused {
				c = cursor
			}
			rows := fl.Ed.View(ew, h, c, fl.Placeholder, th.Dim.Render)
			for r, row := range rows {
				l := strings.Repeat(" ", lw)
				if r == 0 {
					l = label(fl, focused)
				}
				if !focused && row == "" && r == 0 && fl.Placeholder == "" {
					row = th.Dim.Render(strings.Repeat("_", min(ew, 20)))
				}
				lines = append(lines, l+row)
			}
			hits = append(hits, Hit{lw, y, ew, h, i, -1, -1})
		case Select:
			v := ""
			if fl.Sel >= 0 && fl.Sel < len(fl.Opts) {
				v = fl.Opts[fl.Sel].Label
			}
			sel := "‹ " + v + " ›"
			if focused {
				sel = th.Selected.Render(sel)
			}
			lines = append(lines, label(fl, focused)+sel)
			hits = append(hits, Hit{lw, y, ansi.StringWidth("‹ " + v + " ›"), 1, i, -1, -1})
		case Check:
			box := "[ ] "
			if fl.On {
				box = "[x] "
			}
			l := box + fl.Label
			if focused {
				l = th.Selected.Render(box) + th.Accent.Render(fl.Label)
			}
			lines = append(lines, l)
			hits = append(hits, Hit{0, y, min(width, ansi.StringWidth(box+fl.Label)), 1, i, -1, -1})
		case List:
			lines = append(lines, th.Dim.Render(fl.Label))
			y++
			h := max(1, fl.Height)
			fl.Off = max(0, min(fl.Off, len(fl.Opts)-h))
			for r := 0; r < h; r++ {
				oi := fl.Off + r
				if oi >= len(fl.Opts) {
					lines = append(lines, "")
					continue
				}
				o := fl.Opts[oi]
				mark := "  "
				if oi == fl.Sel {
					mark = "› "
				}
				text := mark + o.Label
				if o.Detail != "" {
					text += "  " + o.Detail
				}
				if o.Disabled != "" {
					text += " — " + o.Disabled
				}
				text = style.Fit(text, width)
				switch {
				case o.Disabled != "":
					text = th.Dim.Render(text)
				case oi == fl.Sel && focused:
					text = th.Selected.Render(text)
				case oi == fl.Sel:
					text = th.Bold.Render(text)
				}
				lines = append(lines, text)
				hits = append(hits, Hit{0, y + r, width, 1, i, -1, oi})
			}
			if len(fl.Opts) > h {
				lines = append(lines, th.Dim.Render(fmt.Sprintf("  %d-%d of %d (↑↓, wheel)", fl.Off+1,
					min(fl.Off+h, len(fl.Opts)), len(fl.Opts))))
			}
		}
	}
	lines = append(lines, "")
	if f.Err != "" {
		for _, l := range wrap(f.Err, width) {
			lines = append(lines, th.Bad.Render(l))
		}
	}
	if f.Status != "" {
		lines = append(lines, th.Warn.Render(ansi.Truncate(f.Status, width, "…")))
	}
	y := len(lines)
	x := 0
	var row strings.Builder
	for i, b := range f.Buttons {
		l := "[ " + b.Label + " ]"
		bw := ansi.StringWidth(l)
		if x+bw > width {
			break
		}
		if i > 0 {
			row.WriteString("  ")
		}
		if cur == nil && curBtn == i {
			row.WriteString(th.Selected.Render(l))
		} else {
			row.WriteString(l)
		}
		hits = append(hits, Hit{x, y, bw, 1, -1, i, -1})
		x += bw + 2
	}
	lines = append(lines, row.String())
	return lines, hits
}

// Click handles a left click on hit h at the cell (x, y) relative to the form;
// double is a second click on the same spot.
func (f *Form) Click(h Hit, x, y int, double bool, width int) (Event, string) {
	if h.Button >= 0 {
		if h.Button < len(f.Buttons) {
			f.FocusButton(f.Buttons[h.Button].ID)
			return f.submit(f.Buttons[h.Button].ID)
		}
		return None, ""
	}
	if h.Field < 0 || h.Field >= len(f.Fields) {
		return None, ""
	}
	fl := f.Fields[h.Field]
	if fl.focusable() {
		f.FocusField(fl.ID)
	}
	switch fl.Kind {
	case Text, Area:
		fl.Ed.Click(x-h.X, y-h.Y, f.editorWidth(fl, width))
	case Select:
		fl.move(1)
	case Check:
		fl.On = !fl.On
	case List:
		if h.Opt >= 0 && h.Opt < len(fl.Opts) && fl.Opts[h.Opt].Disabled == "" {
			fl.Sel = h.Opt
			if double {
				return f.submit(f.defaultButton())
			}
		}
	}
	return None, ""
}

// Wheel scrolls the List, Note or Area of hit h by n rows.
func (f *Form) Wheel(h Hit, n, width int) {
	if h.Field < 0 || h.Field >= len(f.Fields) {
		return
	}
	fl := f.Fields[h.Field]
	switch fl.Kind {
	case List, Note:
		fl.Off = max(0, fl.Off+n) // View clamps it
	case Area:
		fl.Ed.Scroll(n, f.editorWidth(fl, width), max(1, fl.Height))
	}
}

// wrap splits text into lines at most w cells wide.
func wrap(text string, w int) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if w > 0 && ansi.StringWidth(l) > w {
			out = append(out, strings.Split(ansi.Wrap(l, w, ""), "\n")...)
			continue
		}
		out = append(out, l)
	}
	return out
}
