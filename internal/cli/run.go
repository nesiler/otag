package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/nesiler/otag/internal/adapter/builtin"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/identity"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
)

func (a *app) tmux() *tmux.Client {
	return tmux.New(paths.TmuxSocket(a.env.Getenv), paths.TmuxConf(a.env.Getenv))
}

func (a *app) manager(ctx context.Context) (*run.Manager, identity.Identity, error) {
	id, err := a.identity(ctx)
	if err != nil {
		return nil, id, err
	}
	return &run.Manager{
		DB: a.db, Adapters: builtin.Registry(), Tmux: a.tmux(), Getenv: a.env.Getenv,
		Actor: run.Actor{Name: a.actorName(id)},
	}, id, nil
}

func cmdRun(ctx context.Context, a *app, args []string) (result, error) {
	subs := map[string]func(context.Context, *app, []string) (result, error){
		"start": runStart, "list": runList, "show": runShow, "stop": runStop, "attach": runAttach, "resume": runResume,
	}
	if len(args) == 0 {
		return result{}, usageErr("usage: otag run start|list|show|stop|attach|resume")
	}
	fn, ok := subs[args[0]]
	if !ok {
		return result{}, usageErr("unknown run command %q", args[0])
	}
	return fn(ctx, a, args[1:])
}

func runStart(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("run start")
	var in run.StartInput
	var noAttach bool
	fs.StringVar(&in.Profile, "profile", "", "profile (default: task profile, then project default)")
	fs.BoolVar(&noAttach, "no-attach", false, "do not attach the terminal to the new window")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag run start <task> [--profile p] [--no-attach]")
	}
	in.TaskID = pos[0]
	m, _, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	res, err := m.Start(ctx, in)
	if err != nil {
		return result{}, err
	}
	out := result{data: res, human: func(w io.Writer) {
		r := res.Run
		_, _ = fmt.Fprintf(w, "started %s for task %s (%s, %s) — %s\n", r.ID, r.TaskID, r.Profile, r.Harness, r.Status)
		if ws := r.Workspace; ws != nil {
			_, _ = fmt.Fprintf(w, "workspace: %s", ws.Path)
			if ws.Branch != "" {
				_, _ = fmt.Fprintf(w, " (branch %s)", ws.Branch)
			}
			_, _ = fmt.Fprintln(w)
		}
		if s := r.Session; s != nil {
			_, _ = fmt.Fprintf(w, "pane:      %s in tmux session %s (socket %s)\n", s.TmuxPane, s.TmuxSession, m.Tmux.Socket)
		}
		if res.Trust == "accepted" {
			_, _ = fmt.Fprintln(w, "trust:     folder trust dialog accepted")
		}
		_, _ = fmt.Fprintf(w, "attach:    %s\n", res.Attach)
		if res.Warning != "" {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", res.Warning)
		}
	}}
	if noAttach || a.json || !isTerminal(a.env.Stdout) || res.Run.Session == nil {
		return out, nil
	}
	out.human(a.env.Stdout)
	return result{}, execAttach(ctx, a, m.Tmux, res.Run.Session.TmuxSession, res.Run.Session.TmuxPane)
}

func runList(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("run list")
	var f run.Filter
	var all bool
	fs.BoolVar(&all, "all", false, "all projects")
	fs.BoolVar(&f.Active, "active", false, "only runs that have not ended")
	fs.StringVar(&f.TaskID, "task", "", "runs of one task")
	fs.IntVar(&f.Limit, "limit", 50, "max rows")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	if !all && f.TaskID == "" {
		p, err := id.RequireProject()
		if err != nil {
			return result{}, err
		}
		f.ProjectID = p.ID
	}
	runs, err := run.List(ctx, a.db, f)
	if err != nil {
		return result{}, err
	}
	checkLive(ctx, a, runs)
	return result{data: runs, human: func(w io.Writer) { printRuns(w, runs, all) }}, nil
}

// checkLive fills liveness for runs that have not ended.
func checkLive(ctx context.Context, a *app, runs []run.Run) {
	var ptrs []*run.Run
	for i := range runs {
		if runs[i].Status != run.Ended {
			ptrs = append(ptrs, &runs[i])
		}
	}
	if len(ptrs) > 0 {
		run.CheckLive(ctx, a.tmux(), ptrs)
	}
}

func liveText(r *run.Run) string {
	switch {
	case r.Live == nil:
		return "-"
	case r.Dead():
		return "DEAD (" + r.Live.State + ", " + r.Live.Detail + ")"
	}
	return r.Live.State
}

func printRuns(w io.Writer, runs []run.Run, withProject bool) {
	if len(runs) == 0 {
		_, _ = fmt.Fprintln(w, "no runs")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if withProject {
		_, _ = fmt.Fprint(tw, "PROJECT\t")
	}
	_, _ = fmt.Fprintln(tw, "RUN\tTASK\tSTATUS\tPROFILE\tPANE\tLIVE\tSTARTED")
	for i := range runs {
		r := &runs[i]
		status := r.Status
		if r.EndedReason != "" {
			status += " (" + r.EndedReason + ")"
		}
		pane := "-"
		if r.Session != nil && r.Session.TmuxPane != "" {
			pane = r.Session.TmuxPane
		}
		if withProject {
			_, _ = fmt.Fprintf(tw, "%s\t", r.Project)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.TaskID, status, r.Profile, pane, liveText(r), r.StartedAt)
	}
	_ = tw.Flush()
}

func runShow(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("run show")
	var n int
	fs.IntVar(&n, "events", 20, "number of recent events")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag run show <run|task>")
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	r, err := run.Resolve(ctx, d, pos[0])
	if err != nil {
		return result{}, err
	}
	if err := run.Details(ctx, d, r, n); err != nil {
		return result{}, err
	}
	run.CheckLive(ctx, a.tmux(), []*run.Run{r})
	return result{data: r, human: func(w io.Writer) { printRun(w, r) }}, nil
}

func printRun(w io.Writer, r *run.Run) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			_, _ = fmt.Fprintf(tw, "%s:\t%s\n", k, v)
		}
	}
	row("run", r.ID)
	row("task", strings.TrimSpace(r.TaskID+" ["+r.TaskStatus+"] "+r.TaskTitle))
	row("project", r.Project)
	status := r.Status
	if r.EndedReason != "" {
		status += " (" + r.EndedReason + ")"
	}
	row("status", status)
	row("error", r.Error)
	row("profile", strings.Join(strings.Fields(fmt.Sprintf("%s (rev %d) %s/%s %s %s",
		r.Profile, r.RevisionID, r.Account, r.Harness, r.Model, r.Effort)), " "))
	row("guard", r.Guard)
	row("attempt", fmt.Sprint(r.Attempt))
	row("retry of", r.RetryOf)
	row("queued", r.QueuedAt)
	row("started", r.StartedAt)
	row("first activity", r.FirstActivityAt)
	row("last progress", r.LastProgressAt)
	row("ended", r.EndedAt)
	if s := r.Session; s != nil {
		row("session", fmt.Sprintf("%s [%s]", s.HarnessSessionID, s.Status))
		row("pane", fmt.Sprintf("%s pid %d in %s", s.TmuxPane, s.PID, s.TmuxSession))
		for _, x := range s.AuxPanes {
			row(x.Role, fmt.Sprintf("%s pane %s pid %d", x.Name, x.Pane, x.PID))
		}
		row("server", s.ServerURL)
	}
	row("live", liveText(r))
	if a := r.AuxDown; a != nil {
		row("aux down", fmt.Sprintf("%s %s: %s %s", a.Role, a.Name, a.Live.State, a.Live.Detail))
	}
	if r.CleanupPending {
		row("cleanup", strings.TrimSpace("pending "+r.CleanupError))
	}
	if ws := r.Workspace; ws != nil {
		row("workspace", fmt.Sprintf("%s (%s) %s", ws.Path, ws.Mode, ws.Branch))
		row("base", strings.TrimSpace(ws.BaseRef+" "+ws.BaseSHA))
		row("head", ws.HeadSHA)
	}
	if u := r.Usage; u != nil && u.Rows > 0 {
		row("usage", fmt.Sprintf("in %d out %d cache r/w %d/%d reasoning %d (%d rows)",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens, u.Rows))
	}
	for _, o := range r.Outputs {
		row("output "+o.Kind, o.Value)
	}
	_ = tw.Flush()
	if len(r.Events) > 0 {
		_, _ = fmt.Fprintln(w, "\nevents:")
		tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, e := range r.Events {
			tr := ""
			if e.ToStatus != "" {
				tr = e.FromStatus + "→" + e.ToStatus
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", e.CreatedAt, e.Kind, tr, e.Source)
		}
		_ = tw.Flush()
	}
}

func runStop(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("run stop")
	var reason string
	var park bool
	fs.StringVar(&reason, "reason", "", "why the run is stopped")
	fs.BoolVar(&park, "park", false, "park it: end the session, keep the task waiting for its answer")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag run stop <run|task> [--reason r] [--park]")
	}
	if park {
		return runStopPark(ctx, a, pos[0], reason)
	}
	m, _, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	res, err := m.Stop(ctx, pos[0], reason)
	if err != nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) {
		r := res.Run
		_, _ = fmt.Fprintf(w, "%s: %s (%s); pane was %s; killed %d window(s); task %s is %s\n",
			r.ID, r.Status, r.EndedReason, res.Before.State, len(res.Killed), r.TaskID, r.TaskStatus)
		if res.CleanupError != "" {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: cleanup incomplete (the daemon retries): %s\n", res.CleanupError)
		}
	}}, nil
}

func runAttach(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("run attach")
	var printOnly bool
	fs.BoolVar(&printOnly, "print", false, "only print the attach command")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag run attach <run|task> [--print]")
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	r, err := run.Resolve(ctx, d, pos[0])
	if err != nil {
		return result{}, err
	}
	if r.Session == nil || r.Session.TmuxSession == "" {
		return result{}, apperr.New(apperr.NotFound, "run %s has no tmux pane", r.ID)
	}
	tc := a.tmux()
	argv := tc.AttachArgs(r.Session.TmuxSession, r.Session.TmuxPane)
	data := map[string]any{"run": r.ID, "session": r.Session.TmuxSession, "pane": r.Session.TmuxPane,
		"socket": tc.Socket, "argv": argv, "command": tmux.ShellQuote(argv)}
	res := result{data: data, human: func(w io.Writer) { _, _ = fmt.Fprintln(w, tmux.ShellQuote(argv)) }}
	if printOnly || a.json || !isTerminal(a.env.Stdout) {
		return res, nil
	}
	return result{}, execAttach(ctx, a, tc, r.Session.TmuxSession, r.Session.TmuxPane)
}

// execAttach replaces the process with tmux attach. TMUX is unset so attaching works
// from inside another tmux (a nested client). The return keys (M-q, C-q, the "← otag"
// status button) are applied to otag's server first, like the TUI does.
func execAttach(ctx context.Context, a *app, tc *tmux.Client, session, pane string) error {
	argv := tc.AttachArgs(session, pane)
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return apperr.New(apperr.Unavailable, "tmux not found")
	}
	if err := tc.EnsureUI(ctx); err != nil {
		_, _ = fmt.Fprintf(a.env.Stderr, "warning: tmux bindings: %v\n", err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_PANE=") {
			env = append(env, kv)
		}
	}
	return syscall.Exec(path, argv, env)
}

// isTerminal reports whether v (stdin or stdout) is a terminal.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
