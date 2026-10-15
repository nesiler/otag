package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/daemon"
	"github.com/nesiler/otag/internal/identity"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/quota"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

func cmdSync(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("sync")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "validate and show changes without writing")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	cfg, err := registry.Load(a.configDir(), a.env.Getenv)
	if err != nil {
		return result{}, err
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	actor := a.actorName(identity.Identity{RunID: a.run})
	var res registry.SyncResult
	if dryRun {
		// Run the real sync and roll it back.
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return result{}, err
		}
		res, err = registry.Sync(ctx, tx, cfg, actor)
		_ = tx.Rollback()
		if err != nil {
			return result{}, err
		}
	} else {
		key := opKey{Command: "sync", Args: []string{a.configDir()}}
		res, err = mutate(ctx, a, key, func(tx *sql.Tx) (registry.SyncResult, error) {
			return registry.Sync(ctx, tx, cfg, actor)
		})
		if err != nil {
			return result{}, err
		}
	}
	return result{data: res, human: func(w io.Writer) {
		for _, warn := range res.Warnings {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", warn)
		}
		verb := "synced"
		if dryRun {
			verb = "would sync"
		}
		_, _ = fmt.Fprintf(w, "%s %s\n", verb, a.configDir())
		for _, c := range []struct {
			name string
			c    registry.Counts
		}{{"accounts", res.Accounts}, {"profiles", res.Profiles}, {"projects", res.Projects}} {
			_, _ = fmt.Fprintf(w, "  %-9s +%d ~%d =%d -%d\n", c.name, c.c.Added, c.c.Updated, c.c.Unchanged, c.c.Retired)
		}
		_, _ = fmt.Fprintf(w, "  fallback lists changed: %d\n", res.Fallbacks)
	}}, nil
}

func cmdProfiles(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("profiles")
	var tag string
	fs.StringVar(&tag, "tag", "", "only profiles with this tag")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	ps, err := registry.ListProfiles(ctx, d, tag)
	if err != nil {
		return result{}, err
	}
	return result{data: ps, human: func(w io.Writer) {
		if len(ps) == 0 {
			_, _ = fmt.Fprintln(w, "no profiles (run `otag sync`?)")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "PROFILE\tHARNESS\tACCOUNT\tMODEL\tEFFORT\tMODE\tTAGS\tFALLBACK")
		for _, p := range ps {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Harness, p.Account, p.Model, p.Effort,
				p.Mode, strings.Join(p.Tags, ","), strings.Join(p.Fallback, " > "))
		}
		_ = tw.Flush()
	}}, nil
}

type projectStatus struct {
	Project string         `json:"project"`
	Guard   string         `json:"guard"` // guard level of new runs: light | strict
	Counts  map[string]int `json:"counts"`
	Ready   int            `json:"ready_unblocked"`
}

type statusData struct {
	Projects []projectStatus `json:"projects"`
	Open     []task.Task     `json:"open_tasks"`
	Runs     []run.Run       `json:"runs"` // runs that have not ended, with a live pane check
	Dead     int             `json:"dead_runs"`
	Daemon   daemonState     `json:"daemon"`
	Unread   int             `json:"unread_notifications"`
	// Quota is the latest snapshot per account and window (last 7 days) and the
	// active cooldowns; global, not filtered by project.
	Quota     []quota.Row      `json:"quota"`
	Cooldowns []quota.Cooldown `json:"cooldowns"`
}

type daemonState struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

func cmdStatus(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("status")
	var all bool
	fs.BoolVar(&all, "all", false, "all projects even inside a project directory")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	var projectID int64
	if id.Project != nil && !all {
		projectID = id.Project.ID
	}
	rows, err := a.db.QueryContext(ctx, `
SELECT p.name, p.guard, t.status, count(t.id),
       (SELECT count(*) FROM ready_tasks r WHERE r.project_id = p.id)
FROM projects p LEFT JOIN tasks t ON t.project_id = p.id
WHERE p.retired_at IS NULL AND (? = 0 OR p.id = ?)
GROUP BY p.id, t.status ORDER BY p.name`, projectID, projectID)
	if err != nil {
		return result{}, err
	}
	data := statusData{Projects: []projectStatus{}}
	for rows.Next() {
		var name, level string
		var status sql.NullString
		var n, ready int
		if err := rows.Scan(&name, &level, &status, &n, &ready); err != nil {
			_ = rows.Close()
			return result{}, err
		}
		if len(data.Projects) == 0 || data.Projects[len(data.Projects)-1].Project != name {
			counts := map[string]int{}
			for _, s := range task.Statuses {
				counts[s] = 0
			}
			data.Projects = append(data.Projects, projectStatus{Project: name, Guard: level, Counts: counts, Ready: ready})
		}
		if status.Valid {
			data.Projects[len(data.Projects)-1].Counts[status.String] = n
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return result{}, err
	}
	if data.Open, err = task.List(ctx, a.db, task.Filter{ProjectID: projectID, Open: true}); err != nil {
		return result{}, err
	}
	if data.Runs, err = run.List(ctx, a.db, run.Filter{ProjectID: projectID, Active: true}); err != nil {
		return result{}, err
	}
	checkLive(ctx, a, data.Runs)
	now := time.Now().UTC()
	if data.Quota, err = quota.List(ctx, a.db, quota.Filter{MaxAge: 7 * 24 * time.Hour}, now); err != nil {
		return result{}, err
	}
	if data.Cooldowns, err = quota.Cooldowns(ctx, a.db, now); err != nil {
		return result{}, err
	}
	for i := range data.Runs {
		if data.Runs[i].Dead() {
			data.Dead++
		}
	}
	data.Daemon.Running, data.Daemon.PID, _ = daemon.Probe(paths.DataDir(a.env.Getenv))
	if data.Unread, err = notify.Unread(ctx, a.db); err != nil {
		return result{}, err
	}
	return result{data: data, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "daemon: %s; %d unread notification(s)", daemonText(data.Daemon.Running, data.Daemon.PID), data.Unread)
		if data.Unread > 0 {
			_, _ = fmt.Fprint(w, " (otag notify list)")
		}
		_, _ = fmt.Fprint(w, "\n\n")
		if len(data.Projects) == 0 {
			_, _ = fmt.Fprintln(w, "no projects (run `otag sync`?)")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprint(tw, "PROJECT\tGUARD")
		for _, s := range task.Statuses {
			_, _ = fmt.Fprintf(tw, "\t%s", strings.ToUpper(s))
		}
		_, _ = fmt.Fprintln(tw, "\tUNBLOCKED")
		for _, p := range data.Projects {
			_, _ = fmt.Fprintf(tw, "%s\t%s", p.Project, p.Guard)
			for _, s := range task.Statuses {
				_, _ = fmt.Fprintf(tw, "\t%d", p.Counts[s])
			}
			_, _ = fmt.Fprintf(tw, "\t%d\n", p.Ready)
		}
		_ = tw.Flush()
		if len(data.Open) > 0 {
			_, _ = fmt.Fprintln(w)
			printTasks(w, data.Open, projectID == 0)
		}
		if len(data.Runs) > 0 {
			_, _ = fmt.Fprintln(w)
			printRuns(w, data.Runs, projectID == 0)
			if data.Dead > 0 {
				if data.Daemon.Running {
					_, _ = fmt.Fprintf(w, "\n%d run(s) lost their pane; the daemon ends them and requeues the task\n", data.Dead)
				} else {
					_, _ = fmt.Fprintf(w, "\n%d run(s) lost their pane; `otag run stop <run>` ends them and requeues the task\n", data.Dead)
				}
			}
		}
		if lines := quotaSummary(data.Quota, now); len(lines) > 0 || len(data.Cooldowns) > 0 {
			_, _ = fmt.Fprintln(w, "\nQUOTA")
			for _, l := range lines {
				_, _ = fmt.Fprintln(w, "  "+l)
			}
			printQuota(w, nil, data.Cooldowns, now)
		}
	}}, nil
}

type contextData struct {
	identity.Identity
	Actor         string `json:"actor"`
	DBPath        string `json:"db_path"`
	ConfigDir     string `json:"config_dir"`
	SchemaVersion int    `json:"schema_version"`
	Cwd           string `json:"cwd"`
}

func cmdContext(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("context")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	data := contextData{Identity: id, Actor: a.actorName(id), DBPath: a.dbPath(), ConfigDir: a.configDir(),
		SchemaVersion: a.db.Version, Cwd: a.env.Cwd}
	return result{data: data, human: func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		if p := id.Project; p != nil {
			_, _ = fmt.Fprintf(tw, "project:\t%s (via %s)\n", p.Name, id.ProjectSource)
			_, _ = fmt.Fprintf(tw, "path:\t%s\n", p.Path)
			_, _ = fmt.Fprintf(tw, "prefix:\t%s\n", p.Prefix)
			_, _ = fmt.Fprintf(tw, "profiles:\tcoordinator=%s default=%s review=%s\n",
				p.CoordinatorProfile, p.DefaultProfile, p.ReviewProfile)
			_, _ = fmt.Fprintf(tw, "workspace:\t%s\n", p.WorkspaceMode)
			_, _ = fmt.Fprintf(tw, "guard:\t%s\n", p.Guard)
		} else {
			_, _ = fmt.Fprintf(tw, "project:\t(none)\n")
		}
		if id.RunID != "" {
			_, _ = fmt.Fprintf(tw, "run:\t%s (via %s)\n", id.RunID, id.RunSource)
		}
		_, _ = fmt.Fprintf(tw, "actor:\t%s\n", data.Actor)
		_, _ = fmt.Fprintf(tw, "db:\t%s (schema v%d)\n", data.DBPath, data.SchemaVersion)
		_, _ = fmt.Fprintf(tw, "config:\t%s\n", data.ConfigDir)
		_ = tw.Flush()
	}}, nil
}

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

func cmdDoctor(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("doctor")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	var checks []check
	add := func(name, status, format string, args ...any) {
		checks = append(checks, check{name, status, fmt.Sprintf(format, args...)})
	}

	cfg, err := registry.Load(a.configDir(), a.env.Getenv)
	if err != nil {
		add("registry", "fail", "%v", err)
	} else {
		add("registry", "ok", "%s: %d accounts, %d profiles, %d projects",
			a.configDir(), len(cfg.Accounts), len(cfg.Profiles), len(cfg.Projects))
		for _, w := range cfg.Warnings {
			add("registry", "warn", "%s", w)
		}
	}

	d, err := a.openDB(ctx)
	switch {
	case err != nil:
		add("database", "fail", "%v", err)
	case d.ReadOnly():
		add("database", "fail", "%s: schema v%d is newer than this binary (v%d); writes refused", d.Path, d.Version, d.Latest)
	default:
		var mode string
		var fk int
		_ = d.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
		_ = d.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
		var integrity string
		_ = d.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity)
		status := "ok"
		if mode != "wal" || fk != 1 || integrity != "ok" {
			status = "fail"
		}
		add("database", status, "%s: schema v%d, journal=%s, foreign_keys=%d, quick_check=%s",
			d.Path, d.Version, mode, fk, integrity)
		// The file is forced to 0600; an existing directory is never chmod-ed, only reported.
		if st, err := os.Stat(filepath.Dir(d.Path)); err == nil && st.Mode().Perm()&0o077 != 0 {
			add("database", "warn", "directory %s is accessible by other users (mode %v)",
				filepath.Dir(d.Path), st.Mode().Perm())
		}
	}
	if cfg != nil && d != nil && !d.ReadOnly() {
		checks = append(checks, syncCheck(ctx, a, cfg))
	}
	if cfg != nil && len(cfg.Projects) > 0 {
		// The guard level of each project's new runs (docs/ADAPTERS.md, "Threat model").
		var levels []string
		for _, p := range cfg.Projects {
			levels = append(levels, p.Name+"="+p.Guard)
		}
		add("guard", "ok", "%s", strings.Join(levels, " "))
	}

	used := map[string]bool{}
	if cfg != nil {
		for _, acc := range cfg.Accounts {
			used[acc.Harness] = true
		}
	}
	for _, h := range registry.Harnesses {
		path, err := exec.LookPath(h)
		if err != nil {
			status := "warn"
			if used[h] {
				status = "fail"
			}
			add("harness "+h, status, "not found on PATH")
			continue
		}
		add("harness "+h, "ok", "%s %s", path, version(ctx, path))
	}
	if cfg != nil {
		for _, name := range sortedNames(cfg.Accounts) {
			acc := cfg.Accounts[name]
			if acc.ConfigDir != "" {
				if st, err := os.Stat(acc.ConfigDir); err != nil || !st.IsDir() {
					add("account "+name, "warn", "config_dir %s does not exist", acc.ConfigDir)
				}
			}
			if acc.Command != "" {
				if _, err := exec.LookPath(acc.Command); err != nil {
					add("account "+name, "warn", "command %q not on PATH (shell functions are not visible to otag)", acc.Command)
				}
			}
		}
	}

	cfgFile := daemon.ConfigFile(a.configDir())
	if _, warns, err := daemon.LoadConfig(cfgFile); err != nil {
		add("daemon config", "fail", "%v", err)
	} else {
		for _, w := range warns {
			add("daemon config", "warn", "%s", w)
		}
	}
	if running, pid, err := daemon.Probe(paths.DataDir(a.env.Getenv)); err != nil {
		add("daemon", "warn", "%v", err)
	} else if running {
		add("daemon", "ok", "running (pid %d)", pid)
	} else {
		add("daemon", "warn", "not running: no crash reaping, stall watchdog or fallback (`otag daemon install`)")
	}

	// The launch agent's environment is all the daemon (and a tmux server it starts,
	// hence every run it launches) has: an OpenCode {env:VAR} it lacks stays unresolved.
	if plist := a.launchd().PlistFile(); cfg != nil {
		if env, err := daemon.ReadPlistEnv(plist); err == nil {
			home := paths.ExpandHome("~", a.env.Getenv)
			for _, name := range sortedNames(cfg.Accounts) {
				acc := cfg.Accounts[name]
				if acc.Harness != "opencode" {
					continue
				}
				path := quota.OpenCodeConfigPath(acc.ConfigDir, home)
				for _, v := range quota.EnvRefs(path) {
					if env[v] == "" {
						add("daemon env", "warn", "%s (account %s) uses {env:%s}, which the launch agent does not set: "+
							"add it to [daemon] env_passthrough in %s and re-run `otag daemon install`", path, name, v, cfgFile)
					}
				}
			}
		}
	}

	// Runs start with a clean environment: an OpenCode {env:VAR} reaches a run only
	// through the profile's env_passthrough or the account/profile env.
	if cfg != nil {
		home := paths.ExpandHome("~", a.env.Getenv)
		for _, name := range sortedNames(cfg.Profiles) {
			p := cfg.Profiles[name]
			acc := cfg.Accounts[p.Account]
			if acc.Harness != "opencode" {
				continue
			}
			path := quota.OpenCodeConfigPath(acc.ConfigDir, home)
			for _, v := range quota.EnvRefs(path) {
				if p.Env[v] != "" || acc.Env[v] != "" || slices.ContainsFunc(p.EnvPassthrough, func(pat string) bool {
					prefix, star := strings.CutSuffix(pat, "*")
					return pat == v || star && strings.HasPrefix(v, prefix)
				}) {
					continue
				}
				add("profile "+name, "warn", "%s uses {env:%s}, which runs do not get (they start with a clean environment): "+
					"add %q to the profile's env_passthrough", path, v, v)
			}
		}
	}

	failed := slices.ContainsFunc(checks, func(c check) bool { return c.Status == "fail" })
	res := result{data: checks, human: func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, c := range checks {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
		}
		_ = tw.Flush()
	}}
	if failed {
		return res, apperr.New(apperr.CheckFailed, "doctor found problems")
	}
	return res, nil
}

// syncCheck dry-runs a sync to see whether the database matches the TOML.
func syncCheck(ctx context.Context, a *app, cfg *registry.Config) check {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return check{"sync", "warn", "could not check: " + err.Error()}
	}
	defer func() { _ = tx.Rollback() }()
	res, err := registry.Sync(ctx, tx, cfg, "doctor")
	if err != nil {
		return check{"sync", "fail", err.Error()}
	}
	pending := res.Accounts.Added + res.Accounts.Updated + res.Accounts.Retired +
		res.Profiles.Added + res.Profiles.Updated + res.Profiles.Retired +
		res.Projects.Added + res.Projects.Updated + res.Projects.Retired + res.Fallbacks
	if pending > 0 {
		return check{"sync", "warn", fmt.Sprintf("database differs from TOML (%d changes); run `otag sync`", pending)}
	}
	return check{"sync", "ok", "database matches TOML"}
}

func version(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil && out.Len() == 0 {
		return "(version unknown: " + err.Error() + ")"
	}
	line, _, _ := bufio.NewReader(&out).ReadLine()
	return strings.TrimSpace(string(line))
}

func sortedNames[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
