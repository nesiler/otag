package tui

import (
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/fixture"
	"github.com/nesiler/otag/internal/tui/style"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

// rclick is a right click at x, y.
func (h *harness) rclick(x, y int) {
	h.t.Helper()
	h.send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})
	h.send(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseRight})
}

// rowMenuOf right-clicks the row key and returns the labels of the open menu.
func (h *harness) rowMenuOf(key string) []string {
	h.t.Helper()
	x, y := h.at(regRow, key)
	h.rclick(x, y)
	if h.m.menu == nil {
		h.t.Fatalf("no menu on %s", key)
	}
	return h.menuLabels()
}

func (h *harness) menuLabels() []string {
	var out []string
	for _, it := range h.m.menu.items {
		out = append(out, it.label)
	}
	return out
}

// menuIndex is the index of the open menu's item called label.
func (h *harness) menuIndex(label string) int {
	h.t.Helper()
	for i, it := range h.m.menu.items {
		if it.label == label {
			return i
		}
	}
	h.t.Fatalf("no menu item %q in %v", label, h.menuLabels())
	return -1
}

// menuClick clicks the menu item called label (it must be on screen).
func (h *harness) menuClick(label string) {
	h.t.Helper()
	x, y := h.at(regMenuItem, strconv.Itoa(h.menuIndex(label)))
	h.click(x, y)
}

// Every item of a context menu runs the code of its key: the same calls, dialogs,
// tabs and attaches as pressing the key on a harness that selected the row.
func TestMenuRoutesLikeKeys(t *testing.T) {
	for _, c := range []struct{ row, label, key string }{
		{"t:al-13", "Start run…", "s"}, {"t:al-13", "Edit…", "e"}, {"t:al-13", "Block…", "b"},
		{"t:al-13", "Cancel…", "x"}, {"t:al-15", "Mark ready", "R"}, {"t:al-16", "Mark ready", "R"},
		{"t:al-9", "Deliver…", "D"}, {"t:al-14", "Reply", "r"}, {"r:al-14-r1", "Reply", "r"},
		{"r:al-12-r2", "Stop…", "S"}, {"r:al-12-r2", "Attach", "enter"}, {"r:al-12-r2", "Type into pane", "i"},
		{"r:al-12-r2", "Details", "2"}, {"t:al-13", "Message task", "m"},
		{"p:alpha", "New task…", "n"}, {"p:alpha", "Sync registry", "Y"}, {"p:alpha", "Show closed tasks", "."},
		{"p:alpha", "Stats", "5"}, {"p:alpha", "Filter…", "/"}, {"p:alpha", "Collapse", "space"},
		{"c:alpha", "Attach", "enter"}, {"g:deliver:alpha", "Deliver…", "D"},
	} {
		t.Run(c.row+" "+c.label, func(t *testing.T) {
			a := newHarness(t, 120, 40)
			a.rowMenuOf(c.row)
			if a.selected() != c.row {
				t.Fatalf("right click selected %s", a.selected())
			}
			a.menuClick(c.label)
			if a.m.menu != nil {
				t.Fatal("the menu stays open after an item")
			}
			b := newHarness(t, 120, 40)
			b.selectRow(c.row)
			b.key(c.key)
			if !reflect.DeepEqual(a.calls, b.calls) || !reflect.DeepEqual(a.attaches, b.attaches) ||
				(a.m.dlg != nil) != (b.m.dlg != nil) || a.m.tab != b.m.tab || a.m.typ.on != b.m.typ.on ||
				a.m.focus != b.m.focus || a.m.filtering != b.m.filtering || a.m.toast.text != b.m.toast.text ||
				a.m.tree.ShowClosed != b.m.tree.ShowClosed || len(a.m.tree.Rows) != len(b.m.tree.Rows) {
				t.Fatalf("menu and key differ:\ncalls %v / %v\nattach %v / %v\ndlg %v / %v\ntab %d / %d toast %q / %q",
					a.calls, b.calls, a.attaches, b.attaches, a.m.dlg != nil, b.m.dlg != nil, a.m.tab, b.m.tab,
					a.m.toast.text, b.m.toast.text)
			}
			if a.m.dlg != nil && a.m.dlg.form == nil {
				t.Fatal("dialog without a form")
			}
		})
	}
}

// A disabled item shows why in the menu; choosing it says why and keeps the menu.
func TestMenuDisabled(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.rowMenuOf("t:al-13")
	for label, why := range map[string]string{"Review…": "select a task in review", "Reply": "no open question here",
		"Attach": "task al-13 has no run"} {
		it := h.m.menu.items[h.menuIndex(label)]
		if !strings.Contains(it.why, why) {
			t.Errorf("%s: why %q, want %q", label, it.why, why)
		}
	}
	if s := h.screenText(); !strings.Contains(s, "— no open question here") {
		t.Fatalf("the reason is not on screen:\n%s", s)
	}
	h.menuClick("Reply")
	if h.m.menu == nil || h.m.dlg != nil || !strings.Contains(h.m.toast.text, "no open question here") || len(h.calls) != 0 {
		t.Fatalf("a disabled item acted: menu %v dlg %v toast %q", h.m.menu != nil, h.m.dlg != nil, h.m.toast.text)
	}
	// Enter on a disabled item does the same.
	h.m.menu.sel = h.menuIndex("Reply")
	h.key("enter")
	if h.m.menu == nil || len(h.calls) != 0 {
		t.Fatal("enter ran a disabled item")
	}
	// Nothing is guessed from the backend's rules: a ready task's "Mark ready", a task's
	// "Deliver…" and "Resume" stay enabled, and the CLI's error is the toast.
	for _, label := range []string{"Mark ready", "Deliver…", "Resume", "Edit…", "Start run…"} {
		if why := h.m.menu.items[h.menuIndex(label)].why; why != "" {
			t.Errorf("%s is disabled: %q", label, why)
		}
	}

	// Read-only: every action is disabled with the database's reason.
	r := newHarness(t, 120, 40)
	r.snap = func() *data.Snapshot { s := fixture.Snapshot(false); s.ReadOnly = true; return s }
	r.send(loadMsg{snap: r.snap()})
	r.rowMenuOf("t:al-13")
	for _, label := range []string{"Start run…", "Edit…", "Block…", "Cancel…", "Reply", "Message task", "Resume"} {
		if why := r.m.menu.items[r.menuIndex(label)].why; !strings.Contains(why, "read-only") {
			t.Errorf("%s read-only: %q", label, why)
		}
	}
	r.key("esc", "3", "tab", "o")
	for _, it := range r.m.menu.items {
		if it.label != "Reload" && !strings.Contains(it.why, "read-only") {
			t.Errorf("item %s on Needs read-only: %q", it.label, it.why)
		}
	}
	r.key("esc", "tab", "tab")
	r.rclick(r.at(regBoxText, ""))
	if why := r.m.menu.items[r.menuIndex("Send")].why; !strings.Contains(why, "read-only") {
		t.Errorf("box Send read-only: %q", why)
	}
}

// A menu item is disabled exactly when its key would not call anything: the same check
// the key does before the CLI, never a guess at the backend's rules.
func TestMenuEnabledMatchesKeys(t *testing.T) {
	rows := []string{"p:alpha", "p:beta", "c:alpha", "g:deliver:alpha", "t:al-9", "t:al-12", "t:al-13", "t:al-14",
		"t:al-15", "t:al-16", "t:al-17", "t:be-1", "r:al-12-r2", "r:al-14-r1"}
	checked := 0
	for _, row := range rows {
		probe := newHarness(t, 120, 40)
		probe.rowMenuOf(row)
		kind := probe.m.tree.Selected().Kind
		for _, it := range probe.m.menu.items {
			if it.key == "" || (it.key == "enter" && kind == tree.Task) {
				continue // a task's enter toggles; its Attach item is about the pane
			}
			k := newHarness(t, 120, 40)
			k.selectRow(row)
			k.key(it.key)
			refused := k.m.toast.bad && k.m.toast.text != "" && len(k.calls) == 0 && k.m.dlg == nil &&
				len(k.attaches) == 0 && !strings.Contains(k.m.toast.text, "not found") && !k.m.typ.on && !k.m.filtering && k.m.focus != focusBox
			if refused != (it.why != "") {
				t.Errorf("%s %q (key %s): disabled %q, but the key refused=%v (toast %q)", row, it.label, it.key, it.why,
					refused, k.m.toast.text)
			}
			checked++
		}
	}
	if checked < 60 {
		t.Fatalf("only %d items checked", checked)
	}
}

// A menu closes when its target goes away with a new snapshot, and is rebuilt (its
// items' availability) when the target stays.
func TestMenuFollowsSnapshots(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("3", "tab", "o") // the question's item
	if h.m.menu == nil || h.m.menu.items[0].label != "Reply" {
		t.Fatalf("menu %v", h.m.menu)
	}
	gone := func() *data.Snapshot {
		s := fixture.Snapshot(false)
		s.Needs.Questions, s.Asks = nil, nil
		return s
	}
	h.snap = gone
	h.send(loadMsg{snap: gone()})
	if h.m.menu != nil {
		t.Fatal("the menu of a vanished item stays open")
	}
	// The same row, now read-only: the open menu is rebuilt with the reasons.
	h.rowMenuOf("t:al-13")
	if h.m.menu.items[h.menuIndex("Edit…")].why != "" {
		t.Fatal("setup")
	}
	ro := fixture.Snapshot(false)
	ro.ReadOnly = true
	h.send(loadMsg{snap: ro})
	if h.m.menu == nil || !strings.Contains(h.m.menu.items[h.menuIndex("Edit…")].why, "read-only") {
		t.Fatal("the open menu kept stale availability")
	}
}

func TestMenuKeysAndClicks(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.key("o")
	if h.m.menu == nil {
		t.Fatal("o opens no menu")
	}
	first := h.m.menu.sel
	h.key("down")
	if h.m.menu.sel != first+1 {
		t.Fatalf("down: %d", h.m.menu.sel)
	}
	h.key("esc")
	if h.m.menu != nil {
		t.Fatal("esc keeps the menu")
	}
	for _, k := range []string{"shift+f10", "menu"} {
		h.key(k)
		if h.m.menu == nil {
			t.Fatalf("%s opens no menu", k)
		}
		h.key("o")
		if h.m.menu != nil {
			t.Fatal("o closes the menu")
		}
	}
	// The menu owns the keyboard: q does not quit, j moves.
	h.key("o", "q")
	if h.quit || h.m.menu != nil {
		t.Fatal("q in a menu")
	}
	// A click outside closes it and does nothing else; a right click elsewhere opens
	// that place's menu.
	h.key("o")
	h.click(h.m.w-1, 0)
	if h.m.menu != nil || h.selected() != "t:al-13" {
		t.Fatal("a click outside")
	}
	h.key("o")
	h.rclick(h.m.w-1, 0)
	if h.m.menu == nil || h.selected() != "t:al-13" || !slices.Contains(h.menuLabels(), "Help") {
		t.Fatalf("a right click elsewhere: menu %v selected %s", h.m.menu != nil, h.selected())
	}
	// The wheel moves the selection of a menu.
	x, y := h.at(regMenuItem, "0")
	h.m.menu.sel = 0
	h.send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	if h.m.menu.sel != 1 {
		t.Fatalf("menu wheel: %d", h.m.menu.sel)
	}
	// A dialog that opens meanwhile closes the menu.
	h.m.open(h.m.confirmDialog("x", "y", "Go", "", []string{"notify", "ack", "--all"}))
	if h.m.menu != nil {
		t.Fatal("a dialog opened under a menu")
	}
}

// Menus exist everywhere: the header, tabs, divider, footer, message box, the empty
// tree, the preview and the text tabs.
func TestMenusEverywhere(t *testing.T) {
	for _, c := range []struct {
		name   string
		kind   regionKind
		id     string
		prep   func(h *harness)
		expect string
	}{
		{"header", regHeader, "", nil, "Filter…"},
		{"counter", regCounter, "needs", nil, "Reload"},
		{"tab", regTab, "2", nil, "Needs"},
		{"divider", regDivider, "", nil, "Wider tree"},
		{"footer", regFooter, "", nil, "Quit"},
		{"box", regBoxText, "", nil, "Recipient…"},
		{"preview", regPane, "", func(h *harness) { h.selectRow("r:al-12-r2") }, "Follow live"},
		{"details", regPane, "", func(h *harness) { h.key("2") }, "Bottom"},
		{"needs item", regPane, "", func(h *harness) { h.key("3") }, "Reply"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, 120, 40)
			if c.prep != nil {
				c.prep(h)
			}
			x, y := h.at(c.kind, c.id)
			if c.kind == regPane {
				y += 3
			}
			h.rclick(x, y)
			if h.m.menu == nil {
				t.Fatal("no menu")
			}
			if !slices.ContainsFunc(h.menuLabels(), func(l string) bool { return strings.HasPrefix(l, c.expect) }) {
				t.Fatalf("labels %v lack %q", h.menuLabels(), c.expect)
			}
		})
	}
	// An empty part of the tree.
	h := newHarness(t, 120, 40)
	s := layout(h.m)
	h.rclick(s.tree.X+3, s.tree.Y+s.tree.H-1)
	if h.m.menu == nil || !slices.Contains(h.menuLabels(), "Help") {
		t.Fatalf("empty tree: %v", h.menuLabels())
	}
}

// On Needs and Inbox, the menu is the highlighted item's: its buttons run as they do
// from a click or the keys, never on the tree row out of sight.
func TestMenuOnItems(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.key("3", "tab") // Needs, focus on the right pane; the first item is the question
	h.key("o")
	if h.m.menu == nil || !slices.Contains(h.menuLabels(), "Reply") {
		t.Fatalf("labels %v", h.menuLabels())
	}
	h.key("enter") // the first item: Reply
	if h.m.dlg == nil || h.selected() != "t:al-13" {
		t.Fatalf("reply dialog %v, selection %s", h.m.dlg != nil, h.selected())
	}
	h.key("esc")
	// The blocked task's item: Unblock runs `task ready al-16`, as the key R does.
	h.key("down", "o")
	label := h.m.menu.items[0].label
	h.menuClick(label)
	menuCalls := h.calls
	k := newHarness(t, 120, 40)
	k.selectRow("t:al-13")
	k.key("3", "tab", "down", "R")
	if len(menuCalls) != 1 || !reflect.DeepEqual(menuCalls, k.calls) || !slices.Equal(menuCalls[0], []string{"--project", "alpha", "task", "ready", "al-16"}) {
		t.Fatalf("menu %v, key %v", menuCalls, k.calls)
	}
	// Right click on an item selects it first.
	h = newHarness(t, 120, 40)
	h.key("3")
	items := h.m.items(tabs.Needs)
	var blocked int
	for i, it := range items {
		if it.Key == "b:al-16" {
			blocked = i
		}
	}
	_, buttons, _ := h.m.itemView(tabs.Needs, false)
	for _, b := range buttons {
		if b.item == blocked {
			s := layout(h.m)
			h.rclick(s.body.X+b.x, s.body.Y+b.line)
		}
	}
	if h.m.menu == nil || h.m.itemKey[tabs.Needs] != "b:al-16" {
		t.Fatalf("item not selected: %q", h.m.itemKey[tabs.Needs])
	}
}

// Every mouse action has a key: each region kind lists the keys that do what a click
// on it does. A kind without an entry fails, so a new clickable part cannot be added
// without its key.
func TestMouseActionsHaveKeys(t *testing.T) {
	hintKeys := []string{}
	for _, b := range hints() {
		hintKeys = append(hintKeys, b.Keys()[0])
	}
	none := []string{} // a block: its parts (or the focus ring) carry the actions
	keys := map[regionKind][]string{
		regHeader: none, regTree: {"tab"}, regDivider: {"<", ">", "="}, regTabs: {"tab"}, regPane: {"tab", "i", "enter"},
		regFooter: none, regTooSmall: none, regBox: {"tab", "m"},
		regCounter: {"!", "4", "j"}, regTab: {"1", "2", "3", "4", "5", "6"}, regRow: {"j", "k", "enter"},
		regToggle: {"left", "right", "space"}, regHint: hintKeys, regToast: {"6"},
		regItemAct: {"enter", "R", "u", "r", "S", "m", "A"}, regBoxTo: {"ctrl+t"}, regBoxKind: {"ctrl+k"},
		regBoxText: {"m"}, regBoxSend: {"enter"}, regHelp: {"esc"}, regDialog: {"esc"},
		regDlgPart: {"tab", "enter", "ctrl+s"}, regMenu: {"esc"}, regMenuItem: {"enter"},
	}
	// Keys that belong to the message box, dialogs and menus rather than the table.
	other := map[string]bool{"ctrl+t": true, "ctrl+k": true, "ctrl+s": true}
	bound := func(k string) bool {
		for _, b := range bindings {
			if slices.Contains(b.Keys(), k) {
				return true
			}
		}
		return other[k]
	}
	for k := regionKind(0); k < regKindCount; k++ {
		ks, ok := keys[k]
		if !ok {
			t.Errorf("region kind %d has no entry: which keys do what a click on it does?", k)
		}
		for _, key := range ks {
			if !bound(key) && key != "enter" && key != "esc" && key != "tab" {
				t.Errorf("region kind %d: key %q is not bound", k, key)
			}
		}
	}
	// The message box keys really do their parts.
	h := newHarness(t, 120, 40)
	h.key("m")
	kind := h.m.box.kind
	h.key("ctrl+k")
	if h.m.box.kind == kind {
		t.Error("ctrl+k does not cycle the kind")
	}
	h.key("ctrl+t")
	if h.m.dlg == nil {
		t.Error("ctrl+t does not open the recipient picker")
	}
	h.key("esc")
	h.key("enter")
	if !strings.Contains(h.m.toast.text, "message first") {
		t.Errorf("enter does not send: %q", h.m.toast.text)
	}
	// The toast click and the counters have their keys: 6 shows the Log, ! the Needs.
	h.key("esc", "6")
	if h.m.tab != tabs.Log {
		t.Error("6")
	}
}

func TestFilter(t *testing.T) {
	h := newHarness(t, 120, 40)
	rowKeys := func() []string {
		var out []string
		for _, r := range h.m.tree.Rows {
			out = append(out, r.Key)
		}
		return out
	}
	all := len(rowKeys())
	h.key("/")
	if !h.m.filtering {
		t.Fatal("/ opens no prompt")
	}
	h.typeText("q") // text: no quit
	if h.quit || h.m.tree.Filter != "q" {
		t.Fatalf("filter %q quit %v", h.m.tree.Filter, h.quit)
	}
	h.key("backspace")
	h.typeText("login")
	if got, want := rowKeys(), []string{"p:alpha", "t:al-12"}; !slices.Equal(got, want) {
		t.Fatalf("rows %v, want %v", got, want)
	}
	if s := h.screenText(); !strings.Contains(s, "/login") {
		t.Fatalf("no filter in the header:\n%s", s)
	}
	// Run rows match by id and profile; their ancestors stay.
	h.key("ctrl+u")
	h.typeText("al-12-r2")
	if got := rowKeys(); !slices.Equal(got, []string{"p:alpha", "t:al-12", "r:al-12-r2"}) {
		t.Fatalf("run rows %v", got)
	}
	h.key("ctrl+u")
	h.typeText("OPUS") // case-insensitive; no profile of the fixture's rows has it
	if len(rowKeys()) != 0 {
		t.Fatalf("rows %v", rowKeys())
	}
	h.key("ctrl+u")
	h.typeText("sonnet")
	if got := rowKeys(); !slices.Contains(got, "r:al-12-r2") || !slices.Contains(got, "c:alpha") {
		t.Fatalf("profile rows %v", got)
	}
	// Enter keeps the filter and gives the keys back to the tree.
	h.key("enter")
	if h.m.filtering || h.m.tree.Filter != "sonnet" {
		t.Fatal("enter")
	}
	h.key("j")
	if h.quit {
		t.Fatal("j")
	}
	// Esc clears it (while not editing too).
	h.key("esc")
	if h.m.tree.Filter != "" || len(rowKeys()) != all {
		t.Fatalf("esc: %q, %d rows", h.m.tree.Filter, len(rowKeys()))
	}
	h.key("/")
	h.typeText("zzz")
	h.key("esc")
	if h.m.filtering || h.m.tree.Filter != "" || len(rowKeys()) != all {
		t.Fatal("esc in the prompt")
	}
	// The selection survives the filter when its row still matches.
	h.selectRow("t:al-12")
	h.key("/")
	h.typeText("fix")
	if h.selected() != "t:al-12" {
		t.Fatalf("selected %s", h.selected())
	}
}

func TestCopy(t *testing.T) {
	h := newHarness(t, 120, 40)
	text := func() string {
		cmd := h.m.copy()
		if cmd == nil {
			return ""
		}
		return reflect.ValueOf(cmd()).String()
	}
	h.selectRow("r:al-12-r2")
	if got := text(); got != "tmux attach otag-alpha %7" {
		t.Fatalf("run: %q", got)
	}
	h.selectRow("t:al-13")
	if got := text(); got != "al-13" {
		t.Fatalf("task: %q", got)
	}
	h.selectRow("p:beta")
	if got := text(); got != "beta" {
		t.Fatalf("project: %q", got)
	}
	h.key("y")
	if h.m.toast.text != "copied beta" {
		t.Fatalf("toast %q", h.m.toast.text)
	}
	// On Needs the item's id.
	h.key("3", "tab", "y")
	if h.m.toast.text != "copied 42" {
		t.Fatalf("item: %q", h.m.toast.text)
	}
}

func TestExpandAll(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-12")
	h.key("left")
	h.selectRow("p:alpha")
	for _, k := range []string{"t:al-12", "g:deliver:alpha"} {
		h.m.tree.SetExpanded(k, false)
	}
	h.m.tree.Rebuild(h.m.snap)
	if slices.ContainsFunc(h.m.tree.Rows, func(r tree.Row) bool { return r.Key == "r:al-12-r2" }) {
		t.Fatal("setup: the run row is shown")
	}
	h.key("E")
	if !slices.ContainsFunc(h.m.tree.Rows, func(r tree.Row) bool { return r.Key == "r:al-12-r2" }) ||
		!slices.ContainsFunc(h.m.tree.Rows, func(r tree.Row) bool { return r.Key == "t:al-9" }) {
		t.Fatal("E did not expand the project")
	}
	if slices.ContainsFunc(h.m.tree.Rows, func(r tree.Row) bool { return r.Key == "t:be-1" && r.Expanded }) {
		t.Fatal("another project changed")
	}
}

func TestSyncRegistry(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("p:alpha")
	h.key("Y")
	if !slices.Equal(h.lastCall(), []string{"sync"}) {
		t.Fatalf("call %v", h.lastCall())
	}
}

func TestHelpMatchesBindings(t *testing.T) {
	lines := helpLines(74)
	text := strings.Join(lines, "\n")
	for _, b := range bindings {
		h := b.Help()
		if h.Key == "" {
			continue
		}
		if !strings.Contains(text, h.Key) {
			t.Errorf("help lacks the keys %q", h.Key)
		}
		for _, w := range strings.Fields(h.Desc) {
			if !strings.Contains(text, w) {
				t.Errorf("help lacks %q of %q", w, h.Desc)
			}
		}
	}
	for _, l := range lines {
		if w := len([]rune(l)); w > 74 {
			t.Errorf("help line of %d cells: %q", w, l)
		}
	}
	// Every key of the table is documented by some entry (the first of each group
	// carries the keys of the rest).
	for _, b := range bindings {
		for _, k := range b.Keys() {
			if !strings.Contains(text, strings.ToLower(k)) && !strings.Contains(text, k) {
				if strings.HasPrefix(k, "shift+") || k == "menu" || len(k) > 1 || (k >= "1" && k <= "6") {
					continue // named keys are written their own way, the digits as 1-6
				}
				t.Errorf("key %q is not in the help", k)
			}
		}
	}
}

// The help scrolls on a screen too short for it: every line is reachable.
func TestHelpScrolls(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("?")
	all := helpLines(min(80-4, 78) - 4)
	_, first := h.m.helpBox()
	if len(first) >= len(all) {
		t.Fatalf("the help fits: %d lines", len(first))
	}
	if !strings.Contains(first[len(first)-1], "of "+strconv.Itoa(len(all))) {
		t.Fatalf("no position line: %q", first[len(first)-1])
	}
	seen := map[string]bool{}
	collect := func() {
		_, lines := h.m.helpBox()
		for _, l := range lines[:len(lines)-1] {
			seen[l] = true
		}
	}
	collect()
	for i := 0; i < len(all) && h.m.help; i++ {
		h.key("pgdown")
		collect()
	}
	for _, l := range all {
		if !seen[l] {
			t.Fatalf("line %q is unreachable", l)
		}
	}
	h.key("home")
	if h.m.helpOff != 0 {
		t.Fatal("home")
	}
	h.key("j")
	if h.m.helpOff != 1 {
		t.Fatalf("j: %d", h.m.helpOff)
	}
	x, y := h.at(regHelp, "")
	h.send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	if h.m.helpOff != 1+wheelLines {
		t.Fatalf("wheel: %d", h.m.helpOff)
	}
	h.key("end")
	if s := h.screenText(); !strings.Contains(s, "tmux.conf") {
		t.Fatalf("the end of the help is not shown:\n%s", s)
	}
	h.key("?")
	if h.m.help {
		t.Fatal("? keeps the help")
	}
	// A tall screen shows it whole, without a position line.
	big := newHarness(t, 120, 90)
	big.key("?")
	_, lines := big.m.helpBox()
	if len(lines) != len(helpLines(74)) {
		t.Fatalf("tall screen: %d of %d lines", len(lines), len(helpLines(74)))
	}
}

// The layout the model reports is what a restart restores.
func TestStateRestores(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("<")
	h.key("3")
	h.selectRow("p:beta")
	h.key("left")
	st := h.m.state()
	if st.Divider == 0 || st.Tab != int(tabs.Needs) || st.Expanded["p:beta"] || !hasKey(st.Expanded, "p:beta") {
		t.Fatalf("state %+v", st)
	}
	r := newHarness(t, 120, 40)
	r.m.applyState(st)
	r.send(loadMsg{snap: r.snap()})
	if r.m.divider != st.Divider || r.m.tab != tabs.Needs || r.m.tree.Rows[len(r.m.tree.Rows)-1].Key != "p:beta" {
		t.Fatalf("restored: divider %d tab %d, last row %s", r.m.divider, r.m.tab, r.m.tree.Rows[len(r.m.tree.Rows)-1].Key)
	}
}

func hasKey(m map[string]bool, k string) bool { _, ok := m[k]; return ok }

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	path := statePath(dir)
	if st := loadState(path); !reflect.DeepEqual(st, uiState{}) {
		t.Fatalf("missing file: %+v", st)
	}
	want := uiState{Divider: 44, Tab: 3, Expanded: map[string]bool{"p:alpha": false, "t:al-12": true}}
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	want.Version = stateVersion
	if got := loadState(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %+v", got)
	}
	// Atomic: no temporary file stays, and the file is private.
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 || ents[0].Name() != "state.json" {
		t.Fatalf("directory: %v", ents)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// Corrupt or out of range: the defaults.
	for name, content := range map[string]string{"garbage": "{not json", "empty": "", "truncated": `{"version":1,"divider":4`,
		"version": `{"version":9,"divider":40}`, "negative": `{"version":1,"divider":-5}`, "tab": `{"version":1,"tab":99}`,
		"type": `{"version":1,"divider":"wide"}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := loadState(path); !reflect.DeepEqual(got, uiState{}) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// A failed write leaves the old file alone.
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	if err := saveState(filepath.Join(blocked, "tui", "state.json"), want); err == nil {
		t.Fatal("a write under a file succeeded")
	}
	if got := loadState(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("old file lost: %+v", got)
	}
	// Too many expansion choices are capped on save.
	big := uiState{Expanded: map[string]bool{}}
	for i := 0; i < maxExpanded+50; i++ {
		big.Expanded["t:x-"+strconv.Itoa(i)] = true
	}
	if err := saveState(path, big); err != nil {
		t.Fatal(err)
	}
	if got := loadState(path); len(got.Expanded) != maxExpanded {
		t.Fatalf("capped to %d", len(got.Expanded))
	}
}

func TestThemeAndTerminalReports(t *testing.T) {
	h := newHarness(t, 120, 40)
	dark := h.m.theme.Warn.Render("x")
	h.send(tea.BackgroundColorMsg{Color: color.RGBA{R: 255, G: 255, B: 255, A: 255}})
	if light := h.m.theme.Warn.Render("x"); light == dark || light != style.Light.Warn.Render("x") {
		t.Fatalf("light theme %q, dark %q", light, dark)
	}
	h.send(tea.BackgroundColorMsg{Color: color.RGBA{A: 255}})
	if h.m.theme.Warn.Render("x") != dark {
		t.Fatal("dark theme not restored")
	}

	// The fixture needs the human twice: a question and a blocked task.
	v := h.m.View()
	if v.WindowTitle != "otag (2)" || v.ProgramStatus == nil || v.ProgramStatus.State != tea.ProgramStateBlocked ||
		v.ProgramStatus.App != "otag" {
		t.Fatalf("title %q status %+v", v.WindowTitle, v.ProgramStatus)
	}
	h.snap = func() *data.Snapshot { s := fixture.Snapshot(false); s.Needs = &msg.Needs{}; return s }
	h.send(loadMsg{snap: h.snap()})
	v = h.m.View()
	if v.WindowTitle != "otag" || v.ProgramStatus == nil || v.ProgramStatus.State != tea.ProgramStateIdle {
		t.Fatalf("title %q status %+v", v.WindowTitle, v.ProgramStatus)
	}
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Fatal("hover is on by default")
	}
}

func TestHover(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.m.hover = true
	if h.m.View().MouseMode != tea.MouseModeAllMotion {
		t.Fatal("hover without all-motion reports")
	}
	x, y := h.at(regRow, "t:al-13")
	h.send(tea.MouseMotionMsg{X: x, Y: y})
	if h.m.tree.Hover != "t:al-13" || h.selected() == "t:al-13" {
		t.Fatalf("hover %q selected %s", h.m.tree.Hover, h.selected())
	}
	h.m.View() // cache the frame
	if !h.m.frameValid {
		t.Fatal("no frame")
	}
	h.send(tea.MouseMotionMsg{X: x + 1, Y: y}) // the same row: nothing to redraw
	if !h.m.frameValid {
		t.Fatal("a motion within a row redrew the screen")
	}
	h.send(tea.MouseMotionMsg{X: 0, Y: 0}) // off the rows
	if h.m.tree.Hover != "" || h.m.frameValid {
		t.Fatal("hover stays off the rows")
	}
	// Off: motion changes nothing.
	h.m.hover = false
	h.send(tea.MouseMotionMsg{X: x, Y: y})
	if h.m.tree.Hover != "" {
		t.Fatal("hover without the option")
	}
}

// The debug log gets what happened, never what was typed or sent.
func TestDebugLogHasNoText(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.fire("capture")
	h.key("i")
	h.typeText("SECRETKEYS")
	h.send(tea.PasteMsg{Content: "SECRETPASTE"})
	h.key("ctrl+q")
	h.m.leaveTyping("", false)
	h.key("tab", "tab", "m")
	h.typeText("SECRETBODY")
	h.key("enter")
	h.key("esc", "/")
	h.typeText("SECRETFILTER")
	if len(h.logs) == 0 {
		t.Fatal("nothing logged")
	}
	all := strings.Join(h.logs, "\n")
	if strings.Contains(all, "SECRET") {
		t.Fatalf("typed text in the log:\n%s", all)
	}
	for _, want := range []string{"typing on run=al-12-r2", "typing off", "action send ok"} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q:\n%s", want, all)
		}
	}
}

func TestOpenDebug(t *testing.T) {
	dir := t.TempDir()
	if l := openDebug(func(string) string { return "" }, dir); l != nil {
		t.Fatal("log without OTAG_TUI_DEBUG")
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); err == nil {
		t.Fatal("log directory without OTAG_TUI_DEBUG")
	}
	l := openDebug(func(k string) string {
		if k == "OTAG_TUI_DEBUG" {
			return "1"
		}
		return ""
	}, dir)
	l.Printf("hello %d", 7)
	l.Close()
	b, err := os.ReadFile(filepath.Join(dir, "logs", "tui.log"))
	if err != nil || !strings.Contains(string(b), "hello 7") || !strings.Contains(string(b), "tui started") {
		t.Fatalf("log %q %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "logs", "tui.log")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// capping copies: the map handed to the worker is not the model's.
func TestCappedCopies(t *testing.T) {
	big := uiState{Expanded: map[string]bool{}}
	for i := 0; i < maxExpanded+10; i++ {
		big.Expanded["t:x-"+strconv.Itoa(i)] = true
	}
	c := capped(big)
	if len(big.Expanded) != maxExpanded+10 || len(c.Expanded) != maxExpanded {
		t.Fatalf("input %d, capped %d", len(big.Expanded), len(c.Expanded))
	}
	c.Expanded["new"] = true
	if _, ok := big.Expanded["new"]; ok {
		t.Fatal("shared map")
	}
}

// A liveness result rebuilds an open menu: Attach was disabled while the pane was
// being checked.
func TestMenuFollowsLiveness(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.send(liveMsg{live: map[string]tmux.Liveness{}})
	h.rowMenuOf("r:al-12-r2")
	if why := h.m.menu.items[h.menuIndex("Attach")].why; !strings.Contains(why, "checking") {
		t.Fatalf("setup: %q", why)
	}
	h.send(liveMsg{live: fixture.Live()})
	if h.m.menu == nil || h.m.menu.items[h.menuIndex("Attach")].why != "" {
		t.Fatal("the open menu kept Attach disabled")
	}
}
