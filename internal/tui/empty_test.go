package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/fixture"
)

// emptySnap is a database without projects (otag freshly installed or set up).
func emptySnap() *data.Snapshot {
	s := fixture.Snapshot(false)
	s.Projects, s.Open, s.Deliverable, s.Closed, s.Runs = nil, nil, nil, nil, nil
	s.Needs, s.Asks, s.Inbox, s.Notes, s.Unread = &msg.Needs{}, nil, nil, nil, 0
	s.Quota, s.Cooldown = nil, nil
	return s
}

func emptyHarness(t *testing.T, w, ht int, unconfigured bool, addDir string) *harness {
	t.Helper()
	h := newHarness(t, w, ht)
	h.m.unconfigured, h.m.addDir = unconfigured, addDir
	h.snap = emptySnap
	h.send(loadMsg{snap: emptySnap()})
	return h
}

func TestEmptyStates(t *testing.T) {
	for _, c := range []struct {
		name         string
		unconfigured bool
		addDir       string
		want         string
	}{
		{"empty-unconfigured", true, "", "run `otag init`"},
		{"empty-add", false, "/Users/me/dev/shop", "Press a to add /Users/me/dev/shop"},
		{"empty", false, "", "Run `otag project add` in a git repository"},
	} {
		h := emptyHarness(t, 80, 24, c.unconfigured, c.addDir)
		golden(t, h, c.name)
		if got := strings.Join(strings.Fields(h.screenText()), " "); !strings.Contains(got, c.want) {
			t.Errorf("%s: no %q in\n%s", c.name, c.want, got)
		}
	}
}

// Key a adds the TUI's repository through `otag project add`; without one it says why.
func TestAddProjectKey(t *testing.T) {
	h := emptyHarness(t, 120, 40, false, "/Users/me/dev/shop")
	h.key("a")
	if len(h.calls) != 1 || !slices.Equal(h.calls[0], []string{"project", "add", "/Users/me/dev/shop", "--yes"}) {
		t.Fatalf("calls %q", h.calls)
	}
	h = emptyHarness(t, 120, 40, true, "")
	h.key("a")
	if len(h.calls) != 0 || !h.m.toast.bad || !strings.Contains(h.m.toast.text, "not a project yet") {
		t.Fatalf("calls %q toast %+v", h.calls, h.m.toast)
	}
	// Once the project exists (the fixture's alpha path), there is nothing to add.
	h = newHarness(t, 120, 40)
	h.m.addDir = fixture.Snapshot(false).Projects[0].Path
	h.key("a")
	if len(h.calls) != 0 {
		t.Fatalf("added a registered project: %q", h.calls)
	}
}

func TestAddProjectMenu(t *testing.T) {
	h := emptyHarness(t, 120, 40, false, "/Users/me/dev/shop")
	h.m.openMenu(h.m.appMenu, 5, 5)
	if h.m.menu == nil || h.m.menu.items[0].label != "Add project shop" || h.m.menu.items[0].why != "" {
		t.Fatalf("menu %+v", h.m.menu)
	}
	h = emptyHarness(t, 120, 40, false, "")
	if items := h.m.appMenu(); strings.HasPrefix(items[0].label, "Add project") {
		t.Fatal("an Add project item without a repository")
	}
}
