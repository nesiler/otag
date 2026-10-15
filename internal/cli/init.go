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

// uiHooks replace the interactive programs in tests (nil fields: the real ones).
type uiHooks struct {
	wizard func(ctx context.Context, o wizard.Options) (wizard.Result, error)
	tui    func(ctx context.Context, a *app, args []string) (result, error)
}

func (a *app) runWizard(ctx context.Context, o wizard.Options) (wizard.Result, error) {
	if a.env.ui != nil && a.env.ui.wizard != nil {
		return a.env.ui.wizard(ctx, o)
	}
	return wizard.Run(ctx, o)
}

func (a *app) runTUI(ctx context.Context, args []string) (result, error) {
	if a.env.ui != nil && a.env.ui.tui != nil {
		return a.env.ui.tui(ctx, a, args)
	}
	return cmdTui(ctx, a, args)
}

// terminal reports whether stdin and stdout are a terminal.
func (a *app) terminal() bool {
	if a.env.Terminal != nil {
		return a.env.Terminal()
	}
	return isTerminal(a.env.Stdin) && isTerminal(a.env.Stdout)
}

// pathEnv is the PATH that tools are looked up in: the environment's (injected in
// tests), else the process's.
func (a *app) pathEnv() string {
	if p := a.env.Getenv("PATH"); p != "" {
		return p
	}
	return os.Getenv("PATH")
}

func (a *app) home() string {
	if h := a.env.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

func (a *app) prober() setup.Prober {
	return setup.Prober{PATH: a.pathEnv(), Home: a.home(), CodexHome: a.env.Getenv("CODEX_HOME")}
}

// configured reports whether registry.toml exists.
func (a *app) configured() bool {
	_, err := os.Stat(setup.RegistryFile(a.configDir()))
	return err == nil
}

// setupBase gathers what setup starts from. It writes nothing: the database is read
// only when it already exists, and the daemon and skill are checked by their files.
func (a *app) setupBase(ctx context.Context, force bool, detect bool) (setup.Base, error) {
	b := setup.Base{ConfigDir: a.configDir(), Home: a.home(), Force: force}
	if detect {
		b.Detect = a.prober().Detect(ctx, a.env.Cwd)
		// A task's worktree is otag's, never a project.
		if paths.Within(b.Detect.Repo, paths.WorktreeRoot(a.env.Getenv)) {
			b.Detect.Repo = ""
		}
	}
	cfg, exists, err := setup.LoadExisting(b.ConfigDir, a.env.Getenv)
	b.RegistryExists = exists
	switch {
	case exists && force:
		// The registry is replaced; the project files stay and must still load with it.
		projects, perr := registry.LoadProjects(b.ConfigDir, a.env.Getenv)
		if perr != nil {
			return b, perr
		}
		b.Existing = &registry.Config{Dir: b.ConfigDir, Projects: projects}
	case err != nil:
		msg := err.Error()
		if e, ok := apperr.As(err); ok {
			msg = e.Message
		}
		return b, apperr.New(apperr.ConfigInvalid, "%s\nfix it, or start over with `otag init --force` (the old registry.toml is backed up)", msg)
	default:
		b.Existing = cfg
	}
	if _, err := os.Stat(a.dbPath()); err == nil {
		d, err := a.openDB(ctx)
		if err != nil {
			return b, err
		}
		rows, err := d.QueryContext(ctx, `SELECT name, prefix FROM projects`)
		if err != nil {
			return b, err
		}
		b.Known = map[string]string{}
		for rows.Next() {
			var name, prefix string
			if err := rows.Scan(&name, &prefix); err != nil {
				_ = rows.Close()
				return b, err
			}
			b.Known[name] = prefix
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return b, err
		}
	}
	if _, err := os.Stat(a.launchd().PlistFile()); err == nil {
		b.DaemonInstalled = true
	}
	if st, err := skillPathsIn(b.Home).fileState(); err == nil && st != "missing" {
		b.SkillInstalled = true
	}
	return b, nil
}

// initData is the result of `otag init` and `otag project add`.
type initData struct {
	ConfigDir string               `json:"config_dir"`
	Files     []setup.File         `json:"files"`
	Sync      *registry.SyncResult `json:"sync,omitempty"`
	Project   *setup.Project       `json:"project,omitempty"`
	Profiles  []string             `json:"profiles"`
	Daemon    map[string]any       `json:"daemon,omitempty"`
	Skill     []skillState         `json:"skill,omitempty"`
	Warnings  []string             `json:"warnings"`
	// Nothing is true when there was nothing to add (`otag init` on a set-up machine).
	Nothing bool `json:"nothing_to_do,omitempty"`
}

// lines describe what was done, for the human output and the wizard's last page.
func (d initData) lines(home string) []string {
	var out []string
	for _, f := range d.Files {
		verb := map[string]string{setup.Create: "wrote", setup.Append: "added to", setup.Replace: "replaced"}[f.Action]
		l := verb + " " + setup.TildePath(f.Path, home)
		if f.Backup != "" {
			l += " (old file: " + setup.TildePath(f.Backup, home) + ")"
		}
		out = append(out, l)
	}
	if s := d.Sync; s != nil {
		var parts []string
		for _, c := range []struct {
			what string
			c    registry.Counts
		}{{"account", s.Accounts}, {"profile", s.Profiles}, {"project", s.Projects}} {
			for _, n := range []struct {
				n    int
				verb string
			}{{c.c.Added, "added"}, {c.c.Updated, "updated"}} {
				if n.n > 0 {
					parts = append(parts, fmt.Sprintf("%d %s%s %s", n.n, c.what, map[bool]string{true: "s"}[n.n > 1], n.verb))
				}
			}
		}
		if len(parts) == 0 {
			parts = []string{"no changes"}
		}
		out = append(out, "synced the database: "+strings.Join(parts, ", "))
	}
	if d.Daemon != nil {
		out = append(out, fmt.Sprintf("installed the daemon (%v)", setup.TildePath(fmt.Sprint(d.Daemon["plist"]), home)))
	}
	if d.Skill != nil {
		out = append(out, "installed the agent skill ("+setup.TildePath(d.Skill[0].Path, home)+")")
	}
	return out
}

// applySetup writes what c asks for and runs sync, then the daemon and skill
// installs: the same code as `otag sync`, `otag daemon install` and `otag skill
// install`. Nothing is written when the result would not load. A daemon or skill
// failure does not undo the files; it is the returned error.
func (a *app) applySetup(ctx context.Context, b setup.Base, c setup.Choices) (initData, error) {
	data := initData{ConfigDir: b.ConfigDir, Files: []setup.File{}, Profiles: []string{}, Warnings: []string{}}
	for _, p := range c.Active() {
		data.Profiles = append(data.Profiles, p.Name)
	}
	data.Project = c.Project
	files, err := b.Files(c, time.Now())
	if err != nil {
		return data, err
	}
	if _, err := setup.Preflight(b.ConfigDir, files, a.env.Getenv); err != nil {
		if b.Force {
			return data, fmt.Errorf("%w\n(--force replaces registry.toml; edit or remove the project files that name its old profiles)", err)
		}
		return data, err
	}
	if err := setup.Write(files); err != nil {
		return data, err
	}
	data.Files = files
	res, err := cmdSync(ctx, a, nil)
	if err != nil {
		return data, err
	}
	sr := res.data.(registry.SyncResult)
	data.Sync = &sr
	data.Warnings = append(data.Warnings, sr.Warnings...)
	var failed []string
	if c.Daemon {
		if res, err := daemonInstall(ctx, a, nil); err != nil {
			failed = append(failed, "daemon install: "+err.Error()+" (retry with `otag daemon install`)")
		} else {
			data.Daemon = res.data.(map[string]any)
		}
	}
	if c.Skill {
		if st, err := skillInstall(skillPathsIn(b.Home), false); err != nil {
			failed = append(failed, "skill install: "+err.Error())
		} else {
			data.Skill = st
		}
	}
	if len(failed) > 0 {
		return data, apperr.New(apperr.Unavailable, "the config is written and synced, but %s", strings.Join(failed, "; "))
	}
	return data, nil
}

// preview is the wizard's Review page for c.
func (a *app) preview(b setup.Base) func(c setup.Choices) wizard.Preview {
	return func(c setup.Choices) wizard.Preview {
		files, err := b.Files(c, time.Now())
		if err != nil {
			return wizard.Preview{Err: errText(err)}
		}
		cfg, err := setup.Preflight(b.ConfigDir, files, a.env.Getenv)
		if err != nil {
			return wizard.Preview{Files: files, Err: errText(err)}
		}
		return wizard.Preview{Files: files, Warnings: cfg.Warnings}
	}
}

// applier adapts applySetup to the wizard.
func (a *app) applier(b setup.Base, last *initData) func(ctx context.Context, c setup.Choices) (wizard.Report, error) {
	return func(ctx context.Context, c setup.Choices) (wizard.Report, error) {
		d, err := a.applySetup(ctx, b, c)
		*last = d
		return wizard.Report{Lines: d.lines(b.Home), Warnings: d.Warnings}, err
	}
}

func cmdInit(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("init")
	var yes, noDaemon, force bool
	fs.BoolVar(&yes, "yes", false, "accept the detected defaults without asking")
	fs.BoolVar(&noDaemon, "no-daemon", false, "do not install the background service")
	fs.BoolVar(&force, "force", false, "replace an existing registry.toml (backed up first)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	switch {
	case len(pos) > 0:
		return result{}, usageErr("usage: otag init [--yes] [--no-daemon] [--force]")
	case a.json && !yes:
		return result{}, usageErr("otag init --json needs --yes (the wizard is interactive)")
	case !yes && !a.terminal():
		return result{}, usageErr("otag init is interactive: run it in a terminal, or pass --yes to accept the detected defaults")
	}
	b, err := a.setupBase(ctx, force, true)
	if err != nil {
		return result{}, err
	}
	if noDaemon {
		b.DaemonInstalled = true // not offered
	}
	c := b.Defaults()
	if noDaemon {
		c.Daemon = false
	}
	if yes {
		if miss := b.Detect.Missing(); len(miss) > 0 {
			var hints []string
			for _, t := range miss {
				hints = append(hints, setup.InstallHint(t))
			}
			return result{}, apperr.New(apperr.Unavailable, "otag needs %s: install with `%s`, then run otag init again",
				strings.Join(miss, " and "), strings.Join(hints, "` and `"))
		}
		if (b.Existing == nil || len(b.Existing.Accounts) == 0) && len(b.Detect.Found()) == 0 {
			return result{}, apperr.New(apperr.Unavailable, "no agent CLI on PATH (claude, codex, opencode); install one, e.g. `%s`, then run otag init again",
				setup.InstallHint("claude"))
		}
	}
	if b.RegistryExists && !force && len(c.Accounts) == 0 && c.Project == nil && !c.Daemon && (yes || b.SkillInstalled) {
		return initNothing(ctx, a, b)
	}
	if yes {
		d, err := a.applySetup(ctx, b, c)
		return initResult(a, b, d, true), err
	}
	var last initData
	res, err := a.runWizard(ctx, wizard.Options{Mode: wizard.Init, Base: b, Choices: c, Preview: a.preview(b),
		Apply: a.applier(b, &last), OfferTUI: true})
	if err != nil {
		return result{}, err
	}
	switch {
	case res.Err != "":
		return initResult(a, b, last, false), apperr.New(apperr.Unavailable, "%s", res.Err)
	case !res.Applied:
		return result{data: map[string]any{"cancelled": true}, human: func(w io.Writer) {
			_, _ = fmt.Fprintln(w, "setup cancelled; nothing was written")
		}}, nil
	case res.OpenTUI:
		return a.runTUI(ctx, nil)
	}
	return initResult(a, b, last, true), nil
}

// initNothing is `otag init` with nothing to add: it syncs (a no-op unless the TOML
// was edited) and says so.
func initNothing(ctx context.Context, a *app, b setup.Base) (result, error) {
	d := initData{ConfigDir: b.ConfigDir, Files: []setup.File{}, Profiles: []string{}, Warnings: []string{}, Nothing: true}
	res, err := cmdSync(ctx, a, nil)
	if err != nil {
		return result{}, err
	}
	sr := res.data.(registry.SyncResult)
	d.Sync = &sr
	d.Warnings = append(d.Warnings, sr.Warnings...)
	return result{data: d, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "otag is already set up (%s): nothing to add.\n", setup.TildePath(b.ConfigDir, b.Home))
		_, _ = fmt.Fprintln(w, "  otag project add      register another git repository")
		_, _ = fmt.Fprintln(w, "  otag init --force     start over (the old registry.toml is backed up)")
		_, _ = fmt.Fprintln(w, "  otag tui              open the terminal UI")
	}}, nil
}

// initResult renders what applySetup did; next adds the next steps.
func initResult(a *app, b setup.Base, d initData, next bool) result {
	return result{data: d, human: func(w io.Writer) {
		for _, l := range d.lines(b.Home) {
			_, _ = fmt.Fprintln(w, l)
		}
		for _, warn := range d.Warnings {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", warn)
		}
		if !next {
			return
		}
		_, _ = fmt.Fprintln(w, "\nNext:")
		tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
		if p := d.Project; p != nil {
			_, _ = fmt.Fprintf(tw, "  otag coord %s --attach\tstart the project's coordinator\n", p.Name)
		} else {
			_, _ = fmt.Fprintln(tw, "  otag project add\tregister a git repository (run it inside one)")
		}
		_, _ = fmt.Fprintln(tw, "  otag tui\topen the terminal UI (later, plain `otag` too)")
		_, _ = fmt.Fprintln(tw, "  otag doctor\tcheck the setup")
		_ = tw.Flush()
	}}
}

// errNotConfigured is the error of commands that need a registry.
func errNotConfigured(dir string) error {
	return apperr.New(apperr.ConfigInvalid, "otag is not set up yet (%s does not exist): run `otag init`",
		filepath.Join(dir, "registry.toml"))
}

// errText is an error's message without the code.
func errText(err error) string {
	if e, ok := apperr.As(err); ok {
		return e.Message
	}
	return err.Error()
}
