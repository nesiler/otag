package form

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Editor is a small text editor: runes and a cursor, soft-wrapped at the width it is
// drawn with. A single-line editor shows the visual row that holds the cursor.
type Editor struct {
	Value []rune
	Pos   int  // cursor: an index into Value
	Off   int  // first visual row shown
	Multi bool // newlines allowed (Insert keeps them)
}

// String returns the text.
func (e *Editor) String() string { return string(e.Value) }

// Set replaces the text and puts the cursor at its end.
func (e *Editor) Set(s string) {
	e.Value, e.Off = nil, 0
	e.Insert(s)
}

// Insert inserts s at the cursor. A single-line editor turns newlines into spaces;
// other control characters are dropped.
func (e *Editor) Insert(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var rs []rune
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			if e.Multi {
				r = '\n'
			} else {
				r = ' '
			}
		case r == '\t':
			r = ' '
		case r < ' ' || r == 0x7f:
			continue
		}
		rs = append(rs, r)
	}
	e.Pos = min(max(e.Pos, 0), len(e.Value))
	v := make([]rune, 0, len(e.Value)+len(rs))
	v = append(append(append(v, e.Value[:e.Pos]...), rs...), e.Value[e.Pos:]...)
	e.Value, e.Pos = v, e.Pos+len(rs)
}

// Key edits with a key press and reports whether it used it. Newlines are the
// caller's choice (Insert("\n")); up and down move between visual rows of width w.
func (e *Editor) Key(k tea.KeyPressMsg, w int) bool {
	switch k.String() {
	case "left", "ctrl+b":
		e.Pos = max(0, e.Pos-1)
	case "right", "ctrl+f":
		e.Pos = min(len(e.Value), e.Pos+1)
	case "home", "ctrl+a":
		e.Pos = e.lineStart()
	case "end", "ctrl+e":
		e.Pos = e.lineEnd()
	case "alt+left", "alt+b":
		e.Pos = e.wordBack()
	case "alt+right", "alt+f":
		for e.Pos < len(e.Value) && e.Value[e.Pos] == ' ' {
			e.Pos++
		}
		for e.Pos < len(e.Value) && e.Value[e.Pos] != ' ' && e.Value[e.Pos] != '\n' {
			e.Pos++
		}
	case "backspace", "ctrl+h":
		if e.Pos > 0 {
			e.cut(e.Pos-1, e.Pos)
		}
	case "delete", "ctrl+d":
		if e.Pos < len(e.Value) {
			e.cut(e.Pos, e.Pos+1)
		}
	case "ctrl+w", "alt+backspace":
		e.cut(e.wordBack(), e.Pos)
	case "ctrl+u":
		e.cut(e.lineStart(), e.Pos)
	case "up", "down":
		if !e.Multi {
			return false
		}
		rows := e.rows(w)
		r, c := e.at(rows)
		if k.String() == "up" {
			r--
		} else {
			r++
		}
		if r < 0 || r >= len(rows) {
			return false
		}
		e.Pos = rows[r].index(e.Value, c)
	default:
		if k.Text == "" || k.Mod&^tea.ModShift != 0 {
			return false
		}
		e.Insert(k.Text)
	}
	return true
}

func (e *Editor) cut(from, to int) {
	if from >= to {
		return
	}
	e.Value = append(e.Value[:from:from], e.Value[to:]...)
	e.Pos = from
}

func (e *Editor) lineStart() int {
	i := e.Pos
	for i > 0 && e.Value[i-1] != '\n' {
		i--
	}
	return i
}

func (e *Editor) lineEnd() int {
	i := e.Pos
	for i < len(e.Value) && e.Value[i] != '\n' {
		i++
	}
	return i
}

func (e *Editor) wordBack() int {
	i := e.Pos
	for i > 0 && e.Value[i-1] == ' ' {
		i--
	}
	for i > 0 && e.Value[i-1] != ' ' && e.Value[i-1] != '\n' {
		i--
	}
	return i
}

// row is one visual row: Value[from:to], without its newline.
type row struct{ from, to int }

// index is the rune index of cell column c in the row.
func (r row) index(v []rune, c int) int {
	x := 0
	for i := r.from; i < r.to; i++ {
		rw := runeWidth(v[i])
		if x+rw > c {
			return i
		}
		x += rw
	}
	return r.to
}

func runeWidth(r rune) int { return max(1, ansi.StringWidth(string(r))) }

// rows wraps the text at w cells. A row that ends in a newline, or the last row, keeps
// a cell for the cursor.
func (e *Editor) rows(w int) []row {
	w = max(w, 2)
	var out []row
	from, x := 0, 0
	for i, r := range e.Value {
		if r == '\n' {
			out = append(out, row{from, i})
			from, x = i+1, 0
			continue
		}
		rw := runeWidth(r)
		if x+rw > w-1 && i > from { // the last cell stays free for the cursor
			out = append(out, row{from, i})
			from, x = i, 0
		}
		x += rw
	}
	return append(out, row{from, len(e.Value)})
}

// at returns the cursor's visual row and cell column.
func (e *Editor) at(rows []row) (int, int) {
	for i, r := range rows {
		last := i == len(rows)-1
		if e.Pos >= r.from && (e.Pos < r.to || (e.Pos == r.to && (last || rows[i+1].from > r.to))) {
			c := 0
			for j := r.from; j < e.Pos; j++ {
				c += runeWidth(e.Value[j])
			}
			return i, c
		}
	}
	return len(rows) - 1, 0
}

// Height is the number of visual rows at width w.
func (e *Editor) Height(w int) int { return len(e.rows(w)) }

// View renders h rows of w cells, scrolled so the cursor shows; cursor draws the cell
// under it (nil: no cursor). placeholder shows, through dim, when the text is empty.
func (e *Editor) View(w, h int, cursor func(string) string, placeholder string, dim func(...string) string) []string {
	rows := e.rows(w)
	cr, _ := e.at(rows)
	if cursor != nil {
		if cr < e.Off {
			e.Off = cr
		}
		if cr >= e.Off+h {
			e.Off = cr - h + 1
		}
	}
	e.Off = max(0, min(e.Off, len(rows)-h))
	out := make([]string, h)
	for i := range out {
		ri := e.Off + i
		if ri >= len(rows) {
			break
		}
		r := rows[ri]
		var b strings.Builder
		if cursor != nil && ri == cr {
			b.WriteString(string(e.Value[r.from:e.Pos]))
			under := " "
			if e.Pos < r.to {
				under = string(e.Value[e.Pos])
			}
			b.WriteString(cursor(under))
			if e.Pos < r.to {
				b.WriteString(string(e.Value[e.Pos+1 : r.to]))
			}
		} else {
			b.WriteString(string(e.Value[r.from:r.to]))
		}
		out[i] = b.String()
	}
	if len(e.Value) == 0 && placeholder != "" && h > 0 {
		if cursor != nil {
			out[0] = cursor(" ") + dim(ansi.Truncate(placeholder, w-1, "…"))
		} else {
			out[0] = dim(ansi.Truncate(placeholder, w, "…"))
		}
	}
	return out
}

// Click moves the cursor to the cell (x, y) of a view w cells wide.
func (e *Editor) Click(x, y, w int) {
	rows := e.rows(w)
	ri := min(max(e.Off+y, 0), len(rows)-1)
	e.Pos = rows[ri].index(e.Value, max(0, x))
}

// Scroll moves the view n visual rows (the cursor stays).
func (e *Editor) Scroll(n, w, h int) {
	e.Off = max(0, min(e.Off+n, e.Height(w)-h))
}
