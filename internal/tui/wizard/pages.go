package wizard

import (
	"fmt"
	"strings"

	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/form"
)

func note(id, text string) *form.Field {
	return &form.Field{Kind: form.Note, ID: id, Text: text}
}

func check(id, label string, on bool) *form.Field {
	return &form.Field{Kind: form.Check, ID: id, Label: label, On: on}
}

func text(id, label, value, placeholder string) *form.Field {
	f := &form.Field{Kind: form.Text, ID: id, Label: label, Placeholder: placeholder}
	f.Ed.Set(value)
	return f
}

// choose is a Select of values with labels; the current value is selected.
func choose(id, label string, values, labels []string, cur string) *form.Field {
	f := &form.Field{Kind: form.Select, ID: id, Label: label}
	for i, v := range values {
		f.Opts = append(f.Opts, form.Option{Value: v, Label: labels[i]})
		if v == cur {
			f.Sel = i
		}
	}
	return f
}

func (m *Model) home(p string) string { return setup.TildePath(p, m.o.Base.Home) }

// detectForm is the welcome page: what was found, and what is missing.
func (m *Model) detectForm() *form.Form {
	b := m.o.Base
	d := b.Detect
	var s strings.Builder
	s.WriteString("otag runs your AI coding agents in tmux and coordinates them. This setup writes its config to " +
		m.home(b.ConfigDir) + "; nothing is written before the last step.\n\nFound on this Mac:\n")
	row := func(ok bool, name, detail string) {
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		fmt.Fprintf(&s, "  %s %-12s %s\n", mark, name, detail)
	}
	for _, t := range d.Harnesses {
		if !t.Found() {
			row(false, setup.HarnessTitle(t.Name), "not found: "+setup.InstallHint(t.Name))
			continue
		}
		detail := orDash(t.Version)
		if t.ConfigDir != "" {
			detail += "  " + m.home(t.ConfigDir)
		}
		row(true, setup.HarnessTitle(t.Name), detail)
	}
	for _, t := range []setup.Tool{d.Tmux, d.Git} {
		if t.Found() {
			row(true, t.Name, orDash(t.Version))
		} else {
			row(false, t.Name, "not found: "+setup.InstallHint(t.Name))
		}
	}
	if d.Repo != "" {
		state := "git repository"
		if b.RepoRegistered(d.Repo) {
			state += ", already registered"
		}
		row(true, "project", m.home(d.Repo)+" ("+state+")")
	}
	if b.RegistryExists && b.Force {
		s.WriteString("\n--force: the current registry.toml is backed up, then replaced.\n")
	} else if e := b.Existing; e != nil {
		fmt.Fprintf(&s, "\nExisting config (%d accounts, %d profiles, %d projects): setup only adds what is missing and keeps your edits.\n",
			len(e.Accounts), len(e.Profiles), len(e.Projects))
	}
	switch why := m.blocked(); why {
	case "":
		return form.New("Welcome to otag", m.navButtons("Continue"), note("found", s.String()))
	case "agent":
		s.WriteString("\nNo agent CLI found. Install one (Claude Code is needed for the coordinator), then run otag again.\n")
	default:
		fmt.Fprintf(&s, "\notag needs %s. Install, then run otag again:\n", why)
		for _, t := range d.Missing() {
			s.WriteString("  " + setup.InstallHint(t) + "\n")
		}
	}
	return form.New("Welcome to otag", []form.Button{{ID: "quit", Label: "Quit", Cancel: true}}, note("found", s.String()))
}

func orDash(s string) string {
	if s == "" {
		return "(version unknown)"
	}
	return s
}

func (m *Model) agentsForm() *form.Form {
	fields := []*form.Field{note("intro", "Choose the agent CLIs otag may run. Each becomes an account that uses "+
		"your normal login (no separate config dir).")}
	on := map[string]bool{}
	for _, a := range m.c.Accounts {
		t := m.o.Base.Detect.Harness(a.Harness)
		label := fmt.Sprintf("%s (%s)  %s", setup.HarnessTitle(a.Harness), a.Harness, orDash(t.Version))
		if a.ConfigDir != "" {
			label += "  config_dir " + m.home(a.ConfigDir)
		}
		fields = append(fields, check("acc:"+a.Harness, label, a.On))
		on[a.Harness] = a.On
	}
	fields = append(fields, note("note", m.coordNote(on)))
	return form.New("Agents", m.navButtons("Next"), fields...)
}

// coordNote says what the agent choice means for the coordinator.
func (m *Model) coordNote(on map[string]bool) string {
	has := on["claude"] || m.existingHarness("claude")
	if has {
		return "\nThe coordinator (the agent that plans and starts tasks) runs on Claude Code."
	}
	return "\nWithout Claude Code there is no coordinator (it runs on Claude only for now); " +
		"child tasks still run on the other agents, started by you or a script."
}

func (m *Model) existingHarness(h string) bool {
	e := m.o.Base.Existing
	if e == nil {
		return false
	}
	for _, a := range e.Accounts {
		if a.Harness == h {
			return true
		}
	}
	return false
}

// profilesForm lists the presets of harness h: on/off, model and effort.
func (m *Model) profilesForm(h string) *form.Form {
	var fields []*form.Field
	efforts := setup.Efforts(h)
	labels := make([]string, len(efforts))
	for i, e := range efforts {
		labels[i] = e
		if e == "" {
			labels[i] = "default"
		}
	}
	for _, p := range m.c.Profiles {
		if p.Harness != h {
			continue
		}
		fields = append(fields, check("on:"+p.Name, p.Name+"  "+p.Description, p.On))
		fields = append(fields, text("model:"+p.Name, "  model", p.Model, modelPlaceholder(h)))
		if len(efforts) > 0 {
			fields = append(fields, choose("effort:"+p.Name, "  effort", efforts, labels, p.Effort))
		}
	}
	fields = append(fields, note("note", m.pageNote(page{kind: pgProfiles, harness: h})))
	return form.New("Profiles: "+setup.HarnessTitle(h), m.navButtons("Next"), fields...)
}

func modelPlaceholder(h string) string {
	switch h {
	case "codex":
		return "default: the model in ~/.codex/config.toml"
	case "opencode":
		return "default: OpenCode's model (else provider/model)"
	}
	return "default model"
}
