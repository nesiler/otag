package run

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/proc"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/workspace"
)

// errStopped means the run was stopped while it was being launched.
var errStopped = apperr.New(apperr.Conflict, "run was stopped while launching")

// launch does everything outside the create transaction: workspace, files, server,
// sidecars, pane. Every window is recorded right after it is created, in a transaction
// that also checks the run is still starting; if `otag run stop` won the race the
// windows are killed instead, so no harness outlives its run.
func (m *Manager) launch(ctx context.Context, p *plan, ad adapter.Adapter) (*StartResult, error) {
	getenv := m.Getenv
	wsSpec := workspace.Spec{Mode: p.mode, ProjectPath: p.projectPath}
	if p.mode == workspace.Worktree {
		wsSpec.Path = paths.Worktree(getenv, p.project, p.taskID)
		wsSpec.Branch = workspace.BranchFor(p.taskID)
	}
	ws, err := workspace.Prepare(ctx, wsSpec)
	if err != nil {
		return nil, withCause(CauseWorkspace, apperr.New(apperr.Unavailable, "workspace: %v", err))
	}
	if ws.BranchExisted {
		// The task's branch predates this run: keep its original base.
		var ref, sha string
		_ = m.DB.QueryRowContext(ctx, `SELECT coalesce(base_ref, ''), coalesce(base_sha, '') FROM workspaces
			WHERE task_id = ? AND branch = ? AND base_sha IS NOT NULL ORDER BY id LIMIT 1`, p.taskID, ws.Branch).
			Scan(&ref, &sha)
		if sha == "" {
			ref, sha = "", workspace.MergeBase(ctx, p.projectPath, ws.Branch)
		}
		ws.BaseRef, ws.BaseSHA = ref, sha
	}
	// Record the workspace (and its base) before anything else can fail.
	err = m.DB.Tx(ctx, func(tx *sql.Tx) error {
		r, err := load(ctx, tx, p.runID)
		if err != nil {
			return err
		}
		if r.Status != Queued {
			return errStopped
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO workspaces (project_id, task_id, run_id, mode, path, branch, base_ref, base_sha, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active')`, r.ProjectID, nullStr(p.taskID), p.runID, ws.Mode, ws.Path, nullStr(ws.Branch),
			nullStr(ws.BaseRef), nullStr(ws.BaseSHA))
		return err
	})
	if err != nil {
		return nil, err
	}

	if p.prior != nil {
		p.prior.readWorkspace(ctx, ws)
	}
	spec := m.runSpec(p, ws)
	if err := os.MkdirAll(spec.RunDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.CacheDir, 0o700); err != nil {
		return nil, err
	}
	ls, err := ad.Prepare(ctx, spec)
	if err != nil {
		return nil, withCause(CauseHarness, apperr.New(apperr.Unavailable, "%s adapter: %v", p.harness, err))
	}
	if len(ls.Argv) == 0 {
		return nil, apperr.New(apperr.Internal, "%s adapter returned an empty argv", p.harness)
	}
	if err := writeFiles(spec.RunDir, ls.Files); err != nil {
		return nil, err
	}

	// The run is starting before anything runs, so the first hook finds it.
	var sessionID int64
	err = m.DB.Tx(ctx, func(tx *sql.Tx) error {
		r, err := load(ctx, tx, p.runID)
		if err != nil {
			return err
		}
		if r.Status != Queued {
			return errStopped
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO sessions (run_id, harness, harness_session_id)
			VALUES (?, ?, ?) RETURNING id`, p.runID, p.harness, nullStr(ls.HarnessSessionID)).Scan(&sessionID); err != nil {
			return err
		}
		if err := r.setStatus(ctx, tx, m.Actor, Starting, map[string]any{"workspace": ws.Path, "reused": ws.Reused}); err != nil {
			return err
		}
		return r.moveTask(ctx, tx, m.Actor, []string{task.Claimed}, task.Running, "")
	})
	if err != nil {
		return nil, err
	}

	// A light run inherits otag's environment without secrets, under everything else.
	env := m.inheritedEnv(p)
	for k, v := range m.runEnv(p) {
		env[k] = v
	}
	// The agent is told to run `otag done`: that must be this otag, on PATH or not.
	path := getenv("PATH")
	if dir := filepath.Dir(spec.OtagBin); spec.OtagBin != "" && path == "" {
		path = dir
	} else if spec.OtagBin != "" {
		path = dir + string(os.PathListSeparator) + path
	}
	if path != "" {
		env["PATH"] = path
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range ls.Env {
		env[k] = v
	}
	addPushBlock(ctx, env) // after every other env, whose GIT_CONFIG_* slots it extends
	cwd := ls.Cwd
	if cwd == "" {
		cwd = spec.Workdir
	}
	l := &launcher{m: m, runID: p.runID, sessionID: sessionID, session: SessionName(p.project), taskID: p.window,
		env: env, cwd: cwd, runDir: spec.RunDir}
	fail := func(err error) (*StartResult, error) {
		l.killAll(ctx)
		return nil, err
	}

	// Server first, then sidecars (subscribed before the TUI can produce events), then the pane.
	if s := ls.Server; s != nil {
		if err := l.startAux(ctx, "server", *s); err != nil {
			return fail(err)
		}
		if err := waitReady(ctx, s.Ready, s.ReadyTimeout, l.aborted(ctx)); err != nil {
			if isStopped(err) {
				return fail(err)
			}
			return fail(withCause(CauseHarness, apperr.New(apperr.Unavailable, "%s server: %v", p.harness, err)))
		}
		if setup, ok := ad.(adapter.ServerSetup); ok {
			if err := setup.SetupServer(ctx, spec, ls); err != nil {
				return fail(withCause(CauseHarness, apperr.New(apperr.Unavailable, "%s server setup: %v", p.harness, err)))
			}
		}
	}
	for _, sc := range ls.Sidecars {
		if err := l.startAux(ctx, "sidecar", sc); err != nil {
			return fail(err)
		}
		if err := waitReady(ctx, sc.Ready, sc.ReadyTimeout, l.aborted(ctx)); err != nil {
			if isStopped(err) {
				return fail(err)
			}
			return fail(withCause(CauseHarness, apperr.New(apperr.Unavailable, "%s sidecar %s: %v", p.harness, sc.Name, err)))
		}
	}
	ref, err := m.Tmux.NewWindow(ctx, tmux.Window{Session: l.session, Name: p.window, Cwd: cwd, Env: env, Argv: ls.Argv,
		Run: p.runID, Owner: m.owner(), CleanEnv: true, EnvDir: spec.RunDir})
	if err != nil {
		return fail(withCause(CauseTmux, apperr.New(apperr.Unavailable, "tmux: %v", err)))
	}
	l.main = &ref
	var launched int64
	err = l.record(ctx, func(tx *sql.Tx, r *row) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET harness_session_id = coalesce(harness_session_id, ?)
			WHERE id = ?`, nullStr(ls.HarnessSessionID), sessionID); err != nil {
			return err
		}
		if ls.Server != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET server_url = ? WHERE id = ?`, nullStr(ls.Server.URL), sessionID); err != nil {
				return err
			}
		}
		var err error
		launched, err = r.recordID(ctx, tx, m.Actor, "run.launched", map[string]any{
			"session": ref.Session, "pane": ref.PaneID, "pid": ref.PID, "argv0": filepath.Base(ls.Argv[0])})
		if err != nil || p.messages == nil {
			return err
		}
		// The first prompt went to the harness with the pane: the messages it carries
		// are sent. Their proof is their marker in the run's first prompt event.
		_, err = msg.Settle(ctx, tx, msg.SettleInput{Nonce: p.messages.Nonce, Outcome: msg.Sent,
			Receipt: adapter.Receipt{Channel: adapter.ChanLaunchPrompt}}, msgActor(m.Actor))
		return err
	})
	if err != nil {
		return fail(err)
	}

	res := &StartResult{Attach: tmux.ShellQuote(m.Tmux.AttachArgs(ref.Session, ref.PaneID))}
	if p.messages != nil {
		for _, it := range p.messages.Items {
			res.Messages = append(res.Messages, it.Delivery.ID)
		}
	}
	if th, ok := ad.(adapter.TrustHandler); ok {
		res.Trust, res.FirstSeen = m.watchStartup(ctx, p.runID, launched, ref.PaneID, ref.PID, th, ls.StartupTimeout)
		if !res.FirstSeen {
			res.Warning = "no hook event yet; check the pane (" + res.Attach + ")"
		}
	}
	if res.Run, err = Get(ctx, m.DB, p.runID); err != nil {
		return nil, err
	}
	return res, nil
}

// launcher tracks the windows of one launch.
type launcher struct {
	m                      *Manager
	runID, session, taskID string
	sessionID              int64
	env                    map[string]string
	cwd, runDir            string
	aux                    []AuxPane
	main                   *tmux.PaneRef
}

func (l *launcher) startAux(ctx context.Context, role string, s adapter.Process) error {
	env := map[string]string{}
	for k, v := range l.env {
		env[k] = v
	}
	for k, v := range s.Env {
		env[k] = v
	}
	cwd := l.cwd
	if s.Cwd != "" {
		cwd = s.Cwd
	}
	name := s.Name
	if name == "" {
		name = role
	}
	ref, err := l.m.Tmux.NewWindow(ctx, tmux.Window{Session: l.session, Name: l.taskID + "~" + name, Cwd: cwd, Env: env, Argv: s.Argv,
		Run: l.runID, Owner: l.m.owner(), CleanEnv: true, EnvDir: l.runDir})
	if err != nil {
		return withCause(CauseTmux, apperr.New(apperr.Unavailable, "tmux %s window: %v", role, err))
	}
	l.aux = append(l.aux, AuxPane{Role: role, Name: name, Pane: ref.PaneID, PID: ref.PID, Birth: proc.Birth(ref.PID)})
	return l.record(ctx, nil)
}

// record stores the windows started so far on the session, unless the run stopped
// meanwhile (then errStopped; the caller kills the windows).
func (l *launcher) record(ctx context.Context, more func(*sql.Tx, *row) error) error {
	return l.m.DB.Tx(ctx, func(tx *sql.Tx) error {
		r, err := load(ctx, tx, l.runID)
		if err != nil {
			return err
		}
		if r.Status == Stopping || r.Status == Ended {
			return errStopped
		}
		var serverPID sql.NullInt64
		if len(l.aux) > 0 && l.aux[0].Role == "server" {
			serverPID = sql.NullInt64{Int64: int64(l.aux[0].PID), Valid: true}
		}
		q := `UPDATE sessions SET aux_panes = ?, server_pid = ?`
		args := []any{jsonList(l.aux), serverPID}
		if l.main != nil {
			// pid_started_at is the process's real birth: with the pid it identifies the
			// harness even after its window is gone (see cleanup.go).
			q += `, tmux_session = ?, tmux_window = ?, tmux_pane = ?, pid = ?, pid_started_at = ?`
			args = append(args, l.main.Session, l.main.WindowID, l.main.PaneID, l.main.PID, nullStr(proc.Birth(l.main.PID)))
		}
		if _, err := tx.ExecContext(ctx, q+" WHERE id = ?", append(args, l.sessionID)...); err != nil {
			return err
		}
		if more != nil {
			return more(tx, r)
		}
		return nil
	})
}

// aborted returns a check for waitReady: errStopped once the run is being stopped.
func (l *launcher) aborted(ctx context.Context) func() error {
	return func() error {
		var status string
		if err := l.m.DB.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, l.runID).Scan(&status); err != nil {
			return err
		}
		if status == Stopping || status == Ended {
			return errStopped
		}
		return nil
	}
}

// killAll kills every window this launch started, and their processes with them
// (reapRecs takes their family while the windows still run; the main pane's process
// is trusted through its tagged pane, its birth may not be recorded yet).
func (l *launcher) killAll(ctx context.Context) {
	var recs []paneRec
	if l.main != nil {
		recs = append(recs, paneRec{pane: l.main.PaneID, pid: l.main.PID})
	}
	for _, a := range l.aux {
		recs = append(recs, paneRec{a.Pane, a.PID, a.Birth})
	}
	_, _, _, _ = l.m.reapRecs(context.WithoutCancel(ctx), l.runID, recs, func(string) bool { return true })
}

// isStopped reports whether err is the lost race against `otag run stop`.
func isStopped(err error) bool { return errors.Is(err, errStopped) }

// causeErr tags a launch error with what failed (Cause*).
type causeErr struct {
	cause string
	err   error
}

func (e *causeErr) Error() string { return e.err.Error() }
func (e *causeErr) Unwrap() error { return e.err }

func withCause(cause string, err error) error { return &causeErr{cause, err} }

// causeOf returns the Cause* of a launch error ("" if unknown).
func causeOf(err error) string {
	var ce *causeErr
	if errors.As(err, &ce) {
		return ce.cause
	}
	return ""
}
