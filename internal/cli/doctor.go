package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/daemon"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/quota"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/setup"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	// Fix is what to run (or edit) to clear a warn or fail.
	Fix string `json:"fix,omitempty"`
}

func cmdDoctor(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("doctor")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	var checks []check
	add := func(name, status, fix, format string, args ...any) {
		checks = append(checks, check{name, status, fmt.Sprintf(format, args...), fix})
	}
	reg := setup.RegistryFile(a.configDir())

	var cfg *registry.Config
	if !a.configured() {
		add("registry", "fail", "otag init", "otag is not set up yet: %s does not exist", reg)
	} else if c, err := registry.Load(a.configDir(), a.env.Getenv); err != nil {
		add("registry", "fail", "edit the files under "+a.configDir()+", then `otag sync` (or start over: `otag init --force`)",
			"%s", errText(err))
	} else {
		cfg = c
		add("registry", "ok", "", "%s: %d accounts, %d profiles, %d projects",
			a.configDir(), len(cfg.Accounts), len(cfg.Profiles), len(cfg.Projects))
		for _, w := range cfg.Warnings {
			fix := "edit " + reg
			if name, ok := strings.CutPrefix(w, "project "); ok {
				name, _, _ = strings.Cut(name, ":")
				fix = fmt.Sprintf("edit %s, or `otag project remove %s`", setup.ProjectFile(a.configDir(), name), name)
			}
			add("registry", "warn", fix, "%s", w)
		}
		if len(cfg.Projects) == 0 {
			add("projects", "warn", "otag project add   (inside a git repository)", "no projects registered")
		}
	}

	d, err := a.openDB(ctx)
	switch {
	case err != nil:
		add("database", "fail", "check that "+filepath.Dir(a.dbPath())+" is writable (or set OTAG_DB)", "%v", err)
	case d.ReadOnly():
		add("database", "fail", "upgrade otag (`brew upgrade otag`)",
			"%s: schema v%d is newer than this binary (v%d); writes refused", d.Path, d.Version, d.Latest)
	default:
		var mode string
		var fk int
		_ = d.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
		_ = d.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
		var integrity string
		_ = d.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity)
		status, fix := "ok", ""
		if mode != "wal" || fk != 1 || integrity != "ok" {
			status, fix = "fail", "restore "+d.Path+" from a backup, or move it away to start a new database"
		}
		add("database", status, fix, "%s: schema v%d, journal=%s, foreign_keys=%d, quick_check=%s",
			d.Path, d.Version, mode, fk, integrity)
		// The file is forced to 0600; an existing directory is never chmod-ed, only reported.
		if st, err := os.Stat(filepath.Dir(d.Path)); err == nil && st.Mode().Perm()&0o077 != 0 {
			add("database", "warn", "chmod 700 "+filepath.Dir(d.Path), "directory %s is accessible by other users (mode %v)",
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
		add("guard", "ok", "", "%s", strings.Join(levels, " "))
	}

	for _, tool := range []string{"tmux", "git"} {
		if path, ok := setup.LookPath(tool, a.pathEnv()); ok {
			add(tool, "ok", "", "%s", path)
		} else {
			add(tool, "fail", setup.InstallHint(tool), "not found on PATH (required)")
		}
	}
	used := map[string]bool{}
	if cfg != nil {
		for _, acc := range cfg.Accounts {
			used[acc.Harness] = true
		}
	}
	for _, h := range registry.Harnesses {
		path, ok := setup.LookPath(h, a.pathEnv())
		switch {
		case ok:
			add("harness "+h, "ok", "", "%s %s", path, version(ctx, path))
		case used[h]:
			add("harness "+h, "fail", setup.InstallHint(h), "not found on PATH, but an account uses it")
		default:
			add("harness "+h, "warn", setup.InstallHint(h)+"   (optional; then `otag init` adds its profiles)",
				"not found on PATH (no account uses it)")
		}
	}
	if cfg != nil {
		for _, name := range sortedNames(cfg.Accounts) {
			acc := cfg.Accounts[name]
			if acc.ConfigDir != "" {
				if st, err := os.Stat(acc.ConfigDir); err != nil || !st.IsDir() {
					add("account "+name, "warn", loginFix(acc), "config_dir %s does not exist", acc.ConfigDir)
				}
			}
			if acc.Command != "" {
				if _, ok := setup.LookPath(acc.Command, a.pathEnv()); !ok && !isExecutable(acc.Command) {
					add("account "+name, "warn", "set command to an absolute path in "+reg,
						"command %q not on PATH (shell functions are not visible to otag)", acc.Command)
				}
			}
		}
	}

	cfgFile := daemon.ConfigFile(a.configDir())
	if _, warns, err := daemon.LoadConfig(cfgFile); err != nil {
		add("daemon config", "fail", "edit "+cfgFile, "%v", err)
	} else {
		for _, w := range warns {
			add("daemon config", "warn", "edit "+cfgFile, "%s", w)
		}
	}
	logPath := filepath.Join(paths.DataDir(a.env.Getenv), "daemon.log")
	if running, pid, err := daemon.Probe(paths.DataDir(a.env.Getenv)); err != nil {
		add("daemon", "warn", "otag daemon install", "%v", err)
	} else if running {
		add("daemon", "ok", "", "running (pid %d)", pid)
	} else {
		fix := "otag daemon install"
		if _, err := os.Stat(a.launchd().PlistFile()); err == nil {
			fix += "   (reloads it; see " + logPath + ")"
		}
		add("daemon", "warn", fix, "not running: no crash reaping, stall watchdog or fallback")
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
						add("daemon env", "warn", fmt.Sprintf("add %q to [daemon] env_passthrough in %s, then `otag daemon install`", v, cfgFile),
							"%s (account %s) uses {env:%s}, which the launch agent does not set", path, name, v)
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
				add("profile "+name, "warn", fmt.Sprintf("add %q to env_passthrough of [profiles.%s] in %s, then `otag sync`", v, name, reg),
					"%s uses {env:%s}, which runs do not get (they start with a clean environment)", path, v)
			}
		}
	}

	failed := slices.ContainsFunc(checks, func(c check) bool { return c.Status == "fail" })
	res := result{data: checks, human: func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, c := range checks {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
			if c.Fix != "" {
				_, _ = fmt.Fprintf(tw, "\t\tfix: %s\n", c.Fix)
			}
		}
		_ = tw.Flush()
	}}
	if failed {
		return res, apperr.New(apperr.CheckFailed, "doctor found problems")
	}
	return res, nil
}

// loginFix is how to create an account's missing config_dir: log in once with it.
func loginFix(acc registry.Account) string {
	switch acc.Harness {
	case "claude":
		return "log in once: CLAUDE_CONFIG_DIR=" + acc.ConfigDir + " claude"
	case "codex":
		return "log in once: CODEX_HOME=" + acc.ConfigDir + " codex login"
	case "opencode":
		return "create " + filepath.Join(acc.ConfigDir, "config", "opencode", "opencode.json") +
			" (config_dir is an account root), or remove config_dir"
	}
	return "create " + acc.ConfigDir + ", or remove config_dir"
}

func isExecutable(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// syncCheck dry-runs a sync to see whether the database matches the TOML.
func syncCheck(ctx context.Context, a *app, cfg *registry.Config) check {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return check{"sync", "warn", "could not check: " + err.Error(), "otag sync"}
	}
	defer func() { _ = tx.Rollback() }()
	res, err := registry.Sync(ctx, tx, cfg, "doctor")
	if err != nil {
		return check{"sync", "fail", err.Error(), "otag sync   (it prints what is wrong)"}
	}
	pending := res.Accounts.Added + res.Accounts.Updated + res.Accounts.Retired +
		res.Profiles.Added + res.Profiles.Updated + res.Profiles.Retired +
		res.Projects.Added + res.Projects.Updated + res.Projects.Retired + res.Fallbacks
	if pending > 0 {
		return check{"sync", "warn", fmt.Sprintf("database differs from TOML (%d changes)", pending), "otag sync"}
	}
	return check{"sync", "ok", "database matches TOML", ""}
}
