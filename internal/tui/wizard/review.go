package wizard

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/form"
)

// pageNote is the live note of a page: the fallbacks of a Profiles page, the
// coordinator note of Agents.
func (m *Model) pageNote(p page) string {
	if p.kind == pgAgents {
		on := map[string]bool{}
		for _, a := range m.c.Accounts {
			on[a.Harness] = a.On
		}
		return m.coordNote(on)
	}
	var lines []string
	for _, pr := range m.c.Active() {
		if pr.Harness == p.harness && len(pr.Fallback) > 0 {
			lines = append(lines, fmt.Sprintf("  %s → %s", pr.Name, strings.Join(pr.Fallback, " → ")))
		}
	}
	if len(lines) == 0 {
		return "\nFallbacks: none (no other profile to fall back to)."
	}
	return "\nFallbacks, set for you (used on quota limits and failed launches):\n" + strings.Join(lines, "\n")
}

// projectForm proposes the repository as a project.
func (m *Model) projectForm() *form.Form {
	p := m.proj
	m.fixProject()
	refs := m.o.Base.Refs(m.c)
	var fields []*form.Field
	if m.o.Mode == Init {
		fields = append(fields, check("register", "Register "+m.home(p.Path)+" as a project", m.c.Project != nil))
	} else {
		fields = append(fields, note("path", "Register "+m.home(p.Path)+" as an otag project."))
	}
	fields = append(fields,
		text("name", "name", p.Name, ""),
		text("prefix", "prefix", p.Prefix, "task ids: <prefix>-x3f"),
		choose("workspace", "workspace", []string{"worktree", "same"},
			[]string{"worktree (a git worktree per task)", "same (the checkout itself)"}, p.Workspace),
		choose("guard", "guard", []string{"light", "strict"},
			[]string{"light (default)", "strict (sandboxed)"}, p.Guard),
		m.profileSelect("coord", "coordinator", refs, p.Coordinator, func(r setup.Ref) bool { return r.Harness == "claude" }),
		m.profileSelect("default", "default", refs, p.Default, nil),
		m.profileSelect("review", "review", refs, p.Review, nil),
		text("deny", "deny", strings.Join(p.Deny, ", "), "comma-separated globs"),
	)
	n := "\nThe coordinator plans and starts tasks; children run the default profile; a review is best on " +
		"another model family."
	if !slices.ContainsFunc(refs, func(r setup.Ref) bool { return r.Harness == "claude" }) {
		n = "\nNo Claude profile: the coordinator is unavailable; tasks still run on the other agents."
	}
	fields = append(fields, note("note", n))
	return form.New("Project", m.navButtons("Next"), fields...)
}

// profileSelect chooses one of refs (ok filters them; nil: all) or none.
func (m *Model) profileSelect(id, label string, refs []setup.Ref, cur string, ok func(setup.Ref) bool) *form.Field {
	values, labels := []string{""}, []string{"(none)"}
	for _, r := range refs {
		if ok == nil || ok(r) {
			values = append(values, r.Name)
			labels = append(labels, r.Name+" ("+r.Harness+")")
		}
	}
	return choose(id, label, values, labels, cur)
}

func (m *Model) daemonForm() *form.Form {
	return form.New("Background service", m.navButtons("Next"),
		check("daemon", "Install the otag daemon (launchd)", m.c.Daemon),
		note("note", "\nThe daemon (launch agent io.github.nesiler.otag) reaps crashed runs, restarts stalled ones, "+
			"falls back to another profile on quota limits and retries queued messages. Same as "+
			"`otag daemon install`; `otag daemon uninstall` removes it."))
}

func (m *Model) skillForm() *form.Form {
	return form.New("Agent skill", m.navButtons("Next"),
		check("skill", "Install the otag skill for Claude Code, Codex and OpenCode", m.c.Skill),
		note("note", "\nIt teaches the agents when to use otag. It writes global agent config "+
			"(~/.agents/skills/otag and a link in ~/.claude/skills), so it is off by default. "+
			"Same as `otag skill install`."))
}

// planNote is a long read-only text in rows rows that scrolls (pgup pgdn, the
// wheel); its label says so when it does not fit.
func (m *Model) planNote(id, body string, rows int) *form.Field {
	fl := note(id, strings.TrimRight(body, "\n"))
	n := 0
	for _, l := range strings.Split(fl.Text, "\n") {
		n += len(strings.Split(ansi.Wrap(l, m.formWidth(), ""), "\n"))
	}
	fl.Height = max(3, rows)
	if n > fl.Height {
		fl.Height--
		fl.Label = fmt.Sprintf("(%d lines: pgup pgdn or the wheel scroll)", n)
	}
	return fl
}

// reviewForm lists exactly what Write does.
func (m *Model) reviewForm() *form.Form {
	var s strings.Builder
	s.WriteString("Files:\n")
	if len(m.review.Files) == 0 {
		s.WriteString("  (none)\n")
	}
	for _, f := range m.review.Files {
		fmt.Fprintf(&s, "  %-7s %s\n", f.Action, m.home(f.Path))
		if f.Backup != "" {
			fmt.Fprintf(&s, "          (the old file is kept as %s)\n", m.home(f.Backup))
		}
	}
	s.WriteString("Then:\n")
	if len(m.review.Files) > 0 {
		s.WriteString("  otag sync             load the config into the database\n")
	}
	if m.c.Daemon {
		s.WriteString("  otag daemon install   start the background service\n")
	}
	if m.c.Skill {
		s.WriteString("  otag skill install    the agent skill\n")
	}
	if p := m.c.Project; p != nil {
		fmt.Fprintf(&s, "Project %s (prefix %s, %s, guard %s):\n  coordinator %s · default %s · review %s\n",
			p.Name, p.Prefix, p.Workspace, p.Guard, dashed(p.Coordinator), dashed(p.Default), dashed(p.Review))
	}
	if active := m.c.Active(); len(active) > 0 {
		s.WriteString("Profiles:\n")
		for _, p := range active {
			model := p.Model
			if model == "" {
				model = "default model"
			}
			if p.Effort != "" {
				model += ", " + p.Effort
			}
			fb := ""
			if len(p.Fallback) > 0 {
				fb = "  → " + strings.Join(p.Fallback, ", ")
			}
			fmt.Fprintf(&s, "  %-14s %s%s\n", p.Name, model, fb)
		}
	}
	for _, w := range m.review.Warnings {
		s.WriteString("warning: " + w + "\n")
	}
	_, _, _, bh := m.boxRect()
	rows := bh - 2 - 2 - 2 // the border, the title and its blank line, the blank line and the buttons
	if m.review.Err != "" {
		rows -= len(strings.Split(ansi.Wrap(m.review.Err, m.formWidth(), ""), "\n"))
	}
	f := form.New("Review", []form.Button{{ID: "write", Label: "Write"}, {ID: "back", Label: "Back"},
		{ID: "cancel", Label: "Cancel", Cancel: true}}, m.planNote("plan", s.String(), rows))
	f.Err = m.review.Err
	return f
}

func dashed(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// doneForm reports what Apply did and offers the TUI.
func (m *Model) doneForm() *form.Form {
	r := m.result
	var s strings.Builder
	for _, l := range r.Report.Lines {
		s.WriteString(l + "\n")
	}
	for _, w := range r.Report.Warnings {
		s.WriteString("warning: " + w + "\n")
	}
	title := "otag is set up"
	if m.o.Mode == Project {
		title = "Project added"
	}
	coord := "otag coord <project> --attach"
	if p := r.Choices.Project; p != nil {
		coord = "otag coord " + p.Name + " --attach"
	}
	var buttons []form.Button
	switch {
	case r.Err != "":
		title = "Setup did not complete"
		s.WriteString("\nerror: " + r.Err + "\n")
		buttons = []form.Button{{ID: "quit", Label: "Close", Cancel: true}}
	case m.o.OfferTUI:
		s.WriteString("\nNext: start the coordinator with `" + coord + "`, or open the TUI and press c on a project.\n\nOpen the TUI now?")
		buttons = []form.Button{{ID: "tui", Label: "Open the TUI (y)"}, {ID: "quit", Label: "Quit (n)", Cancel: true}}
	default:
		s.WriteString("\nNext: `" + coord + "` starts its coordinator.")
		buttons = []form.Button{{ID: "quit", Label: "Close", Cancel: true}}
	}
	_, _, _, bh := m.boxRect()
	return form.New(title, buttons, m.planNote("report", s.String(), bh-2-2-2))
}
