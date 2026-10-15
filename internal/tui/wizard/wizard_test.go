package wizard

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/form"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

const home = "/Users/me"

func detection(harnesses ...string) setup.Detection {
	d := setup.Detection{Tmux: setup.Tool{Name: "tmux", Path: "/opt/homebrew/bin/tmux", Version: "tmux 3.5a"},
		Git: setup.Tool{Name: "git", Path: "/usr/bin/git", Version: "git version 2.50.1"}, Repo: home + "/dev/myapp"}
	for _, h := range registry.Harnesses {
		t := setup.Tool{Name: h}
		if slices.Contains(harnesses, h) {
			t.Path, t.Version, t.ConfigDir = "/opt/homebrew/bin/"+h, h+" 1.2.3", home+"/."+h
		}
		d.Harnesses = append(d.Harnesses, t)
	}
	return d
}

// h drives a wizard model without a terminal.
type h struct {
	t       *testing.T
	m       *Model
	applied []setup.Choices
	err     error // what Apply returns
}

func newH(t *testing.T, mode Mode, b setup.Base, w, ht int) *h {
	t.Helper()
	x := &h{t: t}
	c := b.Defaults()
	o := Options{Mode: mode, Base: b, Choices: c, OfferTUI: mode == Init,
		Preview: func(c setup.Choices) Preview {
			files, err := b.Files(c, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
			if err != nil {
				return Preview{Err: err.Error()}
			}
			return Preview{Files: files}
		},
		Apply: func(_ context.Context, c setup.Choices) (Report, error) {
			x.applied = append(x.applied, c)
			return Report{Lines: []string{"wrote ~/.config/otag/registry.toml", "synced the database"}}, x.err
		}}
	x.m = New(context.Background(), o)
	x.send(tea.WindowSizeMsg{Width: w, Height: ht})
	return x
}

func base(harnesses ...string) setup.Base {
	return setup.Base{ConfigDir: home + "/.config/otag", Home: home, Detect: detection(harnesses...)}
}

// send delivers msg and runs the command it returns (once, synchronously): the
// apply's result comes back as the next message.
func (x *h) send(msg tea.Msg) {
	x.t.Helper()
	_, cmd := x.m.Update(msg)
	for cmd != nil {
		out := cmd()
		cmd = nil
		if _, ok := out.(appliedMsg); ok {
			_, cmd = x.m.Update(out)
		}
	}
}

var named = map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"tab": tea.KeyTab, "enter": tea.KeyEnter, "esc": tea.KeyEscape, "space": tea.KeySpace, "backspace": tea.KeyBackspace,
	"pgdown": tea.KeyPgDown, "pgup": tea.KeyPgUp}

func keyMsg(s string) tea.KeyPressMsg {
	var mod tea.KeyMod
	if r, ok := strings.CutPrefix(s, "ctrl+"); ok {
		mod, s = tea.ModCtrl, r
	}
	if r, ok := strings.CutPrefix(s, "shift+"); ok {
		mod, s = tea.ModShift, r
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

func (x *h) key(keys ...string) {
	x.t.Helper()
	for _, k := range keys {
		x.send(keyMsg(k))
	}
}

func (x *h) typeText(s string) {
	for _, r := range s {
		x.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func (x *h) page() kind { return x.m.pages[x.m.cur].kind }

// clickField clicks the field with id (or the button with id) through the layout.
func (x *h) clickField(id string) {
	x.t.Helper()
	x0, y0 := x.m.formOrigin()
	_, hits := x.m.f.View(x.m.formWidth(), x.m.theme)
	for _, hit := range hits {
		if hit.Field >= 0 && x.m.f.Fields[hit.Field].ID == id || hit.Button >= 0 && x.m.f.Buttons[hit.Button].ID == id {
			x.send(tea.MouseClickMsg{X: x0 + hit.X, Y: y0 + hit.Y, Button: tea.MouseLeft})
			return
		}
	}
	x.t.Fatalf("no hit for %s", id)
}

func (x *h) screen() string {
	got := ansi.Strip(x.m.View().Content)
	var b strings.Builder
	for _, l := range strings.Split(got, "\n") {
		b.WriteString(strings.TrimRight(l, " ") + "\n")
	}
	return b.String()
}

func golden(t *testing.T, x *h, name string) {
	t.Helper()
	content := x.m.View().Content
	lines := strings.Split(content, "\n")
	if len(lines) != x.m.h {
		t.Errorf("%s: %d lines, want %d", name, len(lines), x.m.h)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > x.m.w {
			t.Errorf("%s: line %d is %d cells wide", name, i, w)
		}
	}
	got := x.screen()
	file := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
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
		b    func() setup.Base
		mode Mode
		w, h int
		do   func(x *h)
	}{
		{name: "welcome", do: func(*h) {}},
		{name: "agents", do: func(x *h) { x.key("ctrl+s") }},
		{name: "agents-no-claude", do: func(x *h) { x.key("ctrl+s", "space") }},
		{name: "profiles-claude", do: func(x *h) { x.key("ctrl+s", "ctrl+s") }},
		{name: "profiles-codex", do: func(x *h) { x.key("ctrl+s", "ctrl+s", "ctrl+s") }},
		{name: "project", do: func(x *h) { x.key("ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s") }},
		{name: "daemon", do: func(x *h) { x.key("ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s") }},
		{name: "skill", do: func(x *h) { x.key("ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s", "ctrl+s") }},
		{name: "review", do: func(x *h) { x.key(slices.Repeat([]string{"ctrl+s"}, 8)...) }},
		{name: "done", do: func(x *h) { x.key(slices.Repeat([]string{"ctrl+s"}, 8)...); x.key("enter") }},
		{name: "review-wide", w: 120, h: 40, do: func(x *h) { x.key(slices.Repeat([]string{"ctrl+s"}, 8)...) }},
		{name: "blocked", b: func() setup.Base {
			b := base("claude")
			b.Detect.Tmux = setup.Tool{Name: "tmux"}
			return b
		}, do: func(*h) {}},
		{name: "no-agent", b: func() setup.Base { return base() }, do: func(*h) {}},
		{name: "project-mode", mode: Project, b: func() setup.Base {
			b := base("claude", "codex")
			b.Detect = setup.Detection{Repo: home + "/dev/myapp"}
			b.RegistryExists = true
			b.Existing = &registry.Config{
				Accounts: map[string]registry.Account{"claude": {Harness: "claude"}, "codex": {Harness: "codex"}},
				Profiles: map[string]registry.Profile{"claude-opus": {Account: "claude", Tags: []string{"coordinator"}},
					"claude-sonnet": {Account: "claude"}, "codex": {Account: "codex", Tags: []string{"smart"}}}}
			return b
		}, do: func(*h) {}},
		{name: "too-small", w: 50, h: 12, do: func(*h) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := base("claude", "codex", "opencode")
			if c.b != nil {
				b = c.b()
			}
			w, ht := c.w, c.h
			if w == 0 {
				w, ht = 80, 24
			}
			x := newH(t, c.mode, b, w, ht)
			c.do(x)
			golden(t, x, c.name)
		})
	}
}

func TestFlowWritesChoices(t *testing.T) {
	x := newH(t, Init, base("claude", "codex", "opencode"), 80, 24)
	x.key("enter") // Continue
	if x.page() != pgAgents {
		t.Fatalf("page %v", x.page())
	}
	x.key("down", "space", "ctrl+s") // codex off
	if slices.ContainsFunc(x.m.pages, func(p page) bool { return p.harness == "codex" }) {
		t.Fatal("the codex profile page is still there")
	}
	// Claude profiles: haiku off, sonnet's effort to medium, opus's model edited.
	if x.page() != pgProfiles || x.m.pages[x.m.cur].harness != "claude" {
		t.Fatalf("page %+v", x.m.pages[x.m.cur])
	}
	x.key("tab", "backspace", "backspace", "backspace", "backspace")
	x.typeText("claude-opus-4")
	x.key("tab", "tab", "tab", "tab", "right", "right") // sonnet effort: default → low → medium
	x.key("tab", "space")                               // haiku off
	if !strings.Contains(x.screen(), "claude-opus → opencode → claude-sonnet") {
		t.Fatalf("fallback note not refreshed:\n%s", x.screen())
	}
	x.key("ctrl+s", "ctrl+s") // opencode page, project page
	if x.page() != pgProject {
		t.Fatalf("page %v", x.page())
	}
	x.key("down", "down", "backspace", "backspace", "backspace")
	x.typeText("ma")
	x.key("ctrl+s", "ctrl+s", "space", "ctrl+s") // daemon on, skill on
	if x.page() != pgReview {
		t.Fatalf("page %v:\n%s", x.page(), x.screen())
	}
	if !strings.Contains(x.screen(), "create  ~/.config/otag/registry.toml") ||
		!strings.Contains(x.screen(), "create  ~/.config/otag/projects/myapp.toml") ||
		!strings.Contains(x.screen(), "otag skill install") {
		t.Fatalf("review:\n%s", x.screen())
	}
	if len(x.applied) != 0 {
		t.Fatal("applied before Write")
	}
	x.key("enter") // Write
	if len(x.applied) != 1 || x.page() != pgDone || !x.m.result.Applied {
		t.Fatalf("applied %d, page %v, result %+v", len(x.applied), x.page(), x.m.result)
	}
	c := x.applied[0]
	var names []string
	for _, p := range c.Active() {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"claude-opus", "claude-sonnet", "opencode"}) {
		t.Fatalf("profiles %v", names)
	}
	opus, sonnet := c.Active()[0], c.Active()[1]
	if opus.Model != "claude-opus-4" || sonnet.Effort != "medium" || !slices.Equal(opus.Fallback, []string{"opencode", "claude-sonnet"}) {
		t.Fatalf("opus %+v sonnet %+v", opus, sonnet)
	}
	if p := c.Project; p == nil || p.Prefix != "ma" || p.AutoPrefix || !p.AutoName || p.Review != "opencode" || !c.Daemon || !c.Skill {
		t.Fatalf("project %+v daemon %v skill %v", p, c.Daemon, c.Skill)
	}
	x.key("y")
	if !x.m.done || !x.m.result.OpenTUI {
		t.Fatalf("result %+v", x.m.result)
	}
}

func TestCancelWritesNothing(t *testing.T) {
	for _, keys := range [][]string{{"esc"}, {"ctrl+c"}, {"ctrl+s", "ctrl+s", "esc"}, {"ctrl+s", "tab", "tab", "tab", "enter"}} {
		x := newH(t, Init, base("claude"), 80, 24)
		x.key(keys...)
		if !x.m.done || x.m.result.Applied || len(x.applied) != 0 || x.m.result.OpenTUI {
			t.Fatalf("%v: done %v result %+v applied %d", keys, x.m.done, x.m.result, len(x.applied))
		}
	}
}

func TestBackKeepsEdits(t *testing.T) {
	x := newH(t, Init, base("claude", "codex"), 80, 24)
	x.key("ctrl+s", "ctrl+s") // Claude profiles
	x.key("space")            // opus off
	x.clickField("back")
	if x.page() != pgAgents {
		t.Fatalf("page %v", x.page())
	}
	x.key("ctrl+s")
	if x.m.f.Get("on:claude-opus") != "" {
		t.Fatal("opus is on again after Back")
	}
	// The project's coordinator follows: no Claude smart profile left but sonnet.
	x.key("ctrl+s", "ctrl+s")
	if x.page() != pgProject || x.m.f.Get("coord") != "claude-sonnet" {
		t.Fatalf("page %v coord %q", x.page(), x.m.f.Get("coord"))
	}
}

func TestProjectChecks(t *testing.T) {
	x := newH(t, Init, base("claude"), 80, 24)
	x.key("ctrl+s", "ctrl+s", "ctrl+s") // agents, profiles → project
	x.key("down", "down", "backspace", "backspace", "backspace")
	x.typeText("9x")
	x.key("ctrl+s")
	if x.page() != pgProject || !strings.Contains(x.m.f.Err, "prefix") {
		t.Fatalf("page %v err %q", x.page(), x.m.f.Err)
	}
	// Unchecking Register skips the checks and the project.
	x.clickField("register")
	x.key("ctrl+s", "ctrl+s", "ctrl+s")
	if x.page() != pgReview || strings.Contains(x.screen(), "projects/") {
		t.Fatalf("page %v:\n%s", x.page(), x.screen())
	}
	x.key("enter")
	if c := x.applied[0]; c.Project != nil {
		t.Fatalf("project %+v", c.Project)
	}
}

func TestMouse(t *testing.T) {
	x := newH(t, Init, base("claude", "codex"), 100, 30)
	x.clickField("next")
	if x.page() != pgAgents {
		t.Fatalf("page %v", x.page())
	}
	x.clickField("acc:claude")
	if x.m.f.Get("acc:claude") != "" || !strings.Contains(x.screen(), "Without Claude Code there is no coordinator") {
		t.Fatalf("click did not toggle:\n%s", x.screen())
	}
	x.clickField("cancel")
	if !x.m.done || len(x.applied) != 0 {
		t.Fatal("cancel by click")
	}
}

func TestApplyError(t *testing.T) {
	x := newH(t, Init, base("claude"), 80, 24)
	x.err = errors.New("daemon install: launchctl failed")
	x.key(slices.Repeat([]string{"ctrl+s"}, 6)...)
	if x.page() != pgReview {
		t.Fatalf("page %v", x.page())
	}
	x.key("enter")
	if x.page() != pgDone || x.m.result.Applied || !strings.Contains(x.screen(), "launchctl failed") {
		t.Fatalf("result %+v:\n%s", x.m.result, x.screen())
	}
	x.key("y") // no TUI after a failure
	if x.m.done {
		t.Fatal("y quit after a failure")
	}
	x.key("enter")
	if !x.m.done || x.m.result.OpenTUI {
		t.Fatalf("result %+v", x.m.result)
	}
}

func TestBlocked(t *testing.T) {
	b := base("claude")
	b.Detect.Git = setup.Tool{Name: "git"}
	x := newH(t, Init, b, 80, 24)
	if !strings.Contains(x.screen(), "xcode-select --install") || len(x.m.f.Buttons) != 1 {
		t.Fatalf("blocked page:\n%s", x.screen())
	}
	x.key("enter")
	if !x.m.done || len(x.applied) != 0 {
		t.Fatal("quit")
	}
}

func TestReviewRefusesInvalid(t *testing.T) {
	x := newH(t, Init, base("claude"), 80, 24)
	x.m.o.Preview = func(setup.Choices) Preview { return Preview{Err: "invalid configuration: boom"} }
	x.key(slices.Repeat([]string{"ctrl+s"}, 6)...)
	x.key("enter")
	if len(x.applied) != 0 || !strings.Contains(x.m.f.Err, "boom") {
		t.Fatalf("applied %d err %q", len(x.applied), x.m.f.Err)
	}
}

func TestProjectMode(t *testing.T) {
	b := base()
	b.Detect = setup.Detection{Repo: home + "/dev/myapp"}
	b.RegistryExists = true
	b.Existing = &registry.Config{Accounts: map[string]registry.Account{"c": {Harness: "claude"}},
		Profiles: map[string]registry.Profile{"sonnet": {Account: "c"}}}
	x := newH(t, Project, b, 80, 24)
	if x.page() != pgProject || x.m.f.Field("register") != nil {
		t.Fatalf("page %v", x.page())
	}
	x.key("ctrl+s", "enter")
	if len(x.applied) != 1 || x.applied[0].Project == nil || x.applied[0].Project.Default != "sonnet" {
		t.Fatalf("applied %+v", x.applied)
	}
	if strings.Contains(x.screen(), "Open the TUI") {
		t.Fatal("project mode offers the TUI")
	}
	x.key("enter")
	if !x.m.done || x.m.result.OpenTUI {
		t.Fatal("close")
	}
}

func TestScrollLongReview(t *testing.T) {
	x := newH(t, Init, base("claude", "codex", "opencode"), 80, 18)
	x.key(slices.Repeat([]string{"ctrl+s"}, 8)...)
	fl := x.m.f.Fields[0]
	if fl.Kind != form.Note || fl.Label == "" {
		t.Fatalf("the plan fits 80x18? label %q", fl.Label)
	}
	before := x.screen()
	x.key("pgdown")
	if x.screen() == before || fl.Off == 0 {
		t.Fatal("pgdown did not scroll")
	}
	x.key("pgup")
	if fl.Off != 0 {
		t.Fatalf("off %d", fl.Off)
	}
}

// The project page asks for the branch prefix (feature/), the base (blank; the
// repository's remote default branch is the placeholder) and the push policy
// (coordinator); what is chosen reaches the written project, and a bad prefix is
// refused on the page.
func TestProjectMRFlowFields(t *testing.T) {
	b := base()
	b.Detect = setup.Detection{Repo: home + "/dev/myapp", RepoBase: "origin/trunk"}
	b.RegistryExists = true
	b.Existing = &registry.Config{Accounts: map[string]registry.Account{"c": {Harness: "claude"}},
		Profiles: map[string]registry.Profile{"sonnet": {Account: "c"}}}
	x := newH(t, Project, b, 80, 30)
	f := x.m.f
	if f.Get("branch_prefix") != "feature/" || f.Get("base") != "" || f.Get("push") != "coordinator" ||
		!strings.Contains(f.Field("base").Placeholder, "origin/trunk") {
		t.Fatalf("defaults %q %q %q %q", f.Get("branch_prefix"), f.Get("base"), f.Get("push"), f.Field("base").Placeholder)
	}
	f.Field("branch_prefix").Ed.Set("bad..prefix/")
	x.key("ctrl+s")
	if x.page() != pgProject || !strings.Contains(x.m.f.Err, "prefix") {
		t.Fatalf("bad prefix: page %v err %q", x.page(), x.m.f.Err)
	}
	f.Field("branch_prefix").Ed.Set("team/")
	f.Field("base").Ed.Set("origin/trunk")
	push := f.Field("push")
	for i, o := range push.Opts {
		if o.Value == "child" {
			push.Sel = i
		}
	}
	x.key("ctrl+s", "enter")
	if len(x.applied) != 1 || x.applied[0].Project == nil {
		t.Fatalf("applied %+v", x.applied)
	}
	p := x.applied[0].Project
	if p.BranchPrefix != "team/" || p.Base != "origin/trunk" || p.Push != "child" {
		t.Fatalf("project %+v", p)
	}
}
