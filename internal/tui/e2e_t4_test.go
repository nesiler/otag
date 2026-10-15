package tui_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/tui/tuitest"
)

// rclick sends a right click at the 0-based cell x, y.
func (o *outer) rclick(x, y int) {
	o.t.Helper()
	o.keys("-l", fmt.Sprintf("\x1b[<2;%d;%dM\x1b[<2;%d;%dm", x+1, y+1, x+1, y+1))
}

// mouse sends a raw SGR mouse report: code 0 is a left press, 32 a motion with the
// left button held; release is true for the final lowercase m.
func (o *outer) mouse(code, x, y int, release bool) {
	o.t.Helper()
	end := "M"
	if release {
		end = "m"
	}
	o.keys("-l", fmt.Sprintf("\x1b[<%d;%d;%d%s", code, x+1, y+1, end))
}

// findBelow is find, ignoring the first minY screen rows (the header and the tabs).
func findBelow(screen, text string, minY int) (int, int, bool) {
	for y, line := range strings.Split(screen, "\n") {
		if y < minY {
			continue
		}
		if i := strings.Index(line, text); i >= 0 {
			return len([]rune(line[:i])), y, true
		}
	}
	return 0, 0, false
}

// dividerX is the column of the divider on a body row of the screen (-1: none).
func dividerX(screen string, y int) int {
	lines := strings.Split(screen, "\n")
	if y >= len(lines) {
		return -1
	}
	for x, r := range []rune(lines[y]) {
		if r == '│' {
			return x
		}
	}
	return -1
}

// quit presses q and waits for the TUI's session to end.
func (o *outer) quit() {
	o.t.Helper()
	o.keys("q")
	for i := 0; i < 100; i++ {
		if _, err := o.tmux("has-session", "-t", "t"); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	o.t.Fatalf("q did not quit:\n%s", o.screen())
}

// E2E 10 and the restart: dragging the divider moves it; the layout (divider, tab,
// expansion) is saved to <data>/tui/state.json and a new TUI comes up with it.
func TestE2EDividerDragAndRestart(t *testing.T) {
	e := tuitest.New(t, otagBin)
	e.AddTask("alpha", "Fix login")
	e.AddTask("beta", "Beta thing")
	o := startTUI(t, e, 160, 45)
	s, _ := o.waitFor("the tree", 10*time.Second, contains("▾ alpha", "▾ beta", "Fix login", "Beta thing"))
	const row = 20 // a body row below the tree's rows
	x0 := dividerX(s, row)
	if x0 != 60 {
		t.Fatalf("divider at %d, want 60 (38%% of 160):\n%s", x0, s)
	}

	// Press on the divider, drag with the button held, release.
	o.mouse(0, x0, row, false)
	o.mouse(32, x0+10, row, false)
	o.mouse(32, x0+20, row, false)
	o.mouse(0, x0+20, row, true)
	s, _ = o.waitFor("the divider to move", 5*time.Second, func(s string) bool { return dividerX(s, row) == x0+20 })
	if !strings.Contains(s, "Beta thing") {
		t.Fatalf("the tree vanished:\n%s", s)
	}

	// The Needs tab and a collapsed project are part of the layout too.
	o.keys("3")
	o.waitFor("the Needs tab", 5*time.Second, contains("what waits for you"))
	o.clickText("▾ beta")
	o.waitFor("beta collapsed", 5*time.Second, func(s string) bool {
		return strings.Contains(s, "▸ beta") && !strings.Contains(s, "Beta thing")
	})
	state := filepath.Join(e.Vars["OTAG_DATA_DIR"], "tui", "state.json")
	var st struct {
		Divider  int             `json:"divider"`
		Tab      int             `json:"tab"`
		Expanded map[string]bool `json:"expanded"`
	}
	o.quit() // the layout is written when the TUI exits
	for i := 0; i < 100; i++ {
		b, err := os.ReadFile(state)
		st.Divider, st.Tab, st.Expanded = 0, 0, nil
		if err == nil && json.Unmarshal(b, &st) == nil && st.Divider == x0+20 && st.Tab == 2 && !st.Expanded["p:beta"] {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.Divider != x0+20 || st.Tab != 2 || st.Expanded["p:beta"] {
		t.Fatalf("state file %s: %+v", state, st)
	}

	// A new TUI restores the layout.
	o = startTUI(t, e, 160, 45)
	s, _ = o.waitFor("the restored layout", 10*time.Second, func(s string) bool {
		return strings.Contains(s, "▸ beta") && strings.Contains(s, "what waits for you") && dividerX(s, row) == x0+20
	})
	if strings.Contains(s, "Beta thing") {
		t.Fatalf("beta is expanded again:\n%s", s)
	}
	o.quit()

	// A corrupt file gives the defaults and is rewritten, not an error.
	if err := os.WriteFile(state, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	o = startTUI(t, e, 160, 45)
	s, _ = o.waitFor("the default layout", 10*time.Second, contains("▾ beta", "Beta thing"))
	if dividerX(s, row) != x0 {
		t.Fatalf("divider at %d after a corrupt state, want %d", dividerX(s, row), x0)
	}
	o.keys("<")
	o.waitFor("a narrower tree", 5*time.Second, func(s string) bool { return dividerX(s, row) == x0-4 })
	o.quit()
	for i := 0; i < 100; i++ {
		b, _ := os.ReadFile(state)
		if json.Unmarshal(b, &st) == nil && st.Divider == x0-4 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the corrupt state file was not replaced: %+v", st)
}

// E2E 11: a right click opens the row's context menu; a disabled item says why and
// does nothing; an item runs what its key runs (Details shows the tab, Mark ready
// runs `task ready`); o opens the same menu from the keyboard.
func TestE2EContextMenu(t *testing.T) {
	e := tuitest.New(t, otagBin)
	fix := e.AddTask("alpha", "Fix login")
	draft := e.AddTask("alpha", "Docs", "--draft")
	o := startTUI(t, e, 160, 45)
	s, _ := o.waitFor("the tree", 10*time.Second, contains("Fix login", "Docs"))

	x, y, _ := findBelow(s, "Fix login", 2)
	o.rclick(x, y)
	s, _ = o.waitFor("the menu", 5*time.Second, contains("Start run…", "Block…", "Copy id", "— task "+fix+" has no run"))
	// A disabled item: its click says why and leaves the menu open.
	ax, ay, ok := findBelow(s, "Attach", 3)
	if !ok {
		t.Fatalf("no Attach item:\n%s", s)
	}
	o.click(ax, ay)
	o.waitFor("the reason", 5*time.Second, contains("Attach: task "+fix+" has no run", "Start run…"))

	// An enabled one: Details shows the tab and closes the menu.
	s = o.screen()
	dx, dy, ok := findBelow(s, "Details  ", 3)
	if !ok {
		t.Fatalf("no Details item:\n%s", s)
	}
	o.click(dx, dy)
	o.waitFor("the Details tab", 5*time.Second, func(s string) bool {
		return strings.Contains(s, "details of "+fix) && !strings.Contains(s, "Start run…")
	})

	// Mark ready on a draft task: the CLI runs, the task becomes ready.
	s = o.screen()
	x, y, _ = findBelow(s, "Docs", 2)
	o.rclick(x, y)
	s, _ = o.waitFor("the draft's menu", 5*time.Second, contains("Mark ready", "Block…"))
	mx, my, _ := findBelow(s, "Mark ready", 3)
	o.click(mx, my)
	e.WaitTask(draft, func(status string) bool { return status == "ready" })
	o.waitFor("the menu closed", 5*time.Second, func(s string) bool { return !strings.Contains(s, "Block…") })

	// The keyboard opens the same menu; Esc closes it.
	o.keys("o")
	o.waitFor("the menu by key", 5*time.Second, contains("Start run…", "Copy id"))
	o.escape()
	o.waitFor("the menu closed", 5*time.Second, func(s string) bool { return !strings.Contains(s, "Copy id") })
}
