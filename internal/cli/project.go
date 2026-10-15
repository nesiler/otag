package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/setup"
	"github.com/nesiler/otag/internal/tui/wizard"
)

func cmdProject(ctx context.Context, a *app, args []string) (result, error) {
	if len(args) == 0 {
		return result{}, usageErr("usage: otag project add [path] [--name N] [--prefix P] [--guard light|strict] [--yes] | list | remove <name>")
	}
	switch args[0] {
	case "add":
		return projectAdd(ctx, a, args[1:])
	case "list", "ls":
		return projectList(ctx, a, args[1:])
	case "remove", "rm":
		return projectRemove(ctx, a, args[1:])
	}
	return result{}, usageErr("unknown project command %q (want add|list|remove)", args[0])
}

// projectAdd registers the git repository of path (default: the current directory):
// projects/<name>.toml, prefilled like the wizard's project page, then sync.
func projectAdd(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("project add")
	var name, prefix, guardLevel, workspace string
	var yes bool
	fs.StringVar(&name, "name", "", "project name (default: the directory name)")
	fs.StringVar(&prefix, "prefix", "", "task id prefix (default: derived from the name)")
	fs.StringVar(&guardLevel, "guard", "", "guard level: light (default) | strict")
	fs.StringVar(&workspace, "workspace", "", "worktree (default) | same")
	fs.BoolVar(&yes, "yes", false, "do not ask")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	switch {
	case len(pos) > 1:
		return result{}, usageErr("usage: otag project add [path] [--name N] [--prefix P] [--guard light|strict] [--yes]")
	case a.json && !yes:
		return result{}, usageErr("otag project add --json needs --yes")
	case !yes && !a.terminal():
		return result{}, usageErr("otag project add asks for confirmation: run it in a terminal, or pass --yes")
	}
	dir := a.env.Cwd
	if len(pos) == 1 {
		dir = paths.ExpandHome(pos[0], a.env.Getenv)
		if !filepath.IsAbs(dir) {
			if a.env.Cwd == "" {
				return result{}, usageErr("a relative path needs a working directory: pass an absolute path")
			}
			dir = filepath.Join(a.env.Cwd, dir)
		}
	}
	if dir == "" {
		return result{}, usageErr("no directory: pass the repository's path")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return result{}, apperr.New(apperr.Invalid, "%s is not a directory", dir)
	}
	b, err := a.setupBase(ctx, false, false)
	if err != nil {
		return result{}, err
	}
	if !b.RegistryExists {
		return result{}, errNotConfigured(b.ConfigDir)
	}
	repo := a.prober().Repo(ctx, dir)
	if repo == "" {
		return result{}, apperr.New(apperr.Invalid, "%s is not inside a git repository (otag runs tasks in git worktrees; `git init` it first)", dir)
	}
	for _, p := range b.Existing.Projects {
		if setup.SamePath(p.Path, repo) {
			return result{}, apperr.New(apperr.Conflict, "%s is already registered as project %s (%s)", repo, p.Name, p.File)
		}
	}
	p := b.NewProject(repo, setup.Choices{})
	if name != "" {
		p.Name = name
		if prefix == "" {
			p.Prefix = b.UniquePrefix(name)
		}
	}
	if prefix != "" {
		p.Prefix = prefix
	}
	if guardLevel != "" {
		p.Guard = guardLevel
	}
	if workspace != "" {
		p.Workspace = workspace
	}
	c := setup.Choices{Project: &p}
	if yes {
		if err := b.CheckProject(p, c); err != nil {
			return result{}, err
		}
		d, err := a.applySetup(ctx, b, c)
		return projectAdded(a, b, d), err
	}
	var last initData
	res, err := a.runWizard(ctx, wizard.Options{Mode: wizard.Project, Base: b, Choices: c, Preview: a.preview(b),
		Apply: a.applier(b, &last)})
	switch {
	case err != nil:
		return result{}, err
	case res.Err != "":
		return projectAdded(a, b, last), apperr.New(apperr.Unavailable, "%s", res.Err)
	case !res.Applied:
		return result{data: map[string]any{"cancelled": true}, human: func(w io.Writer) {
			_, _ = fmt.Fprintln(w, "cancelled; nothing was written")
		}}, nil
	}
	return projectAdded(a, b, last), nil
}

func projectAdded(a *app, b setup.Base, d initData) result {
	return result{data: d, human: func(w io.Writer) {
		if p := d.Project; p != nil && d.Sync != nil {
			_, _ = fmt.Fprintf(w, "added project %s (prefix %s) at %s\n", p.Name, p.Prefix, setup.TildePath(p.Path, b.Home))
		}
		for _, l := range d.lines(b.Home) {
			_, _ = fmt.Fprintln(w, "  "+l)
		}
		for _, warn := range d.Warnings {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", warn)
		}
		if p := d.Project; p != nil && d.Sync != nil {
			_, _ = fmt.Fprintf(w, "next: otag coord %s --attach   (or open `otag tui` and press c on the project)\n", p.Name)
		}
	}}
}

// projectInfo is a row of `otag project list`.
type projectInfo struct {
	Name               string   `json:"name"`
	Path               string   `json:"path"`
	Prefix             string   `json:"prefix"`
	Workspace          string   `json:"workspace"`
	Guard              string   `json:"guard"`
	CoordinatorProfile string   `json:"coordinator_profile,omitempty"`
	DefaultProfile     string   `json:"default_profile,omitempty"`
	ReviewProfile      string   `json:"review_profile,omitempty"`
	Deny               []string `json:"deny"`
	File               string   `json:"file"`
}

func projectList(_ context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("project list")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		if err == nil {
			err = usageErr("usage: otag project list")
		}
		return result{}, err
	}
	cfg, err := a.loadRegistry()
	if err != nil {
		return result{}, err
	}
	out := []projectInfo{}
	for _, p := range cfg.Projects {
		deny := p.Deny
		if deny == nil {
			deny = []string{}
		}
		out = append(out, projectInfo{Name: p.Name, Path: p.Path, Prefix: p.Prefix, Workspace: p.Workspace, Guard: p.Guard,
			CoordinatorProfile: p.CoordinatorProfile, DefaultProfile: p.DefaultProfile, ReviewProfile: p.ReviewProfile,
			Deny: deny, File: p.File})
	}
	return result{data: out, human: func(w io.Writer) {
		if len(out) == 0 {
			_, _ = fmt.Fprintln(w, "no projects: run `otag project add` in a git repository")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "PROJECT\tPREFIX\tGUARD\tWORKSPACE\tCOORDINATOR\tDEFAULT\tREVIEW\tPATH")
		for _, p := range out {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Prefix, p.Guard, p.Workspace,
				dash(p.CoordinatorProfile), dash(p.DefaultProfile), dash(p.ReviewProfile), setup.TildePath(p.Path, a.home()))
		}
		_ = tw.Flush()
	}}, nil
}

// loadRegistry loads the TOML registry; a missing one says to run otag init.
func (a *app) loadRegistry() (*registry.Config, error) {
	if !a.configured() {
		return nil, errNotConfigured(a.configDir())
	}
	return registry.Load(a.configDir(), a.env.Getenv)
}

// projectRemove unregisters a project: its TOML file is renamed (kept as a backup,
// no longer loaded) and sync retires the project. Sync never deletes a row, so its
// tasks, runs, messages and events stay, and no worktree or branch is touched;
// `otag project add` brings it back. A project with runs that have not ended is
// refused.
func projectRemove(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("project remove")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag project remove <name>")
	}
	cfg, err := a.loadRegistry()
	if err != nil {
		return result{}, err
	}
	var proj *registry.Project
	for i := range cfg.Projects {
		if cfg.Projects[i].Name == pos[0] {
			proj = &cfg.Projects[i]
		}
	}
	if proj == nil {
		return result{}, apperr.New(apperr.NotFound, "no project %s in %s", pos[0], filepath.Join(a.configDir(), "projects"))
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	var live int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM runs r JOIN projects p ON p.id = r.project_id
		WHERE p.name = ? AND r.status <> 'ended'`, proj.Name).Scan(&live); err != nil {
		return result{}, err
	}
	if live > 0 {
		return result{}, apperr.New(apperr.Conflict, "project %s has %d run(s) that have not ended: stop them first (`otag run list --project %s`)",
			proj.Name, live, proj.Name)
	}
	kept := proj.File + ".removed-" + time.Now().Format("20060102-150405")
	if err := os.Rename(proj.File, kept); err != nil {
		return result{}, err
	}
	res, err := cmdSync(ctx, a, nil)
	if err != nil {
		// Put the file back: the registry stays as it was.
		if rerr := os.Rename(kept, proj.File); rerr != nil {
			return result{}, fmt.Errorf("%w (and restoring %s failed: %v)", err, proj.File, rerr)
		}
		return result{}, err
	}
	sr := res.data.(registry.SyncResult)
	data := map[string]any{"project": proj.Name, "file": kept, "sync": sr}
	return result{data: data, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "removed project %s; its file is kept as %s\n", proj.Name, setup.TildePath(kept, a.home()))
		_, _ = fmt.Fprintln(w, "its tasks, runs and history stay in the database, worktrees and branches stay on disk;")
		_, _ = fmt.Fprintf(w, "add it back with `otag project add %s`\n", setup.TildePath(proj.Path, a.home()))
		for _, warn := range sr.Warnings {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", strings.TrimSpace(warn))
		}
	}}, nil
}
