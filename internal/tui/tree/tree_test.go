package tree

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/tui/fixture"
	"github.com/nesiler/otag/internal/tui/style"
)

func keys(m *Model) []string {
	var out []string
	for _, r := range m.Rows {
		out = append(out, strings.Repeat(" ", r.Depth)+r.Key)
	}
	return out
}

func TestOrder(t *testing.T) {
	m := New()
	m.Rebuild(fixture.Snapshot(false))
	want := []string{
		"p:alpha",
		" c:alpha",
		" g:deliver:alpha",
		"  t:al-9", // its ended run is behind the collapsed task
		" t:al-14", // waiting and blocked first, then by priority and id
		"  r:al-14-r1",
		" t:al-16",
		" t:al-12", // running; its review task nests under it
		"  r:al-12-r2",
		"  h:al-12",
		"  t:al-17",
		" t:al-13", // ready
		" t:al-15", // draft
		"p:beta",
		" t:be-1",
	}
	if got := keys(&m); !slices.Equal(got, want) {
		t.Fatalf("rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r := m.Rows[0]; r.Count != 6 || r.Ready != 2 {
		t.Errorf("alpha counts %d open, %d ready", r.Count, r.Ready)
	}
	if r := m.Rows[9]; r.Kind != Earlier || r.Count != 1 || r.Expanded {
		t.Errorf("earlier row %+v", r)
	}
}

func TestExpandAndHistory(t *testing.T) {
	s := fixture.Snapshot(false)
	m := New()
	m.Rebuild(s)
	if !m.SetExpanded("h:al-12", true) || m.SetExpanded("h:al-12", true) {
		t.Fatal("SetExpanded on the earlier row")
	}
	if h := m.History(); !slices.Equal(h, []string{"al-12"}) {
		t.Fatalf("history %v", h)
	}
	m.Rebuild(s)
	i := slices.IndexFunc(m.Rows, func(r Row) bool { return r.Key == "h:al-12" })
	if r := m.Rows[i+1]; r.Key != "r:al-12-r1" || r.Depth != 3 {
		t.Fatalf("history row %+v", r)
	}
	// Collapse a project: its rows go; a row without children cannot expand.
	if !m.SetExpanded("p:alpha", false) || m.SetExpanded("t:al-13", true) {
		t.Fatal("SetExpanded")
	}
	m.Rebuild(s)
	if got := keys(&m); !slices.Equal(got, []string{"p:alpha", "p:beta", " t:be-1"}) {
		t.Fatalf("collapsed: %v", got)
	}
	// The ended run of a done task shows once the task is expanded.
	m.SetExpanded("p:alpha", true)
	m.Rebuild(s)
	m.SetExpanded("t:al-9", true)
	m.Rebuild(s)
	if !m.Select("r:al-9-r1") {
		t.Fatal("ended run row missing")
	}
}

func TestSelectionKept(t *testing.T) {
	s := fixture.Snapshot(false)
	m := New()
	m.Rebuild(s)
	m.Select("t:al-13")
	// A new task above it: the cursor stays on al-13.
	s2 := fixture.Snapshot(false)
	s2.Open = append(s2.Open, s2.Open[3])
	s2.Open[len(s2.Open)-1].ID, s2.Open[len(s2.Open)-1].Status = "al-10", "waiting"
	m.Rebuild(s2)
	if m.Cursor() != "t:al-13" || m.Selected().Task.ID != "al-13" {
		t.Fatalf("cursor %s", m.Cursor())
	}
	// The selected row disappears: the cursor keeps its index.
	m.Select("t:al-10")
	idx := m.Index()
	m.Rebuild(s)
	if m.Index() != idx || m.Cursor() != m.Rows[idx].Key {
		t.Fatalf("cursor %s at %d, was %d", m.Cursor(), m.Index(), idx)
	}
	if p := m.Parent(m.Index()); m.Rows[p].Key != "p:alpha" {
		t.Fatalf("parent %d", p)
	}
}

func TestClosed(t *testing.T) {
	m := New()
	m.ShowClosed = true
	m.Rebuild(fixture.Snapshot(true))
	got := keys(&m)
	// al-9 is done: in the deliver group only, not again among the closed tasks.
	if n := slices.Index(got, "  t:al-9"); n < 0 || slices.Contains(got, " t:al-9") {
		t.Fatalf("al-9: %v", got)
	}
	if got[len(got)-1] != " t:be-2" {
		t.Fatalf("closed task last in its project: %v", got)
	}
}

func TestRender(t *testing.T) {
	s := fixture.Snapshot(false)
	m := New()
	m.Rebuild(s)
	in := NewInfo(s, fixture.Live(), fixture.Now, style.Default)
	lines := m.Render(60, 40, true, in)
	text := func(key string) string {
		i := slices.IndexFunc(m.Rows, func(r Row) bool { return r.Key == key })
		return ansi.Strip(lines[i])
	}
	for key, want := range map[string][]string{
		"p:alpha":         {"▾ alpha", "strict", "6 open · 2 ready"},
		"c:alpha":         {"○ coord", "idle 2h", "q91%"},
		"r:al-12-r2":      {"● r2 sonnet", "running 12m", "q91%"},
		"r:al-14-r1":      {"? r1", "waiting 20m"},
		"t:al-14":         {"? al-14 Ask about colors", "waiting 5m"},
		"t:al-13":         {"· al-13 Docs", "ready p2 3h"},
		"t:al-17":         {"al-17 Review of al-12", "30s"},
		"h:al-12":         {"+1 earlier"},
		"g:deliver:alpha": {"⇪ Ready to deliver (1)"},
	} {
		got := text(key)
		if ansi.StringWidth(got) != 60 {
			t.Errorf("%s: width %d", key, ansi.StringWidth(got))
		}
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", key, got, w)
			}
		}
	}
	// A gone pane of a live run: ✕ and DEAD.
	live := fixture.Live()
	l := live["al-12-r2"]
	l.State = "gone"
	live["al-12-r2"] = l
	lines = m.Render(60, 40, true, NewInfo(s, live, fixture.Now, style.Default))
	if got := text("r:al-12-r2"); !strings.Contains(got, "✕ r2") || !strings.Contains(got, "DEAD") {
		t.Errorf("dead run: %q", got)
	}
}

// A filter keeps the matches and their ancestors, expands what hides a match and leaves
// the explicit expansion alone.
func TestFilter(t *testing.T) {
	m := New()
	m.Rebuild(fixture.Snapshot(false))
	m.SetExpanded("t:al-12", false)
	m.Rebuild(fixture.Snapshot(false))
	m.Filter = "AL-12-R2"
	m.Rebuild(fixture.Snapshot(false))
	if got, want := keys(&m), []string{"p:alpha", " t:al-12", "  r:al-12-r2"}; !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q", got, want)
	}
	m.Filter = "beta"
	m.Rebuild(fixture.Snapshot(false))
	if got := keys(&m); !slices.Equal(got, []string{"p:beta", " t:be-1"}) {
		t.Fatalf("project match: %q", got)
	}
	m.Filter = ""
	m.Rebuild(fixture.Snapshot(false))
	if slices.Contains(keys(&m), " r:al-12-r2") || slices.Contains(keys(&m), "  r:al-12-r2") {
		t.Fatalf("the explicit collapse was lost: %q", keys(&m))
	}
	e := m.Expansion()
	if on, ok := e["t:al-12"]; !ok || on {
		t.Fatalf("expansion %v", e)
	}
	n := New()
	n.SetExpansion(e)
	n.Rebuild(fixture.Snapshot(false))
	if !slices.Equal(keys(&n), keys(&m)) {
		t.Fatal("a restored expansion builds other rows")
	}
}
