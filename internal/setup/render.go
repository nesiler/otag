package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DocsURL is where the generated files point for every key.
const DocsURL = "https://github.com/nesiler/otag/blob/main/docs/SETUP.md"

const rule = "# ----------------------------------------------------------------"

// quote is a TOML basic string: JSON's string syntax is valid TOML.
func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func quoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// kv is `key = value`, with a comment aligned after it.
func kv(key, value, comment string) string {
	l := key + " = " + value
	if comment == "" {
		return l + "\n"
	}
	return fmt.Sprintf("%-32s # %s\n", l, comment)
}

// TildePath writes p under home as ~/... (home also with its symlinks resolved: git
// reports /private/var/... for a home under /var).
func TildePath(p, home string) string {
	if home == "" || !filepath.IsAbs(p) {
		return p
	}
	homes := []string{home}
	if r, err := filepath.EvalSymlinks(home); err == nil && r != home {
		homes = append(homes, r)
	}
	for _, h := range homes {
		if rel, err := filepath.Rel(h, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return p
}

// RenderRegistry is a new registry.toml with the active accounts and profiles.
func RenderRegistry(b Base, c Choices, now time.Time) string {
	var w strings.Builder
	w.WriteString(`# otag registry: the agent accounts and profiles otag may use.
# Written by ` + "`otag init`" + ` on ` + now.Format("2006-01-02") + `. Edit it freely, then run ` + "`otag sync`" + `.
# Setup and every key: ` + DocsURL + `
# A fuller example (several logins, custom models, env): examples/registry.toml in the repository.
#
# account  a harness login. harness = claude | codex | opencode. Without config_dir the
#          harness's normal login is used (~/.claude, ~/.codex, ~/.config/opencode); a second
#          login gets its own config_dir (see the docs).
# profile  account + model + effort. Projects and tasks name profiles. When an account hits a
#          quota limit (or a launch fails) the daemon retries the task on the profile's
#          fallback list, in order. Tags and description are informational only.

`)
	writeAccountsProfiles(&w, b, c, true)
	return w.String()
}

// RenderAppend is the block appended to an existing registry.toml: the accounts and
// profiles of the harnesses it does not use yet. The user's own text above it is kept
// byte for byte.
func RenderAppend(b Base, c Choices, now time.Time) string {
	var w strings.Builder
	w.WriteString("\n" + rule + " added by `otag init` on " + now.Format("2006-01-02") + "\n")
	w.WriteString("# Setup and every key: " + DocsURL + "\n\n")
	writeAccountsProfiles(&w, b, c, false)
	return w.String()
}

func writeAccountsProfiles(w *strings.Builder, b Base, c Choices, headings bool) {
	if headings {
		w.WriteString(rule + " accounts\n\n")
	}
	for _, a := range c.Accounts {
		if !a.On {
			continue
		}
		t := b.Detect.Harness(a.Harness)
		about := HarnessTitle(a.Harness)
		if t.Version != "" {
			about += " (" + t.Version + ")"
		}
		fmt.Fprintf(w, "# %s. No config_dir: %s.\n", about, defaultLogin(a.Harness))
		fmt.Fprintf(w, "[accounts.%s]\n", a.Name)
		w.WriteString(kv("harness", quote(a.Harness), ""))
		w.WriteString("\n")
	}
	if headings {
		w.WriteString(rule + " profiles\n\n")
	}
	seen := map[string]bool{}
	for _, p := range c.Active() {
		first := !seen[p.Harness]
		seen[p.Harness] = true
		if p.Description != "" {
			fmt.Fprintf(w, "# %s.\n", p.Description)
		}
		fmt.Fprintf(w, "[profiles.%s]\n", p.Name)
		w.WriteString(kv("account", quote(p.Account), ""))
		modelNote, effortNote := "", ""
		if first {
			modelNote, effortNote = modelHelp(p.Harness), effortHelp(p.Harness)
		}
		if p.Model != "" {
			w.WriteString(kv("model", quote(p.Model), modelNote))
		} else if first {
			fmt.Fprintf(w, "# model = %s   # %s\n", quote(exampleModel(p.Harness)), defaultModel(p.Harness))
		}
		if p.Effort != "" {
			w.WriteString(kv("effort", quote(p.Effort), effortNote))
		}
		if len(p.Tags) > 0 {
			w.WriteString(kv("tags", quoteList(p.Tags), ""))
		}
		if len(p.Fallback) > 0 {
			note := ""
			if len(seen) == 1 && first {
				note = "tried in order when this profile cannot run"
			}
			w.WriteString(kv("fallback", quoteList(p.Fallback), note))
		}
		if p.Description != "" {
			w.WriteString(kv("description", quote(p.Description), ""))
		}
		w.WriteString("\n")
	}
}

func defaultLogin(h string) string {
	switch h {
	case "claude":
		return "your normal Claude Code login (~/.claude)"
	case "codex":
		return "your normal Codex login (~/.codex, or $CODEX_HOME)"
	case "opencode":
		return "your normal OpenCode setup (~/.config/opencode).\n# For an isolated account, config_dir is an account ROOT, not the opencode config dir (docs)"
	}
	return "the harness's default"
}

func modelHelp(h string) string {
	switch h {
	case "claude":
		return "a Claude Code alias (opus, sonnet, haiku) or a full model id"
	case "codex":
		return "a Codex model id; unset = the model in ~/.codex/config.toml"
	case "opencode":
		return "provider/model as OpenCode names it"
	}
	return ""
}

func exampleModel(h string) string {
	switch h {
	case "codex":
		return "gpt-5.5"
	case "opencode":
		return "provider/model"
	}
	return ""
}

func defaultModel(h string) string {
	switch h {
	case "codex":
		return "unset: the model in ~/.codex/config.toml"
	case "opencode":
		return "unset: OpenCode's default model; else provider/model"
	}
	return "unset: the harness's default model"
}

func effortHelp(h string) string {
	switch h {
	case "claude":
		return "low | medium | high | xhigh | max"
	case "codex":
		return "model_reasoning_effort: minimal | low | medium | high | xhigh"
	}
	return ""
}

// RenderProject is a projects/<name>.toml.
func RenderProject(p Project, home string, now time.Time) string {
	var w strings.Builder
	fmt.Fprintf(&w, `# otag project %q, written by otag on %s.
# Edit it freely, then run `+"`otag sync`"+`. The file name is the project name.
# Setup and every key: %s

`, p.Name, now.Format("2006-01-02"), DocsURL)
	w.WriteString(kv("path", quote(TildePath(p.Path, home)), "the git checkout; otag never writes into it"))
	w.WriteString(kv("prefix", quote(p.Prefix), "task ids: "+p.Prefix+"-x3f"))
	w.WriteString("\n")
	profile := func(key, name, about string) {
		if name == "" {
			fmt.Fprintf(&w, "# %s = \"\"   # %s (none set)\n", key, about)
			return
		}
		w.WriteString(kv(key, quote(name), about))
	}
	profile("coordinator_profile", p.Coordinator, "the project's planning agent (a Claude profile)")
	profile("default_profile", p.Default, "children run this unless a task names another")
	profile("review_profile", p.Review, "reviews; best a different model family")
	w.WriteString("\n")
	w.WriteString("# worktree: each task gets a git worktree and branch otag/<task>; same: the checkout itself\n")
	w.WriteString(kv("workspace", quote(p.Workspace), ""))
	w.WriteString("# light: push, git on other refs, writes outside the worktree and deny files are refused\n")
	w.WriteString("# strict: the full guard behind the harness's sandbox (docs/ADAPTERS.md \"Threat model\")\n")
	w.WriteString(kv("guard", quote(p.Guard), ""))
	w.WriteString(kv("deny", quoteList(p.Deny), "files the agents must not read or write"))
	return w.String()
}
