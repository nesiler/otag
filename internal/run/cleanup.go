package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/proc"
	"github.com/nesiler/otag/internal/tmux"
)

// Ending a run (done, fail, stop, crash, quota, exit) sets runs.cleanup_pending in the
// same transaction. Whoever ended it kills its windows and processes after the commit
// (Cleanup) and clears the flag once nothing is left; a failure is kept in
// runs.cleanup_error. The daemon retries pending cleanups, and Start finishes them
// before it reuses their workspace, so no agent outlives its run.

// killGrace is how long the processes of a killed window get to exit after the SIGHUP
// tmux sends before the survivors get SIGKILL; reapWait bounds the wait after that.
var (
	killGrace = 2 * time.Second
	reapWait  = 2 * time.Second
)

// paneRec is one recorded window of a run: its pane and the process started in it.
// birth is the process start time recorded at launch ("" when unknown: runs from
// before schema v5, or a system without a process table).
type paneRec struct {
	pane  string
	pid   int
	birth string
}

// sessionPanes lists every pane (main first, then aux) recorded for the run, with the
// pid it was started with.
func (m *Manager) sessionPanes(ctx context.Context, runID string) ([]paneRec, error) {
	rows, err := m.DB.QueryContext(ctx, `SELECT coalesce(tmux_pane, ''), coalesce(pid, 0),
		coalesce(pid_started_at, ''), aux_panes FROM sessions WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[[2]any]bool{} // pane ids repeat after a tmux server restart: key on the pid too
	var out []paneRec
	add := func(p string, pid int, birth string) {
		if k := [2]any{p, pid}; p != "" && !seen[k] {
			seen[k] = true
			out = append(out, paneRec{p, pid, birth})
		}
	}
	for rows.Next() {
		var pane, birth, aux string
		var pid int
		if err := rows.Scan(&pane, &pid, &birth, &aux); err != nil {
			return nil, err
		}
		add(pane, pid, birth)
		var as []AuxPane
		_ = json.Unmarshal([]byte(aux), &as)
		for _, a := range as {
			add(a.Pane, a.PID, a.Birth)
		}
	}
	return out, rows.Err()
}

// selfPane is the pane of otag's tmux server this process runs in ("" outside it). A
// pane of another tmux server (the user's) has an id that may also exist on otag's.
func (m *Manager) selfPane() string {
	if m.Getenv == nil {
		return ""
	}
	pane := m.Getenv("TMUX_PANE")
	if sock, _, _ := strings.Cut(m.Getenv("TMUX"), ","); sock != "" && m.Tmux != nil && filepath.Base(sock) != m.Tmux.Socket {
		return ""
	}
	return pane
}

// owner is the value of the @otag_db tag of this database's windows.
func (m *Manager) owner() string { return m.DB.Path }

// reap kills what is left of a run's recorded windows that want selects: aux windows
// first, then the main pane, the caller's own pane ($TMUX_PANE) always last. The
// processes of those windows must exit after the SIGHUP tmux sends; survivors get
// SIGKILL after killGrace. They are taken from one snapshot before anything is killed:
// each recorded process (pid + birth recorded at launch) with its descendants and the
// rest of its session, which also finds children reparented after it died
// (proc.Family). A record without a birth (before v5) is trusted only through a pane
// that still runs it. A pid that now runs another process, a window tagged for another
// run or database, this process and its ancestors are never touched. It returns the panes whose windows it killed, whether it looked at
// everything (every recorded pane was selected, and the caller's own pane, whose
// processes it cannot wait for, was not killed), and what failed: windows that could
// not be killed (werr), processes that could not be killed or verified gone (perr).
func (m *Manager) reap(ctx context.Context, runID string, want func(string) bool) (closed []string, complete bool, werr, perr error) {
	recs, err := m.sessionPanes(ctx, runID)
	if err != nil {
		return []string{}, false, err, nil
	}
	return m.reapRecs(ctx, runID, recs, want)
}

// reapRecs is reap over the given windows of run runID.
func (m *Manager) reapRecs(ctx context.Context, runID string, recs []paneRec, want func(string) bool) (closed []string, complete bool, werr, perr error) {
	closed = []string{}
	if m.Tmux == nil {
		return closed, false, nil, nil
	}
	// The windows are killed even when ctx ends; only the wait for processes stops then.
	wait := ctx
	ctx = context.WithoutCancel(ctx)
	self := m.selfPane()
	slices.Reverse(recs)
	slices.SortStableFunc(recs, func(a, b paneRec) int {
		switch {
		case a.pane == self && b.pane != self:
			return 1
		case b.pane == self && a.pane != self:
			return -1
		}
		return 0
	})
	complete = true
	var werrs, perrs []error
	panes, lerr := m.Tmux.Panes(ctx)
	all, aerr := proc.All()
	if aerr != nil && !errors.Is(aerr, proc.ErrUnsupported) {
		perrs = append(perrs, fmt.Errorf("processes not verified: %w", aerr))
	}
	// A window is the run's when its tags name this run and database. An untagged one
	// is only accepted for a record without a birth (made before windows were tagged).
	// A recorded birth must match the process the pane runs now.
	owns := func(rec paneRec) func(tmux.PaneInfo) bool {
		return func(p tmux.PaneInfo) bool {
			if rec.birth != "" && aerr == nil && !p.Dead {
				if cur, ok := all[p.PID]; ok && cur.Birth != rec.birth {
					return false
				}
			}
			if p.Run != "" || p.Owner != "" {
				return p.Run == runID && p.Owner == m.owner()
			}
			return rec.birth == ""
		}
	}
	var tree []proc.Process
	for _, p := range recs {
		if !want(p.pane) {
			complete = false
			continue
		}
		if aerr != nil || p.pane == self || p.pid <= 1 {
			continue
		}
		if p.birth != "" {
			// pid + birth identify the process, its window or not.
			tree = append(tree, proc.Family(all, p.pid, p.birth)...)
		} else if lerr == nil && tmux.CheckIn(panes, p.pane, p.pid).State == tmux.Alive && owns(p)(panes[p.pane]) {
			tree = append(tree, proc.Family(all, p.pid, "")...)
		}
	}
	if aerr == nil {
		mine := proc.Ancestors(all, os.Getpid())
		tree = slices.DeleteFunc(tree, func(p proc.Process) bool { return mine[p.PID] || p.PID <= 1 })
	}
	kill := func(p paneRec) {
		killed, err := m.Tmux.KillOwned(ctx, p.pane, p.pid, owns(p))
		if err != nil {
			werrs = append(werrs, fmt.Errorf("kill %s: %w", p.pane, err))
		} else if killed {
			closed = append(closed, p.pane)
		}
	}
	var own *paneRec
	for i, p := range recs {
		switch {
		case !want(p.pane):
		case p.pane == self && self != "":
			own = &recs[i]
		default:
			kill(p)
		}
	}
	if err := waitGone(wait, tree); err != nil {
		perrs = append(perrs, err)
	}
	if own != nil {
		// Killing our own window ends this process: everything above is done by now.
		n := len(closed)
		kill(*own)
		complete = complete && len(closed) == n
	}
	return closed, complete, errors.Join(werrs...), errors.Join(perrs...)
}

// waitGone waits until every process in ps has exited (or runs something else now),
// sending SIGKILL to the survivors after killGrace, and gives up reapWait after that
// or when ctx ends.
func waitGone(ctx context.Context, ps []proc.Process) error {
	start := time.Now()
	killed := false
	for len(ps) > 0 {
		var left []proc.Process
		var unknown error
		for _, p := range ps {
			same, err := proc.Same(p.PID, p.Birth)
			if err != nil {
				unknown = err
			}
			if same || err != nil {
				left = append(left, p)
			}
		}
		if ps = left; len(ps) == 0 {
			return nil
		}
		if !killed && time.Since(start) >= killGrace {
			for _, p := range ps {
				_, _ = proc.Signal(p, syscall.SIGKILL)
			}
			killed = true
		}
		var timeout bool
		select {
		case <-ctx.Done():
			timeout = true
		case <-time.After(50 * time.Millisecond):
			timeout = killed && time.Since(start) >= killGrace+reapWait
		}
		if timeout {
			pids := make([]string, len(ps))
			for i, p := range ps {
				pids[i] = fmt.Sprint(p.PID)
			}
			if unknown != nil {
				return fmt.Errorf("process(es) %s not verified gone: %w", strings.Join(pids, ", "), unknown)
			}
			return fmt.Errorf("process(es) %s still running after SIGKILL", strings.Join(pids, ", "))
		}
	}
	return nil
}

// Cleanup kills what is left of a run: every recorded window and every process of it
// (see reap). For an ended run it then settles the cleanup obligation: when nothing is
// left, runs.cleanup_pending is cleared; a failure is kept in runs.cleanup_error for
// the next try (the daemon's, or the next start in the workspace). It returns the panes
// whose windows it killed. When called from inside the run's pane (`otag done` run by
// the agent) the caller dies with its pane; the daemon then confirms the cleanup.
func (m *Manager) Cleanup(ctx context.Context, runID string) ([]string, error) {
	closed, complete, werr, perr := m.reap(ctx, runID, func(string) bool { return true })
	err := errors.Join(werr, perr)
	if complete || err != nil {
		if serr := m.settleCleanup(ctx, runID, err); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	return closed, err
}

// Quiesce makes sure nothing of a run is left before another run uses its worktree. It
// is Cleanup with an error whenever something could not be killed or verified gone
// within ctx (the daemon gives it 15 s).
func (m *Manager) Quiesce(ctx context.Context, runID string) error {
	if _, err := m.Cleanup(ctx, runID); err != nil {
		return fmt.Errorf("run %s: %w", runID, err)
	}
	return nil
}

// killPanes is reap for callers that do not own the cleanup (the hook bridge): it
// settles an ended run's cleanup only when it looked at everything.
func (m *Manager) killPanes(ctx context.Context, runID string, want func(pane string) bool) []string {
	closed, complete, werr, perr := m.reap(ctx, runID, want)
	if err := errors.Join(werr, perr); complete || err != nil {
		_ = m.settleCleanup(ctx, runID, err)
	}
	return closed
}

// settleCleanup records the outcome of a cleanup of an ended run. A complete one also
// drops the run's delivery inbox (dropInbox).
func (m *Manager) settleCleanup(ctx context.Context, runID string, cerr error) error {
	ctx = context.WithoutCancel(ctx)
	ended := false
	defer func() {
		if ended {
			dropInbox(m.Getenv, runID)
		}
	}()
	return m.DB.Tx(ctx, func(tx *sql.Tx) error {
		if cerr == nil {
			_, err := tx.ExecContext(ctx, `UPDATE runs SET cleanup_pending = 0, cleanup_error = NULL
				WHERE id = ? AND status = 'ended' AND (cleanup_pending = 1 OR cleanup_error IS NOT NULL)`, runID)
			if err == nil {
				var st string
				ended = tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, runID).Scan(&st) == nil && st == Ended
			}
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE runs SET cleanup_pending = 1, cleanup_error = ?, cleanup_after = ?
			WHERE id = ? AND status = 'ended'`, oneLine(cerr.Error()),
			time.Now().Add(CleanupRetry).UTC().Format(db.TimeFormat), runID)
		return err
	})
}

// CleanupRetry spaces the daemon's attempts at a cleanup that failed
// (runs.cleanup_after).
const CleanupRetry = 30 * time.Second

// PendingCleanups lists ended runs whose cleanup has not been confirmed and is due at
// now (a failed one waits until runs.cleanup_after), the longest waiting first, so runs
// that keep failing never hold back the others.
func PendingCleanups(ctx context.Context, q db.Querier, now time.Time, limit int) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM runs
		WHERE cleanup_pending = 1 AND status = 'ended' AND (cleanup_after IS NULL OR cleanup_after <= ?)
		ORDER BY coalesce(cleanup_after, ended_at), id LIMIT ?`, now.UTC().Format(db.TimeFormat), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --------------------------------------------------------------------------- orphans

// Orphan is a window tagged for a run of this database (tmux options @otag_run and
// @otag_db) whose run has ended or does not exist: a launch that died between creating
// the window and recording it, or a cleanup that never happened.
type Orphan struct {
	Pane      string `json:"pane"`
	PID       int    `json:"pid"`
	Run       string `json:"run"`
	RunStatus string `json:"run_status"` // ended, or "" when the run does not exist
}

// Orphans lists the orphan windows of this database. A window whose run is still
// active is never one, recorded or not (its launch may still record it).
func (m *Manager) Orphans(ctx context.Context) ([]Orphan, error) {
	panes, err := m.Tmux.Panes(ctx)
	if err != nil {
		return nil, err
	}
	status := map[string]string{}
	var out []Orphan
	ids := make([]string, 0, len(panes))
	for id := range panes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := panes[id]
		if p.Run == "" || p.Owner != m.owner() {
			continue
		}
		st, ok := status[p.Run]
		if !ok {
			err := m.DB.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, p.Run).Scan(&st)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			status[p.Run] = st
		}
		if st == "" || st == Ended {
			out = append(out, Orphan{Pane: p.PaneID, PID: p.PID, Run: p.Run, RunStatus: st})
		}
	}
	return out, nil
}

// KillOrphan kills an orphan window (if it still runs o.PID and is still tagged for
// o.Run and this database) and every process of it (proc.Family).
func (m *Manager) KillOrphan(ctx context.Context, o Orphan) error {
	wait := ctx
	ctx = context.WithoutCancel(ctx)
	tagged := func(p tmux.PaneInfo) bool { return p.Run == o.Run && p.Owner == m.owner() }
	var tree []proc.Process
	if all, err := proc.All(); err == nil {
		panes, err := m.Tmux.Panes(ctx)
		if err != nil {
			return fmt.Errorf("orphan window of pane %s: %w", o.Pane, err)
		}
		if tmux.CheckIn(panes, o.Pane, o.PID).State == tmux.Alive && tagged(panes[o.Pane]) {
			mine := proc.Ancestors(all, os.Getpid())
			tree = slices.DeleteFunc(proc.Family(all, o.PID, ""), func(p proc.Process) bool { return mine[p.PID] })
		}
	} else if !errors.Is(err, proc.ErrUnsupported) {
		return fmt.Errorf("processes of pane %s not verified: %w", o.Pane, err)
	}
	if _, err := m.Tmux.KillOwned(ctx, o.Pane, o.PID, tagged); err != nil {
		return fmt.Errorf("kill orphan window of pane %s (run %s): %w", o.Pane, o.Run, err)
	}
	return waitGone(wait, tree)
}

// clearWorkspace makes sure nothing of an earlier run still works in the directory the
// task's next run will use (the task's worktree; in "same" mode the project directory,
// shared with every run there): pending cleanups of ended runs there are finished and
// orphan windows of them are killed. It fails when something survives. Start calls it
// before it creates the run, so a failure leaves nothing behind and may be retried.
func (m *Manager) clearWorkspace(ctx context.Context, taskID string) error {
	var mode, path string
	err := m.DB.QueryRowContext(ctx, `SELECT p.workspace_mode, p.path FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE t.id = ?`, taskID).Scan(&mode, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // create reports it
	}
	if err != nil {
		return err
	}
	orphans, err := m.Orphans(ctx)
	if err != nil {
		return fmt.Errorf("list windows: %w", err)
	}
	byRun := map[string][]Orphan{}
	var ids []string
	for _, o := range orphans {
		if o.RunStatus == Ended {
			if _, ok := byRun[o.Run]; !ok {
				ids = append(ids, o.Run)
			}
			byRun[o.Run] = append(byRun[o.Run], o)
		}
	}
	runs, err := workspaceRuns(ctx, m.DB, taskID, mode, path, ids)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range runs {
		if _, err := m.Cleanup(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
		}
		for _, o := range byRun[id] {
			if err := m.KillOrphan(ctx, o); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("an earlier run still works in the workspace: %w", err)
	}
	return nil
}

// workspaceRuns returns the ended runs that used the directory of the task's next run
// (the same task; in same mode any run in that directory) and still have a cleanup
// pending, or are among also.
func workspaceRuns(ctx context.Context, q db.Querier, taskID, mode, path string, also []string) ([]string, error) {
	args := []any{taskID, mode}
	marks := ""
	for _, id := range also {
		args = append(args, id)
		marks += ", ?"
	}
	rows, err := q.QueryContext(ctx, `
SELECT r.id, coalesce(r.task_id, ''), coalesce(w.path, '') FROM runs r
LEFT JOIN workspaces w ON w.id = (SELECT max(id) FROM workspaces WHERE run_id = r.id)
WHERE r.status = 'ended' AND (r.task_id = ? OR (? = 'same' AND w.mode = 'same'))
  AND (r.cleanup_pending = 1 OR r.id IN (''`+marks+`))`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, task, p string
		if err := rows.Scan(&id, &task, &p); err != nil {
			return nil, err
		}
		if task == taskID || sameDir(p, path) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// sameDir reports whether a and b name the same directory: the same file (device and
// inode, so case or symlink aliases on APFS count as one directory), or, when one of
// them cannot be read, the same cleaned path ignoring case (which may only refuse a
// start too many, never allow one too many on a case-insensitive volume).
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	clean := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		if r, err := filepath.Abs(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return strings.EqualFold(clean(a), clean(b))
}
