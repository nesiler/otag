// Package wizard is otag's setup wizard (`otag init`, bare `otag` on a fresh machine,
// and the confirm step of `otag project add`): a short sequence of form pages built
// with the TUI's dialog kit (internal/tui/form) and theme. The model is pure: the
// detection results and the current choices come in through Options, and the only
// side effects (preview and apply) are functions there, so it is tested without a
// terminal or any file writes. Nothing is written before the Review page's Write.
package wizard

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/form"
	"github.com/nesiler/otag/internal/tui/style"
)

// Mode selects the pages.
type Mode int

// Modes.
const (
	// Init is the whole setup: detect, agents, profiles, project, daemon, skill, review.
	Init Mode = iota
	// Project is `otag project add` on a terminal: the project page and the review.
	Project
)

// Preview is what the Review page shows: the files that would be written and the
// warnings the result would load with (Err: it would not load, Write is refused).
type Preview struct {
	Files    []setup.File
	Warnings []string
	Err      string
}

// Report is what Apply did, one line per step, and its warnings.
type Report struct {
	Lines    []string
	Warnings []string
}

// Options configure the wizard.
type Options struct {
	Mode    Mode
	Base    setup.Base
	Choices setup.Choices // the proposals (setup.Base.Defaults)
	// Preview computes the Review page for the current choices (no writes).
	Preview func(c setup.Choices) Preview
	// Apply writes the files and runs sync, daemon and skill install as chosen.
	Apply func(ctx context.Context, c setup.Choices) (Report, error)
	// Offer the TUI at the end (Init mode).
	OfferTUI bool
}

// Result is how the wizard ended.
type Result struct {
	Applied bool          `json:"applied"`
	OpenTUI bool          `json:"open_tui"`
	Choices setup.Choices `json:"choices"`
	Report  Report        `json:"report"`
	Err     string        `json:"error,omitempty"`
}

// page kinds.
type kind int

const (
	pgDetect kind = iota
	pgAgents
	pgProfiles // one per chosen harness
	pgProject
	pgDaemon
	pgSkill
	pgReview
	pgDone
)

type page struct {
	kind    kind
	harness string // pgProfiles
}

// Model is the wizard's Bubble Tea model.
type Model struct {
	o      Options
	ctx    context.Context
	c      setup.Choices
	pages  []page
	cur    int
	f      *form.Form
	w, h   int
	theme  style.Theme
	review Preview

	working bool
	done    bool
	result  Result
	// proj is the proposed project, kept while its Register box is unchecked (the
	// choices then have none); nil: there is no repository to register.
	proj *setup.Project
}

// New returns the wizard on its first page.
func New(ctx context.Context, o Options) *Model {
	m := &Model{o: o, ctx: ctx, c: o.Choices, w: 80, h: 24, theme: style.Default}
	if p := o.Choices.Project; p != nil {
		cp := *p
		m.proj = &cp
		m.c.Project = m.proj
	}
	m.pages = m.buildPages()
	m.enter(0)
	return m
}

// Result is the outcome once the program ended.
func (m *Model) Result() Result { return m.result }

// buildPages lists the pages for the current choices: the profile pages follow the
// chosen harnesses.
func (m *Model) buildPages() []page {
	if m.o.Mode == Project {
		return []page{{kind: pgProject}, {kind: pgReview}, {kind: pgDone}}
	}
	ps := []page{{kind: pgDetect}}
	if len(m.c.Accounts) > 0 {
		ps = append(ps, page{kind: pgAgents})
		for _, a := range m.c.Accounts {
			if a.On {
				ps = append(ps, page{kind: pgProfiles, harness: a.Harness})
			}
		}
	}
	if m.proj != nil {
		ps = append(ps, page{kind: pgProject})
	}
	if !m.o.Base.DaemonInstalled {
		ps = append(ps, page{kind: pgDaemon})
	}
	if !m.o.Base.SkillInstalled {
		ps = append(ps, page{kind: pgSkill})
	}
	return append(ps, page{kind: pgReview}, page{kind: pgDone})
}

// blocked: tmux or git is missing, or there is no agent to set up; the detect page
// only offers Quit.
func (m *Model) blocked() string {
	if miss := m.o.Base.Detect.Missing(); len(miss) > 0 {
		return strings.Join(miss, " and ")
	}
	if m.o.Mode == Init && !m.existingAny() && len(m.o.Base.Detect.Found()) == 0 {
		return "agent"
	}
	return ""
}

// existingAny: the kept registry has profiles (then adding none is fine).
func (m *Model) existingAny() bool {
	e := m.o.Base.Existing
	return e != nil && len(e.Profiles) > 0
}

// enter shows page i with a fresh form built from the choices.
func (m *Model) enter(i int) {
	m.cur = max(0, min(i, len(m.pages)-1))
	p := m.pages[m.cur]
	switch p.kind {
	case pgDetect:
		m.f = m.detectForm()
	case pgAgents:
		m.f = m.agentsForm()
	case pgProfiles:
		m.f = m.profilesForm(p.harness)
	case pgProject:
		m.f = m.projectForm()
	case pgDaemon:
		m.f = m.daemonForm()
	case pgSkill:
		m.f = m.skillForm()
	case pgReview:
		m.review = Preview{}
		if m.o.Preview != nil {
			m.review = m.o.Preview(m.c)
		}
		m.f = m.reviewForm()
	case pgDone:
		m.f = m.doneForm()
	}
}

// navButtons are Next / Back / Cancel (no Back on the first page).
func (m *Model) navButtons(next string) []form.Button {
	bs := []form.Button{{ID: "next", Label: next}}
	if m.cur > 0 {
		bs = append(bs, form.Button{ID: "back", Label: "Back"})
	}
	return append(bs, form.Button{ID: "cancel", Label: "Cancel", Cancel: true})
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd { return tea.RequestBackgroundColor }

type appliedMsg struct {
	report Report
	err    error
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.theme = style.ForBackground(msg.IsDark())
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.PasteMsg:
		if !m.working {
			m.f.Paste(msg.Content)
		}
	case tea.MouseClickMsg:
		return m, m.click(msg.Mouse())
	case tea.MouseWheelMsg:
		m.wheel(msg.Mouse())
	case appliedMsg:
		m.working = false
		m.result.Applied = msg.err == nil
		m.result.Report = msg.report
		if msg.err != nil {
			m.result.Err = msg.err.Error()
		}
		m.result.Choices = m.c
		m.enter(len(m.pages) - 1)
	}
	return m, nil
}

func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	if m.working {
		return nil // the apply runs to its end; its result is the next page
	}
	if k.String() == "ctrl+c" {
		return m.quit(false)
	}
	if kd := m.pages[m.cur].kind; kd == pgReview || kd == pgDone {
		// The plan or report scrolls with the page keys; the focus stays on the buttons.
		if fl := m.f.Fields[0]; fl.Kind == form.Note {
			switch k.String() {
			case "pgup":
				fl.Off = max(0, fl.Off-max(1, fl.Height-1))
				return nil
			case "pgdown":
				fl.Off += max(1, fl.Height-1) // View clamps it
				return nil
			}
		}
	}
	if m.pages[m.cur].kind == pgDone {
		switch k.String() {
		case "y", "Y":
			if m.result.Applied && m.o.OfferTUI {
				return m.quit(true)
			}
		case "n", "N", "q", "esc":
			return m.quit(false)
		}
	}
	ev, id := m.f.Key(k, m.formWidth())
	return m.event(ev, id)
}

func (m *Model) click(ev tea.Mouse) tea.Cmd {
	if m.working || ev.Button != tea.MouseLeft {
		return nil
	}
	x0, y0 := m.formOrigin()
	_, hits := m.f.View(m.formWidth(), m.theme)
	for _, h := range hits {
		x, y := ev.X-x0, ev.Y-y0
		if x >= h.X && x < h.X+h.W && y >= h.Y && y < h.Y+h.H {
			e, id := m.f.Click(h, x, y, false, m.formWidth())
			return m.event(e, id)
		}
	}
	return nil
}

func (m *Model) wheel(ev tea.Mouse) {
	n := 1
	if ev.Button == tea.MouseWheelUp {
		n = -1
	}
	x0, y0 := m.formOrigin()
	_, hits := m.f.View(m.formWidth(), m.theme)
	for _, h := range hits {
		x, y := ev.X-x0, ev.Y-y0
		if h.Field >= 0 && x >= h.X && x < h.X+h.W && y >= h.Y && y < h.Y+h.H {
			m.f.Wheel(h, n*3, m.formWidth())
			return
		}
	}
}

// event acts on a form event: a button, Esc, or a change to refresh.
func (m *Model) event(ev form.Event, id string) tea.Cmd {
	switch ev {
	case form.Cancel:
		return m.quit(false)
	case form.Submit:
		return m.submit(id)
	}
	m.refresh()
	return nil
}

// quit ends the program; before the apply nothing has been written.
func (m *Model) quit(openTUI bool) tea.Cmd {
	m.result.OpenTUI = openTUI
	m.result.Choices = m.c
	m.done = true
	return tea.Quit
}

func (m *Model) submit(id string) tea.Cmd {
	p := m.pages[m.cur]
	switch id {
	case "quit", "cancel":
		return m.quit(false)
	case "tui":
		return m.quit(true)
	case "back":
		m.collect(false)
		m.enter(m.cur - 1)
		return nil
	case "write":
		if m.review.Err != "" {
			m.f.Err = "nothing can be written: " + m.review.Err
			return nil
		}
		m.working = true
		m.f.Status = "writing…"
		c, apply, ctx := m.c, m.o.Apply, m.ctx
		return func() tea.Msg {
			if apply == nil {
				return appliedMsg{err: fmt.Errorf("no apply function")}
			}
			r, err := apply(ctx, c)
			return appliedMsg{report: r, err: err}
		}
	}
	// next
	if err := m.collect(true); err != "" {
		m.f.Err = err
		return nil
	}
	if p.kind == pgAgents {
		m.pages = m.buildPages()
	}
	m.enter(m.cur + 1)
	return nil
}

// collect reads the page's fields into the choices; check validates them ("" = ok).
func (m *Model) collect(check bool) string {
	switch m.pages[m.cur].kind {
	case pgAgents:
		for i := range m.c.Accounts {
			m.c.Accounts[i].On = m.f.Get("acc:"+m.c.Accounts[i].Harness) == "true"
		}
		if check && len(m.c.Active()) == 0 && !m.existingAny() {
			return "choose at least one agent"
		}
		m.c.Wire(m.o.Base)
		m.fixProject()
	case pgProfiles:
		h := m.pages[m.cur].harness
		for i := range m.c.Profiles {
			p := &m.c.Profiles[i]
			if p.Harness != h {
				continue
			}
			p.On = m.f.Get("on:"+p.Name) == "true"
			p.Model = strings.TrimSpace(m.f.Get("model:" + p.Name))
			if fl := m.f.Field("effort:" + p.Name); fl != nil {
				p.Effort = fl.Value()
			}
		}
		m.c.Wire(m.o.Base)
		m.fixProject()
		if check && len(m.c.Active()) == 0 && !m.existingAny() {
			return "choose at least one profile"
		}
	case pgProject:
		p := m.proj
		if p == nil {
			return ""
		}
		name, prefix := strings.TrimSpace(m.f.Get("name")), strings.TrimSpace(m.f.Get("prefix"))
		if name != p.Name {
			p.AutoName = false
		}
		if prefix != p.Prefix {
			p.AutoPrefix = false
		}
		p.Name, p.Prefix = name, prefix
		p.Workspace = m.f.Get("workspace")
		p.Guard = m.f.Get("guard")
		p.BranchPrefix = strings.TrimSpace(m.f.Get("branch_prefix"))
		p.Base = strings.TrimSpace(m.f.Get("base"))
		p.Push = m.f.Get("push")
		p.Coordinator, p.Default, p.Review = m.f.Get("coord"), m.f.Get("default"), m.f.Get("review")
		p.Deny = splitList(m.f.Get("deny"))
		m.c.Project = p
		if m.o.Mode == Init && m.f.Get("register") != "true" {
			m.c.Project = nil
			return ""
		}
		if check {
			if err := m.o.Base.CheckProject(*p, m.c); err != nil {
				return err.Error()
			}
		}
	case pgDaemon:
		m.c.Daemon = m.f.Get("daemon") == "true"
	case pgSkill:
		m.c.Skill = m.f.Get("skill") == "true"
	}
	return ""
}

// fixProject keeps the project's profiles valid when the profile set changed: one
// that no longer exists goes back to the default pick.
func (m *Model) fixProject() {
	p := m.proj
	if p == nil {
		return
	}
	refs := m.o.Base.Refs(m.c)
	has := func(n string) bool { return slices.ContainsFunc(refs, func(r setup.Ref) bool { return r.Name == n }) }
	coord, def, review := setup.DefaultProfiles(refs)
	if p.Coordinator == "" || !has(p.Coordinator) {
		p.Coordinator = coord
	}
	if p.Default == "" || !has(p.Default) {
		p.Default = def
	}
	if p.Review == "" || !has(p.Review) {
		p.Review = review
	}
}

// refresh reads the page into the choices after a change (no checks) and updates
// its notes: the coordinator note of Agents, the fallbacks of a Profiles page.
func (m *Model) refresh() {
	switch m.pages[m.cur].kind {
	case pgAgents, pgProfiles:
		m.collect(false)
		if fl := m.f.Field("note"); fl != nil {
			fl.Text = m.pageNote(m.pages[m.cur])
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Layout: a header line, the box (rounded border, padding 1), a footer line.

func (m *Model) boxRect() (x, y, w, h int) {
	w = min(m.w, 84)
	x = (m.w - w) / 2
	return x, 1, w, max(3, m.h-2)
}

func (m *Model) formWidth() int {
	_, _, w, _ := m.boxRect()
	return max(10, w-4)
}

func (m *Model) formOrigin() (int, int) {
	x, y, _, _ := m.boxRect()
	return x + 2, y + 1
}

// tooSmall is the minimum the pages fit in.
func (m *Model) tooSmall() bool { return m.w < 60 || m.h < 18 }

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "otag setup"
	return v
}

func (m *Model) render() string {
	th := m.theme
	if m.tooSmall() {
		return style.Block([]string{"otag setup needs at least 60x18 cells", "(this terminal: " +
			fmt.Sprintf("%dx%d", m.w, m.h) + "); ctrl+c cancels"}, m.w, m.h)
	}
	x, _, bw, bh := m.boxRect()
	lines, _ := m.f.View(m.formWidth(), th)
	body := style.Block(lines, bw-4, bh-2)
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Cyan).Padding(0, 1).
		Width(bw).Height(bh).MaxHeight(bh).Render(body)
	pad := strings.Repeat(" ", x)
	var out []string
	out = append(out, style.Fit(th.Bold.Render(" "+m.appName())+th.Dim.Render(" │ ")+m.stepLabel(), m.w))
	for _, l := range strings.Split(box, "\n") {
		out = append(out, style.Fit(pad+l, m.w))
	}
	out = append(out, th.Footer.Render(style.Fit(" "+m.hint(), m.w)))
	return style.Block(out, m.w, m.h)
}

// appName heads the screen: the command that runs the wizard.
func (m *Model) appName() string {
	if m.o.Mode == Project {
		return "otag project add"
	}
	return "otag setup"
}

// stepLabel is "step 2 of 6: Agents" (pages after Review are not counted).
func (m *Model) stepLabel() string {
	n := len(m.pages) - 1 // without Done
	p := m.pages[m.cur]
	if p.kind == pgDone {
		return m.title(p)
	}
	return fmt.Sprintf("step %d of %d: %s", m.cur+1, n, m.title(p))
}

func (m *Model) title(p page) string {
	switch p.kind {
	case pgDetect:
		return "Welcome"
	case pgAgents:
		return "Agents"
	case pgProfiles:
		return "Profiles: " + setup.HarnessTitle(p.harness)
	case pgProject:
		return "Project"
	case pgDaemon:
		return "Background service"
	case pgSkill:
		return "Agent skill"
	case pgReview:
		return "Review"
	}
	if m.result.Applied {
		return "Done"
	}
	return "Not completed"
}

func (m *Model) hint() string {
	switch {
	case m.working:
		return "writing…"
	case m.pages[m.cur].kind == pgDone && m.result.Applied && m.o.OfferTUI:
		return "y open the TUI · n quit · pgup pgdn scroll"
	case m.pages[m.cur].kind == pgDone:
		return "enter close · pgup pgdn scroll"
	case m.pages[m.cur].kind == pgReview:
		return "←→ tab choose a button · enter press it · pgup pgdn scroll · esc cancel"
	case m.blocked() != "" && m.pages[m.cur].kind == pgDetect:
		return "enter quit"
	}
	return "tab ↑↓ move · space toggle · ←→ choose · ctrl+s next · esc cancel"
}
