package cli

import (
	"context"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/nesiler/otag/internal/adapter/builtin"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/daemon"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/quota"
	"github.com/nesiler/otag/internal/run"
)

func cmdDaemon(ctx context.Context, a *app, args []string) (result, error) {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return daemonInstall(ctx, a, args[1:])
		case "uninstall":
			return daemonUninstall(ctx, a, args[1:])
		case "status":
			return daemonStatus(ctx, a, args[1:])
		}
	}
	return daemonRun(ctx, a, args)
}

// daemonRun is `otag daemon`: the loops in the foreground until SIGTERM/SIGINT. The
// launch agent runs `otag daemon --service`: permanent startup problems then exit 0
// (launchd restarts the daemon only after a failure), and the log is rotated.
func daemonRun(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("daemon")
	var once, service bool
	fs.BoolVar(&once, "once", false, "run one tick and exit")
	fs.BoolVar(&service, strings.TrimPrefix(daemon.ServiceFlag, "--"), false, "run as the launch agent")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		if err == nil {
			err = usageErr("usage: otag daemon [--once] | otag daemon install|uninstall|status")
		}
		return result{}, err
	}
	logger := log.New(a.env.Stderr, "otag daemon: ", log.LstdFlags)
	logPath := filepath.Join(paths.DataDir(a.env.Getenv), "daemon.log")
	rotate := func() {
		if rotated, err := daemon.RotateLog(logPath, daemon.LogMaxBytes, 1, 2); err != nil {
			logger.Printf("rotate %s: %v", logPath, err)
		} else if rotated {
			logger.Printf("log rotated (previous: %s.1)", logPath)
		}
	}
	// permanent ends the launch agent's process successfully so launchd does not
	// restart it every ThrottleInterval; a foreground daemon reports the error.
	permanent := func(err error) (result, error) {
		if !service {
			return result{}, err
		}
		logger.Printf("not starting: %v", err)
		logger.Printf("this does not go away by itself: fix it, then run `otag daemon install` (or `launchctl kickstart gui/%d/%s`)",
			os.Getuid(), daemon.Label)
		return result{data: map[string]any{"started": false, "error": err.Error()}}, nil
	}
	if service {
		rotate()
	}
	cfg, warnings, err := daemon.LoadConfig(daemon.ConfigFile(a.configDir()))
	if err != nil {
		return permanent(err)
	}
	for _, w := range warnings {
		logger.Printf("warning: %s", w)
	}
	lock, err := daemon.Acquire(paths.DataDir(a.env.Getenv))
	if apperr.Code(err) == apperr.Conflict {
		return permanent(err)
	}
	if err != nil {
		return result{}, err
	}
	defer func() { _ = lock.Release() }()
	d, err := a.openDB(ctx)
	if apperr.Code(err) == apperr.SchemaTooNew {
		return permanent(err)
	}
	if err != nil {
		return result{}, err
	}
	if d.ReadOnly() {
		return permanent(apperr.New(apperr.SchemaTooNew, "%s has schema v%d, this otag knows v%d", d.Path, d.Version, d.Latest))
	}
	dm := &daemon.Daemon{
		DB: d, Config: cfg, Log: logger,
		Runs: &run.Manager{DB: d, Adapters: builtin.Registry(), Tmux: a.tmux(), Getenv: a.env.Getenv, Actor: daemon.Actor},
		// One long-lived value: it keeps rollout offsets and network back-off.
		Collectors: []daemon.QuotaCollector{quota.New(d, quota.Options{Getenv: a.env.Getenv})},
	}
	if once {
		dm.Collect(ctx)
		dm.Tick(ctx)
		return result{data: map[string]any{"ticks": 1}}, nil
	}
	if service {
		var last time.Time
		dm.Housekeeping = func() {
			if time.Since(last) >= time.Minute {
				last = time.Now()
				rotate()
			}
		}
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	logger.Printf("started (pid %d, tick %s, db %s)", os.Getpid(), cfg.Tick, d.Path)
	err = dm.Run(ctx)
	logger.Printf("stopped")
	return result{}, err
}

func (a *app) launchd() daemon.Launchd {
	home := a.env.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return daemon.Launchd{Home: home, Launchctl: a.env.Getenv("OTAG_LAUNCHCTL"), UID: os.Getuid()}
}

// daemonBaseEnv are copied into the launch agent when set: if the daemon starts the
// otag tmux server, every run it launches inherits this environment.
var daemonBaseEnv = []string{"HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_*", "SSH_AUTH_SOCK"}

// daemonEnv is what the launch agent passes on: PATH with otag's directory first and
// without short-lived entries, the OTAG_* paths set for this otag (absolute) so the
// daemon uses the same database, the locale/user basics and the variables listed in
// [daemon] env_passthrough. It also returns the PATH entries it dropped.
func (a *app) daemonEnv(otagBin string, passthrough []string) (map[string]string, []string) {
	g := a.env.Getenv
	tmp := []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders", g("TMPDIR")}
	fnmDir := g("FNM_DIR")
	if fnmDir == "" && g("HOME") != "" {
		fnmDir = filepath.Join(g("HOME"), ".local", "share", "fnm")
	}
	fnmDefault := ""
	if fnmDir != "" {
		fnmDefault = filepath.Join(fnmDir, "aliases", "default", "bin")
	}
	path, dropped := daemon.PlistPATH(otagBin, g("PATH"), tmp, fnmDefault)
	env := map[string]string{"PATH": path}
	names := map[string]bool{}
	environ := a.env.Environ
	if environ == nil {
		environ = os.Environ
	}
	for _, kv := range environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			names[k] = true
		}
	}
	// PATH (sanitized above) and the OTAG_* paths (resolved below) are reserved: a
	// passthrough pattern such as "P*" must not bring the raw values back.
	reserved := func(k string) bool { return k == "PATH" || strings.HasPrefix(k, "OTAG_") }
	for _, pat := range append(slices.Clone(daemonBaseEnv), passthrough...) {
		if prefix, ok := strings.CutSuffix(pat, "*"); ok {
			for k := range names {
				if strings.HasPrefix(k, prefix) && !reserved(k) && g(k) != "" {
					env[k] = g(k)
				}
			}
		} else if v := g(pat); v != "" && !reserved(pat) {
			env[pat] = v
		}
	}
	// launchd gives GUI agents their own SSH agent socket, which changes every boot:
	// only a stable custom one (1Password, gpg-agent, ...) is worth pinning.
	if s := env["SSH_AUTH_SOCK"]; s != "" && slices.ContainsFunc(tmp, func(t string) bool {
		return t != "" && strings.HasPrefix(s, strings.TrimRight(t, "/")+"/")
	}) {
		delete(env, "SSH_AUTH_SOCK")
	}
	resolved := map[string]func() string{
		"OTAG_DB":          func() string { return paths.DB(g) },
		"OTAG_CONFIG_DIR":  func() string { return paths.ConfigDir(g) },
		"OTAG_DATA_DIR":    func() string { return paths.DataDir(g) },
		"OTAG_TMUX_SOCKET": func() string { return paths.TmuxSocket(g) },
		"OTAG_TMUX_CONF":   func() string { return paths.TmuxConf(g) },
		"OTAG_BIN":         func() string { return otagBin },
	}
	for k, f := range resolved {
		if g(k) != "" {
			env[k] = f()
		}
	}
	return env, dropped
}

type daemonInfo struct {
	Running bool               `json:"running"`
	PID     int                `json:"pid,omitempty"`
	Agent   daemon.AgentStatus `json:"launch_agent"`
	Config  string             `json:"config"`
	Log     string             `json:"log"`
	Unread  int                `json:"unread_notifications"`
}

func daemonInstall(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("daemon install")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		if err == nil {
			err = usageErr("usage: otag daemon install")
		}
		return result{}, err
	}
	cfg, _, err := daemon.LoadConfig(daemon.ConfigFile(a.configDir()))
	if err != nil {
		return result{}, err
	}
	otagBin := paths.OtagBin(a.env.Getenv)
	logPath := filepath.Join(paths.DataDir(a.env.Getenv), "daemon.log")
	env, dropped := a.daemonEnv(otagBin, cfg.EnvPassthrough)
	path, err := a.launchd().Install(ctx, daemon.PlistSpec{OtagBin: otagBin, LogPath: logPath, Env: env})
	if err != nil {
		return result{}, err
	}
	names := slices.Sorted(maps.Keys(env))
	data := map[string]any{"plist": path, "otag_bin": otagBin, "log": logPath, "env": names, "path": env["PATH"],
		"path_dropped": dropped}
	return result{data: data, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "installed %s (runs %s daemon %s)\nlog: %s\nenvironment: %s\n", path, otagBin,
			daemon.ServiceFlag, logPath, strings.Join(names, " "))
		if len(dropped) > 0 {
			_, _ = fmt.Fprintf(w, "PATH: dropped short-lived entries %s\n", strings.Join(dropped, ", "))
		}
	}}, nil
}

func daemonUninstall(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("daemon uninstall")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		if err == nil {
			err = usageErr("usage: otag daemon uninstall")
		}
		return result{}, err
	}
	l := a.launchd()
	removed, err := l.Uninstall(ctx)
	if err != nil {
		return result{}, err
	}
	return result{data: map[string]any{"removed": removed, "plist": l.PlistFile()}, human: func(w io.Writer) {
		if removed {
			_, _ = fmt.Fprintf(w, "removed %s\n", l.PlistFile())
		} else {
			_, _ = fmt.Fprintf(w, "not installed (%s)\n", l.PlistFile())
		}
	}}, nil
}

func daemonStatus(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("daemon status")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		if err == nil {
			err = usageErr("usage: otag daemon status")
		}
		return result{}, err
	}
	data := daemonInfo{Agent: a.launchd().Status(ctx), Config: daemon.ConfigFile(a.configDir()),
		Log: filepath.Join(paths.DataDir(a.env.Getenv), "daemon.log")}
	running, pid, err := daemon.Probe(paths.DataDir(a.env.Getenv))
	if err != nil {
		return result{}, err
	}
	data.Running, data.PID = running, pid
	if d, err := a.openDB(ctx); err == nil {
		data.Unread, _ = notify.Unread(ctx, d)
	}
	return result{data: data, human: func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintf(tw, "daemon:\t%s\n", daemonText(running, pid))
		agent := "not installed"
		if data.Agent.Installed {
			agent = "installed"
		}
		if data.Agent.Loaded {
			agent += ", loaded"
		}
		_, _ = fmt.Fprintf(tw, "launch agent:\t%s (%s)\n", agent, data.Agent.Plist)
		_, _ = fmt.Fprintf(tw, "config:\t%s\n", data.Config)
		_, _ = fmt.Fprintf(tw, "log:\t%s\n", data.Log)
		_, _ = fmt.Fprintf(tw, "notifications:\t%d unread\n", data.Unread)
		_ = tw.Flush()
	}}, nil
}

func daemonText(running bool, pid int) string {
	if !running {
		return "not running"
	}
	return "running (pid " + strconv.Itoa(pid) + ")"
}

// --------------------------------------------------------------------------- notify

func cmdNotify(ctx context.Context, a *app, args []string) (result, error) {
	if len(args) == 0 {
		return result{}, usageErr("usage: otag notify list|ack")
	}
	switch args[0] {
	case "list":
		return notifyList(ctx, a, args[1:])
	case "ack":
		return notifyAck(ctx, a, args[1:])
	}
	return result{}, usageErr("unknown notify command %q", args[0])
}

func notifyList(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("notify list")
	var f notify.Filter
	fs.BoolVar(&f.All, "all", false, "include acknowledged notifications")
	fs.IntVar(&f.Limit, "limit", 50, "max rows")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		if err == nil {
			err = usageErr("usage: otag notify list [--all] [--limit N]")
		}
		return result{}, err
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	ns, err := notify.List(ctx, d, f)
	if err != nil {
		return result{}, err
	}
	return result{data: ns, human: func(w io.Writer) {
		if len(ns) == 0 {
			_, _ = fmt.Fprintln(w, "no notifications")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID\tTIME\tSEVERITY\tKIND\tTASK\tRUN\tTITLE")
		for _, n := range ns {
			mark := ""
			if n.ReadAt != "" {
				mark = " (acked)"
			}
			_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s%s\n", n.ID, n.CreatedAt, n.Severity, n.Kind, dash(n.TaskID),
				dash(n.RunID), n.Title, mark)
		}
		_ = tw.Flush()
		for _, n := range ns {
			if n.Body != "" {
				_, _ = fmt.Fprintf(w, "\n#%d %s\n  %s\n", n.ID, n.Title, n.Body)
			}
		}
	}}, nil
}

func notifyAck(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("notify ack")
	var all bool
	fs.BoolVar(&all, "all", false, "acknowledge every unread notification")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if all == (len(pos) > 0) {
		return result{}, usageErr("usage: otag notify ack ID... | otag notify ack --all")
	}
	var ids []int64
	for _, p := range pos {
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			return result{}, usageErr("bad notification id %q", p)
		}
		ids = append(ids, id)
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	n, err := notify.Ack(ctx, d, ids)
	if err != nil {
		return result{}, err
	}
	return result{data: map[string]any{"acked": n}, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "acknowledged %d notification(s)\n", n)
	}}, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
