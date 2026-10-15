// Package setup is otag's first-run setup: it detects the agent CLIs and tools on the
// machine, proposes accounts, profiles and a project, renders them as commented TOML
// and writes the files atomically. It holds no UI: `otag init` (internal/cli) and the
// setup wizard (internal/tui/wizard) drive it, and every function but the Prober and
// Write is pure, so the choices are unit-testable with injected detection results.
package setup

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nesiler/otag/internal/registry"
)

// Tool is a program found (or not) on PATH.
type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`    // "" = not found
	Version string `json:"version,omitempty"` // the first line of its version output
	// ConfigDir is the harness's default config directory when it exists (display only;
	// the generated account leaves config_dir unset, which means this directory).
	ConfigDir string `json:"config_dir,omitempty"`
}

// Found reports whether the tool is on PATH.
func (t Tool) Found() bool { return t.Path != "" }

// Detection is what the machine has.
type Detection struct {
	Harnesses []Tool `json:"harnesses"` // registry.Harnesses order
	Tmux      Tool   `json:"tmux"`
	Git       Tool   `json:"git"`
	// Repo is the top-level directory of the git repository the current directory is
	// in ("" = none).
	Repo string `json:"repo,omitempty"`
}

// Harness returns the detection result of harness h.
func (d Detection) Harness(h string) Tool {
	for _, t := range d.Harnesses {
		if t.Name == h {
			return t
		}
	}
	return Tool{Name: h}
}

// Found lists the harnesses on PATH, in registry.Harnesses order.
func (d Detection) Found() []string {
	var out []string
	for _, t := range d.Harnesses {
		if t.Found() {
			out = append(out, t.Name)
		}
	}
	return out
}

// Missing lists the required tools (tmux, git) that are not on PATH.
func (d Detection) Missing() []string {
	var out []string
	for _, t := range []Tool{d.Tmux, d.Git} {
		if !t.Found() {
			out = append(out, t.Name)
		}
	}
	return out
}

// HarnessTitle is a harness's product name.
func HarnessTitle(h string) string {
	switch h {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex CLI"
	case "opencode":
		return "OpenCode"
	}
	return h
}

// InstallHint is the command that installs a harness or a required tool.
func InstallHint(name string) string {
	switch name {
	case "claude":
		return "curl -fsSL https://claude.ai/install.sh | bash"
	case "codex":
		return "npm install -g @openai/codex"
	case "opencode":
		return "curl -fsSL https://opencode.ai/install | bash"
	case "tmux":
		return "brew install tmux"
	case "git":
		return "xcode-select --install   # or: brew install git"
	}
	return ""
}

// Prober detects tools. Its fields are the process environment; tests replace Run.
type Prober struct {
	PATH string // the PATH searched (not the process's own)
	Home string
	// CodexHome is $CODEX_HOME ("" = ~/.codex).
	CodexHome string
	// Run runs bin with args and returns its combined output; nil = os/exec with a
	// 5 s timeout.
	Run func(ctx context.Context, bin string, args ...string) (string, error)
}

// Detect looks for the harnesses, tmux and git, and the git repository of cwd. The
// version commands run in parallel.
func (p Prober) Detect(ctx context.Context, cwd string) Detection {
	run := p.Run
	if run == nil {
		run = runCommand
	}
	d := Detection{Tmux: Tool{Name: "tmux"}, Git: Tool{Name: "git"}}
	for _, h := range registry.Harnesses {
		d.Harnesses = append(d.Harnesses, Tool{Name: h})
	}
	type job struct {
		t    *Tool
		args []string
	}
	var jobs []job
	for i := range d.Harnesses {
		jobs = append(jobs, job{&d.Harnesses[i], []string{"--version"}})
	}
	jobs = append(jobs, job{&d.Tmux, []string{"-V"}}, job{&d.Git, []string{"--version"}})
	var wg sync.WaitGroup
	for _, j := range jobs {
		path, ok := LookPath(j.t.Name, p.PATH)
		if !ok {
			continue
		}
		j.t.Path = path
		j.t.ConfigDir = p.configDir(j.t.Name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := run(ctx, path, j.args...)
			if v := firstLine(out); v != "" && err == nil {
				j.t.Version = v
			}
		}()
	}
	wg.Wait()
	if d.Git.Found() {
		d.Repo = p.repo(ctx, d.Git.Path, cwd)
	}
	return d
}

// Repo is the top-level directory of the git repository dir is in ("" = none, or
// git is not on PATH).
func (p Prober) Repo(ctx context.Context, dir string) string {
	bin, ok := LookPath("git", p.PATH)
	if !ok {
		return ""
	}
	return p.repo(ctx, bin, dir)
}

func (p Prober) repo(ctx context.Context, bin, dir string) string {
	if dir == "" {
		return ""
	}
	run := p.Run
	if run == nil {
		run = runCommand
	}
	out, err := run(ctx, bin, "-C", dir, "rev-parse", "--show-toplevel")
	if top := firstLine(out); err == nil && filepath.IsAbs(top) {
		return filepath.Clean(top)
	}
	return ""
}

// configDir is the harness's default config directory if it exists.
func (p Prober) configDir(h string) string {
	if p.Home == "" {
		return ""
	}
	var dir string
	switch h {
	case "claude":
		dir = filepath.Join(p.Home, ".claude")
	case "codex":
		dir = p.CodexHome
		if dir == "" {
			dir = filepath.Join(p.Home, ".codex")
		}
	case "opencode":
		dir = filepath.Join(p.Home, ".config", "opencode")
	default:
		return ""
	}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir
	}
	return ""
}

// LookPath finds an executable named name in the directories of pathEnv (the
// process's own PATH is not consulted, so tests and the CLI's injected environment
// agree).
func LookPath(name, pathEnv string) (string, bool) {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

func runCommand(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			return l
		}
	}
	return ""
}
