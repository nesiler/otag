package tui

import (
	"strconv"

	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/tui/tabs"
)

// Screen limits.
const (
	narrowWidth = 90 // below: one pane at a time
	minWidth    = 60 // below (or below minHeight): "terminal too small"
	minHeight   = 16
	minTree     = 24 // divider clamp: tree at least this wide...
	minPane     = 30 // ...and the right pane at least this wide
)

// Rect is a screen rectangle in cells.
type Rect struct{ X, Y, W, H int }

// Contains reports whether the cell (x, y) is inside r.
func (r Rect) Contains(x, y int) bool { return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H }

// Intersect is the part of r inside o (empty: zero width or height).
func (r Rect) Intersect(o Rect) Rect {
	x0, y0 := max(r.X, o.X), max(r.Y, o.Y)
	x1, y1 := min(r.X+r.W, o.X+o.W), min(r.Y+r.H, o.Y+o.H)
	return Rect{x0, y0, max(0, x1-x0), max(0, y1-y0)}
}

// regionKind names a clickable or drawable area.
type regionKind int

const (
	// Blocks (z 0) tile the screen; each is one Lip Gloss layer in View.
	regHeader regionKind = iota
	regTree
	regDivider
	regTabs
	regPane // the right pane: title bar and body
	regFooter
	regTooSmall
	regBox // the message box: separator and text rows
	// Parts (z >= 1) sit on top of a block.
	regCounter  // a header counter; ID: needs | inbox | deliver
	regTab      // a tab label; ID: the tab number
	regRow      // a tree row; ID: the row key
	regToggle   // a row's expansion arrow; ID: the row key
	regHint     // a footer button; ID: its index in hints()
	regToast    // the toast at the footer's right end
	regItemAct  // an inline action button of Needs or Inbox; ID: item:action
	regBoxTo    // the message box's recipient chip
	regBoxKind  // its kind chip
	regBoxText  // its text
	regBoxSend  // its [Send] button
	regHelp     // the help overlay (z 10)
	regDialog   // a dialog (z 20)
	regDlgPart  // a dialog's field, list item or button (z 21); ID: the form hit's index
	regMenu     // a context menu (z 30)
	regMenuItem // a menu item (z 31); ID: its index
	regKindCount
)

var blockIDs = map[regionKind]string{regHeader: "header", regTree: "tree", regDivider: "divider", regTabs: "tabs",
	regPane: "pane", regFooter: "footer", regTooSmall: "toosmall", regBox: "box", regHelp: "help", regDialog: "dialog", regMenu: "menu"}

// region is one rectangle of the layout.
type region struct {
	kind regionKind
	id   string
	rect Rect
	z    int
}

// screen is the pure layout of a model: Update hit-tests clicks with it and View draws
// into the same rectangles.
type screen struct {
	regions  []region // lowest z first
	tooSmall bool
	narrow   bool
	showTree bool // narrow mode: the tree (not the right pane) is shown
	tree     Rect // tree rows
	pane     Rect // right pane including its title bar
	body     Rect // right pane below the title bar
}

// hit returns the topmost region at (x, y).
func (s screen) hit(x, y int) (region, bool) {
	for i := len(s.regions) - 1; i >= 0; i-- {
		if s.regions[i].rect.Contains(x, y) {
			return s.regions[i], true
		}
	}
	return region{}, false
}

func (s screen) find(k regionKind) (region, bool) {
	for _, r := range s.regions {
		if r.kind == k {
			return r, true
		}
	}
	return region{}, false
}

// treeWidth is the tree's width for a screen w cells wide: m.divider clamped to
// [minTree, w-minPane-1], or about 38% by default.
func treeWidth(divider, w int) int {
	if divider <= 0 {
		divider = w * 38 / 100
	}
	return max(minTree, min(divider, w-minPane-1))
}

// layout computes the screen of m. It reads only model state, never the terminal.
func layout(m *Model) screen {
	w, h := m.w, m.h
	var s screen
	add := func(k regionKind, id string, r Rect, z int) {
		if r.W > 0 && r.H > 0 {
			s.regions = append(s.regions, region{kind: k, id: id, rect: r, z: z})
		}
	}
	if w < minWidth || h < minHeight {
		s.tooSmall = true
		add(regTooSmall, blockIDs[regTooSmall], Rect{0, 0, w, h}, 0)
		return s
	}
	g := boxGeom(m)
	bodyH := h - 2 - g.h
	add(regHeader, blockIDs[regHeader], Rect{0, 0, w, 1}, 0)
	s.narrow = w < narrowWidth
	s.showTree = !s.narrow || !m.narrowPane
	paneX, paneW := 0, w
	if !s.narrow {
		tw := treeWidth(m.divider, w)
		s.tree = Rect{0, 1, tw, bodyH}
		add(regTree, blockIDs[regTree], s.tree, 0)
		add(regDivider, blockIDs[regDivider], Rect{tw, 1, 1, bodyH}, 0)
		paneX, paneW = tw+1, w-tw-1
	} else if s.showTree {
		s.tree = Rect{0, 1, w, bodyH}
		add(regTree, blockIDs[regTree], s.tree, 0)
	}
	if !s.narrow || !s.showTree {
		add(regTabs, blockIDs[regTabs], Rect{paneX, 1, paneW, 1}, 0)
		s.pane = Rect{paneX, 2, paneW, bodyH - 1}
		s.body = Rect{paneX, 3, paneW, bodyH - 2}
		add(regPane, blockIDs[regPane], s.pane, 0)
	}
	add(regBox, blockIDs[regBox], Rect{0, g.y, w, g.h}, 0)
	add(regFooter, blockIDs[regFooter], Rect{0, h - 1, w, 1}, 0)

	// Parts.
	x := 0
	for _, seg := range headerSegments(m) {
		sw := ansi.StringWidth(seg.text)
		if seg.id != "" {
			add(regCounter, seg.id, Rect{x, 0, min(sw, w-x), 1}, 1)
		}
		x += sw
	}
	if !s.narrow || !s.showTree {
		x = paneX
		for i, t := range tabs.All {
			lw := ansi.StringWidth(tabLabel(t, m))
			add(regTab, strconv.Itoa(i), Rect{x, 1, min(lw, paneX+paneW-x), 1}, 1)
			x += lw + 1 // the separator
		}
	}
	if s.showTree && m.tree.Rows != nil {
		off := m.tree.VisibleOff(s.tree.H)
		for i := off; i < len(m.tree.Rows) && i-off < s.tree.H; i++ {
			r := &m.tree.Rows[i]
			y := s.tree.Y + i - off
			add(regRow, r.Key, Rect{s.tree.X, y, s.tree.W, 1}, 1)
			if r.CanExpand {
				from, to := r.ToggleCells()
				// Deep rows push the arrow past the tree's edge: only its visible part counts
				// (add drops an empty one).
				add(regToggle, r.Key, Rect{s.tree.X + from, y, to - from, 1}.Intersect(s.tree), 2)
			}
		}
	}
	if isItemTab(m.tab) && (!s.narrow || !s.showTree) && s.body.H > 0 {
		lines, buttons, _ := m.itemView(m.tab, false)
		off := max(0, min(m.scroll[m.tab], len(lines)-s.body.H))
		for _, b := range buttons {
			if b.line >= off && b.line < off+s.body.H {
				r := Rect{s.body.X + b.x, s.body.Y + b.line - off, b.w, 1}.Intersect(s.body)
				add(regItemAct, strconv.Itoa(b.item)+":"+strconv.Itoa(b.act), r, 1)
			}
		}
	}
	ty := g.y + 1
	add(regBoxTo, "to", Rect{0, ty, g.chipW, 1}, 1)
	add(regBoxKind, "kind", Rect{g.chipW, ty, g.kindW, 1}, 1)
	add(regBoxText, "text", Rect{g.textX, ty, g.textW, g.rows}, 1)
	add(regBoxSend, "send", Rect{g.sendX, ty, w - g.sendX, 1}, 1)
	tw := 0
	if t := m.footerRight(); t != "" {
		tw = min(w, ansi.StringWidth(t))
		if m.toast.text != "" {
			add(regToast, "toast", Rect{w - tw, h - 1, tw, 1}, 1)
		}
	}
	x = 0
	for i, b := range hints() {
		lw := ansi.StringWidth(hintLabel(b))
		if x+lw > w-tw-1 {
			break
		}
		add(regHint, strconv.Itoa(i), Rect{x, h - 1, lw, 1}, 1)
		x += lw + 2
	}
	if m.help {
		r, _ := m.helpBox()
		add(regHelp, blockIDs[regHelp], r, 10)
	}
	if m.dlg != nil {
		r, _, hits := m.dialogView()
		add(regDialog, blockIDs[regDialog], r, 20)
		inner := Rect{r.X + 2, r.Y + 1, r.W - 4, r.H - 2}
		for i, ht := range hits {
			add(regDlgPart, strconv.Itoa(i), Rect{inner.X + ht.X, inner.Y + ht.Y, ht.W, ht.H}.Intersect(inner), 21)
		}
	}
	if m.menu != nil {
		r, lines, first := m.menuView()
		add(regMenu, blockIDs[regMenu], r, 30)
		for i := range lines {
			add(regMenuItem, strconv.Itoa(first+i), Rect{r.X + 2, r.Y + 1 + i, r.W - 4, 1}, 31)
		}
	}
	return s
}
