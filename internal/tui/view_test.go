package tui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/fixture"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// golden compares the plain text of the screen with testdata/<name>.golden.
func golden(t *testing.T, h *harness, name string) {
	t.Helper()
	content := h.m.View().Content
	lines := strings.Split(content, "\n")
	if len(lines) != h.m.h {
		t.Errorf("%s: %d lines, want %d", name, len(lines), h.m.h)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > h.m.w {
			t.Errorf("%s: line %d is %d cells wide", name, i, w)
		}
	}
	got := ansi.Strip(content)
	// The help overlay is composed by Lip Gloss, which trims trailing spaces.
	var b strings.Builder
	for _, l := range strings.Split(got, "\n") {
		b.WriteString(strings.TrimRight(l, " ") + "\n")
	}
	got = b.String()
	file := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs (go test -update rewrites it):\n%s", name, got)
	}
}

func TestGolden(t *testing.T) {
	for _, c := range []struct {
		name string
		w, h int
		snap func() *data.Snapshot
		do   func(h *harness)
	}{
		{name: "main", w: 120, h: 40},
		{name: "run", w: 120, h: 40, do: func(h *harness) { h.selectRow("r:al-12-r2"); h.fire("capture") }},
		{name: "typing", w: 120, h: 40, do: func(h *harness) { h.selectRow("r:al-12-r2"); h.fire("capture"); h.key("i") }},
		{name: "dialog-started", w: 120, h: 40, do: func(h *harness) {
			h.result = func([]string) Outcome {
				return Outcome{Data: &run.StartResult{Run: &run.Run{ID: "al-13-r1",
					Session: &run.Session{TmuxSession: "otag-alpha", TmuxPane: "%9"}}},
					Human: "started al-13-r1 for task al-13 (sonnet, claude) — running\nattach:    tmux -L otag …\n"}
			}
			h.selectRow("t:al-13")
			h.key("s", "enter")
		}},
		{name: "needs", w: 120, h: 40, do: func(h *harness) { h.key("3") }},
		{name: "inbox", w: 120, h: 40, do: func(h *harness) { h.key("4") }},
		{name: "details", w: 120, h: 40, do: func(h *harness) { h.selectRow("t:al-14"); h.key("2") }},
		{name: "help", w: 120, h: 40, do: func(h *harness) { h.key("?") }},
		{name: "help-scrolled", w: 120, h: 40, do: func(h *harness) { h.key("?", "end") }},
		{name: "help-short", w: 80, h: 24, do: func(h *harness) { h.key("?", "pgdown") }},
		{name: "help-tall", w: 100, h: 80, do: func(h *harness) { h.key("?") }},
		{name: "menu-task", w: 120, h: 40, do: func(h *harness) { h.rowMenuOf("t:al-13") }},
		{name: "menu-run", w: 120, h: 40, do: func(h *harness) { h.rowMenuOf("r:al-12-r2") }},
		{name: "menu-short", w: 80, h: 16, do: func(h *harness) { h.rowMenuOf("t:al-13") }},
		{name: "menu-needs", w: 120, h: 40, do: func(h *harness) { h.key("3", "tab", "o") }},
		{name: "filter", w: 120, h: 40, do: func(h *harness) { h.key("/"); h.typeText("al-1") }},
		{name: "closed", w: 120, h: 40, snap: func() *data.Snapshot { return fixture.Snapshot(true) },
			do: func(h *harness) { h.key("."); h.fire("load") }},
		{name: "readonly", w: 120, h: 40, snap: func() *data.Snapshot {
			s := fixture.Snapshot(false)
			s.ReadOnly = true
			return s
		}},
		{name: "narrow", w: 80, h: 24},
		{name: "narrow-pane", w: 80, h: 24, do: func(h *harness) { h.key("tab") }},
		{name: "too-small", w: 50, h: 10},
		{name: "dialog-add", w: 120, h: 40, do: func(h *harness) {
			h.selectRow("t:al-13")
			h.key("n")
			h.typeText("Fix the bug")
			h.key("tab")
			h.typeText("It breaks on save.")
		}},
		{name: "dialog-start", w: 120, h: 40, do: func(h *harness) { h.selectRow("t:al-13"); h.key("s") }},
		{name: "dialog-deliver", w: 80, h: 24, do: func(h *harness) { h.selectRow("t:al-9"); h.key("D") }},
		{name: "dialog-short", w: 80, h: 16, do: func(h *harness) { h.selectRow("t:al-9"); h.key("D") }},
		{name: "dialog-error", w: 120, h: 40, do: func(h *harness) {
			h.result = func([]string) Outcome {
				return Outcome{Err: apperr.New(apperr.InvalidTransition, "task al-13 cannot move from ready to blocked")}
			}
			h.selectRow("t:al-13")
			h.key("b")
			h.typeText("waits")
			h.key("ctrl+s")
		}},
		{name: "msgbox-reply", w: 120, h: 40, do: func(h *harness) {
			h.selectRow("r:al-14-r1")
			h.key("r")
			h.typeText("Use the brand blue, see the style guide for the exact value; and keep the contrast above 4.5")
			h.key("alt+enter")
			h.typeText("thanks")
		}},
		{name: "log", w: 120, h: 40, do: func(h *harness) {
			h.selectRow("t:al-13")
			h.key("R")
			h.result = func([]string) Outcome { return Outcome{Err: apperr.New(apperr.Conflict, "busy")} }
			h.key("R", "6")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.w, c.h)
			if c.snap != nil {
				h.snap = c.snap
				h.send(loadMsg{snap: c.snap()})
			}
			if c.do != nil {
				c.do(h)
			}
			golden(t, h, c.name)
		})
	}
}

// The Lip Gloss layers sit exactly at the layout's blocks, so a hit test on the
// composed screen and one on the layout agree.
func TestLayersMatchLayout(t *testing.T) {
	for _, size := range [][3]int{{120, 40, 0}, {80, 24, 0}, {120, 40, 1}, {80, 24, 1}} {
		h := newHarness(t, size[0], size[1])
		if size[2] == 0 {
			h.key("?")
		} else {
			h.selectRow("t:al-9")
			h.key("D")
		}
		c := h.m.compose()
		s := layout(h.m)
		for _, r := range s.regions {
			id, ok := blockIDs[r.kind]
			if !ok || (r.z != 0 && r.kind != regHelp && r.kind != regDialog) {
				continue
			}
			l := c.GetLayer(id)
			if l == nil {
				t.Fatalf("%dx%d: no layer %s", size[0], size[1], id)
			}
			if l.GetX() != r.rect.X || l.GetY() != r.rect.Y || l.Width() != r.rect.W || l.Height() != r.rect.H {
				t.Errorf("%dx%d %s: layer %d,%d %dx%d, layout %+v", size[0], size[1], id, l.GetX(), l.GetY(),
					l.Width(), l.Height(), r.rect)
			}
			for _, p := range [][2]int{{r.rect.X, r.rect.Y}, {r.rect.X + r.rect.W - 1, r.rect.Y + r.rect.H - 1}} {
				top, _ := s.hit(p[0], p[1])
				want := blockIDs[top.kind]
				if top.z != 0 && top.kind != regHelp && top.kind != regDialog {
					continue // a part (row, tab) on top of the block
				}
				if got := c.Hit(p[0], p[1]).ID(); got != want {
					t.Errorf("%dx%d at %v: layer %q, layout %q", size[0], size[1], p, got, want)
				}
			}
		}
	}
}
