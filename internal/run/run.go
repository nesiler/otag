// Package run implements the run lifecycle: start a harness in a tmux window on a
// workspace, follow it through normalized hook events, and end it with done, fail,
// stop or a crash. Every state change is a CAS update plus an events row in one
// transaction; task status follows the run (see docs/ADAPTERS.md, "State machine").
package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/tmux"
)

// Run statuses.
const (
	Queued   = "queued"   // row created, nothing launched yet
	Starting = "starting" // pane launched, no session event yet
	Idle     = "idle"     // harness alive, turn finished, waiting for input
	Running  = "running"  // turn in progress
	Waiting  = "waiting"  // blocked on a question or permission decision
	Parked   = "parked"   // session closed on purpose, resumable (phase 4)
	Stopping = "stopping" // `otag run stop` in progress
	Ended    = "ended"    // see ended_reason
)

// Statuses lists every run status.
var Statuses = []string{Queued, Starting, Idle, Running, Waiting, Parked, Stopping, Ended}

// Ended reasons.
const (
	ReasonDone         = "done"          // child called `otag done`
	ReasonFailed       = "failed"        // child called `otag fail`
	ReasonStopped      = "stopped"       // `otag run stop` on a live run
	ReasonCrashed      = "crashed"       // pane/process died without done/fail (found by stop or liveness)
	ReasonExited       = "exited"        // harness reported session_end without done/fail
	ReasonLaunchFailed = "launch_failed" // workspace, adapter or tmux failed during start, or the harness died before its first event
	ReasonStalled      = "stalled"       // daemon: running with no event for too long
	ReasonQuota        = "quota"         // a turn failed on a rate/usage limit
	ReasonSuperseded   = "superseded"    // migration v3: a second active run of a task
)

// Launch failure causes, recorded as data.cause on the run.status event that ends a
// launch_failed run. Only CauseHarness makes the daemon fall back to another profile.
const (
	CauseHarness   = "harness"   // adapter, harness server/sidecar, or the harness died during startup
	CauseWorkspace = "workspace" // git worktree
	CauseTmux      = "tmux"
	// CauseInterrupted: the launching process (CLI or daemon) died before the pane was
	// recorded; the daemon ends such runs after its launch_timeout.
	CauseInterrupted = "interrupted"
)

// Live statuses: the harness may be working.
func live(status string) bool {
	return status == Starting || status == Idle || status == Running || status == Waiting
}

// Run is a run with its current session and workspace.
type Run struct {
	ID              string         `json:"id"`
	Project         string         `json:"project"`
	ProjectID       int64          `json:"-"`
	TaskID          string         `json:"task_id"`
	TaskStatus      string         `json:"task_status,omitempty"`
	TaskTitle       string         `json:"task_title,omitempty"`
	Role            string         `json:"role"`
	Attempt         int            `json:"attempt"`
	RetryOf         string         `json:"retry_of,omitempty"`
	FallbackOrigin  string         `json:"fallback_origin,omitempty"` // profile whose fallback list started this run (daemon)
	Profile         string         `json:"profile"`
	ProfileID       int64          `json:"-"`
	RevisionID      int64          `json:"profile_revision_id"`
	Account         string         `json:"account"`
	Harness         string         `json:"harness"`
	Model           string         `json:"model"`
	Effort          string         `json:"effort"`
	Guard           string         `json:"guard"` // the guard level the run started with (light|strict)
	Status          string         `json:"status"`
	EndedReason     string         `json:"ended_reason,omitempty"`
	Error           string         `json:"error,omitempty"`
	Version         int64          `json:"version"`
	QueuedAt        string         `json:"queued_at"`
	StartedAt       string         `json:"started_at,omitempty"`
	FirstActivityAt string         `json:"first_activity_at,omitempty"`
	LastProgressAt  string         `json:"last_progress_at,omitempty"`
	HeartbeatAt     string         `json:"heartbeat_at,omitempty"`
	EndedAt         string         `json:"ended_at,omitempty"`
	CleanupPending  bool           `json:"cleanup_pending,omitempty"` // ended, windows/processes not confirmed gone yet
	CleanupError    string         `json:"cleanup_error,omitempty"`   // the last failed cleanup
	Session         *Session       `json:"session,omitempty"`
	Workspace       *Workspace     `json:"workspace,omitempty"`
	Live            *tmux.Liveness `json:"live,omitempty"`
	Usage           *UsageTotal    `json:"usage,omitempty"`
	Outputs         []Output       `json:"outputs,omitempty"`
	Events          []EventRow     `json:"events,omitempty"`
	// AuxDown is the first server or sidecar window found dead while the main pane
	// lives (CheckLive).
	AuxDown *AuxDown `json:"aux_down,omitempty"`
}

// Dead reports a run that should be working but whose pane is not alive.
func (r *Run) Dead() bool {
	return live(r.Status) && r.Live != nil && r.Live.State != tmux.Alive && r.Live.State != tmux.Unknown
}

// Session is the latest harness session of a run.
type Session struct {
	ID               int64     `json:"id"`
	HarnessSessionID string    `json:"harness_session_id,omitempty"`
	Status           string    `json:"status"`
	TmuxSession      string    `json:"tmux_session,omitempty"`
	TmuxWindow       string    `json:"tmux_window,omitempty"`
	TmuxPane         string    `json:"tmux_pane,omitempty"`
	PID              int       `json:"pid,omitempty"`
	PIDStartedAt     string    `json:"pid_started_at,omitempty"` // the process's birth (since v5)
	AuxPanes         []AuxPane `json:"aux_panes,omitempty"`
	ServerURL        string    `json:"server_url,omitempty"`
	TranscriptPath   string    `json:"transcript_path,omitempty"`
	StartedAt        string    `json:"started_at"`
	EndedAt          string    `json:"ended_at,omitempty"`
	EndReason        string    `json:"end_reason,omitempty"`
}

// AuxPane is a server or sidecar window started for a run.
type AuxPane struct {
	Role string `json:"role"` // server | sidecar
	Name string `json:"name"`
	Pane string `json:"pane"`
	PID  int    `json:"pid"`
	// Birth is the start time of PID (proc.TimeFormat), "" when unknown; with PID it
	// identifies the process even after its window is gone.
	Birth string `json:"birth,omitempty"`
}

// AuxDown is a dead server or sidecar window of a run.
type AuxDown struct {
	Role string        `json:"role"`
	Name string        `json:"name"`
	Live tmux.Liveness `json:"live"`
}

// Workspace is the latest workspace row of a run.
type Workspace struct {
	ID      int64  `json:"id"`
	Mode    string `json:"mode"`
	Path    string `json:"path"`
	Branch  string `json:"branch,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	BaseSHA string `json:"base_sha,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	Status  string `json:"status"`
}

// UsageTotal sums run_usage.
type UsageTotal struct {
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	Rows             int      `json:"rows"`
}

// Output is a run_outputs row.
type Output struct {
	Kind      string          `json:"kind"`
	Value     string          `json:"value"`
	Data      json.RawMessage `json:"data"`
	CreatedAt string          `json:"created_at"`
}

// EventRow is an events row of a run.
type EventRow struct {
	ID         int64           `json:"id"`
	Source     string          `json:"source"`
	Kind       string          `json:"kind"`
	FromStatus string          `json:"from_status,omitempty"`
	ToStatus   string          `json:"to_status,omitempty"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  string          `json:"created_at"`
}

const selectRun = `
SELECT r.id, pr.name, r.project_id, coalesce(r.task_id, ''), coalesce(t.status, ''), coalesce(t.title, ''),
       r.role, r.attempt, coalesce(r.retry_of_run_id, ''), p.name, r.profile_id, r.profile_revision_id,
       a.name, r.harness, pv.model, pv.effort, r.status, coalesce(r.ended_reason, ''), coalesce(r.error, ''),
       r.version, r.queued_at, coalesce(r.started_at, ''), coalesce(r.first_activity_at, ''),
       coalesce(r.last_progress_at, ''), coalesce(r.heartbeat_at, ''), coalesce(r.ended_at, ''),
       s.id, coalesce(s.harness_session_id, ''), coalesce(s.status, ''), coalesce(s.tmux_session, ''),
       coalesce(s.tmux_window, ''), coalesce(s.tmux_pane, ''), coalesce(s.pid, 0), coalesce(s.aux_panes, '[]'),
       coalesce(s.server_url, ''), coalesce(s.transcript_path, ''), coalesce(s.started_at, ''),
       coalesce(s.ended_at, ''), coalesce(s.end_reason, ''),
       w.id, coalesce(w.mode, ''), coalesce(w.path, ''), coalesce(w.branch, ''), coalesce(w.base_ref, ''),
       coalesce(w.base_sha, ''), coalesce(w.head_sha, ''), coalesce(w.status, ''), coalesce(fo.name, ''),
       r.cleanup_pending, coalesce(r.cleanup_error, ''), coalesce(s.pid_started_at, ''), coalesce(r.guard, 'strict')
FROM runs r
JOIN projects pr ON pr.id = r.project_id
JOIN profiles p ON p.id = r.profile_id
JOIN profile_revisions pv ON pv.id = r.profile_revision_id
JOIN accounts a ON a.id = r.account_id
LEFT JOIN tasks t ON t.id = r.task_id
LEFT JOIN sessions s ON s.id = (SELECT max(id) FROM sessions WHERE run_id = r.id)
LEFT JOIN workspaces w ON w.id = (SELECT max(id) FROM workspaces WHERE run_id = r.id)
LEFT JOIN profiles fo ON fo.id = r.fallback_origin_profile_id`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var s Session
	var w Workspace
	var sid, wid sql.NullInt64
	var aux string
	err := sc.Scan(&r.ID, &r.Project, &r.ProjectID, &r.TaskID, &r.TaskStatus, &r.TaskTitle,
		&r.Role, &r.Attempt, &r.RetryOf, &r.Profile, &r.ProfileID, &r.RevisionID,
		&r.Account, &r.Harness, &r.Model, &r.Effort, &r.Status, &r.EndedReason, &r.Error,
		&r.Version, &r.QueuedAt, &r.StartedAt, &r.FirstActivityAt, &r.LastProgressAt, &r.HeartbeatAt, &r.EndedAt,
		&sid, &s.HarnessSessionID, &s.Status, &s.TmuxSession, &s.TmuxWindow, &s.TmuxPane, &s.PID, &aux,
		&s.ServerURL, &s.TranscriptPath, &s.StartedAt, &s.EndedAt, &s.EndReason,
		&wid, &w.Mode, &w.Path, &w.Branch, &w.BaseRef, &w.BaseSHA, &w.HeadSHA, &w.Status, &r.FallbackOrigin,
		&r.CleanupPending, &r.CleanupError, &s.PIDStartedAt, &r.Guard)

	if err != nil {
		return nil, err
	}
	if sid.Valid {
		s.ID = sid.Int64
		_ = json.Unmarshal([]byte(aux), &s.AuxPanes)
		r.Session = &s
	}
	if wid.Valid {
		w.ID = wid.Int64
		r.Workspace = &w
	}
	return &r, nil
}

// Get returns a run by id.
func Get(ctx context.Context, q db.Querier, id string) (*Run, error) {
	r, err := scanRun(q.QueryRowContext(ctx, selectRun+" WHERE r.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.NotFound, "run %s not found", id)
	}
	return r, err
}

// Resolve accepts a run id or a task id (its current, else latest, run).
func Resolve(ctx context.Context, q db.Querier, ref string) (*Run, error) {
	r, err := Get(ctx, q, ref)
	if apperr.Code(err) != apperr.NotFound {
		return r, err
	}
	var id string
	err = q.QueryRowContext(ctx, `
SELECT coalesce(t.current_run_id, (SELECT id FROM runs WHERE task_id = t.id ORDER BY attempt DESC LIMIT 1), '')
FROM tasks t WHERE t.id = ?`, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && id == "") {
		return nil, apperr.New(apperr.NotFound, "no run or task with runs named %s", ref)
	}
	if err != nil {
		return nil, err
	}
	return Get(ctx, q, id)
}

// Filter selects runs for List.
type Filter struct {
	ProjectID int64 // 0 = all
	TaskID    string
	Active    bool // not ended
	Limit     int  // 0 = 50
}

// List returns runs, newest first.
func List(ctx context.Context, q db.Querier, f Filter) ([]Run, error) {
	query := selectRun + " WHERE 1 = 1"
	var args []any
	if f.ProjectID != 0 {
		query += " AND r.project_id = ?"
		args = append(args, f.ProjectID)
	}
	if f.TaskID != "" {
		query += " AND r.task_id = ?"
		args = append(args, f.TaskID)
	}
	if f.Active {
		query += " AND r.status <> 'ended'"
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	query += " ORDER BY r.queued_at DESC, r.id DESC LIMIT ?"
	args = append(args, f.Limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// Details fills usage totals, outputs and the last n events of r.
func Details(ctx context.Context, q db.Querier, r *Run, n int) error {
	var u UsageTotal
	var cost sql.NullFloat64
	err := q.QueryRowContext(ctx, `
SELECT count(*), coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0), coalesce(sum(cache_read_tokens), 0),
       coalesce(sum(cache_write_tokens), 0), coalesce(sum(reasoning_tokens), 0), sum(cost_usd)
FROM run_usage WHERE run_id = ?`, r.ID).Scan(&u.Rows, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens,
		&u.CacheWriteTokens, &u.ReasoningTokens, &cost)
	if err != nil {
		return err
	}
	if cost.Valid {
		u.CostUSD = &cost.Float64
	}
	r.Usage = &u

	rows, err := q.QueryContext(ctx, `SELECT kind, value, data, created_at FROM run_outputs WHERE run_id = ? ORDER BY id`, r.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var o Output
		var data string
		if err := rows.Scan(&o.Kind, &o.Value, &data, &o.CreatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		o.Data = json.RawMessage(data)
		r.Outputs = append(r.Outputs, o)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = q.QueryContext(ctx, `
SELECT id, source, kind, coalesce(from_status, ''), coalesce(to_status, ''), data, created_at
FROM (SELECT * FROM events WHERE run_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, r.ID, n)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e EventRow
		var data string
		if err := rows.Scan(&e.ID, &e.Source, &e.Kind, &e.FromStatus, &e.ToStatus, &data, &e.CreatedAt); err != nil {
			return err
		}
		e.Data = json.RawMessage(data)
		r.Events = append(r.Events, e)
	}
	return rows.Err()
}

// CheckLive fills Live for every run that has a pane, with one tmux call, and AuxDown
// for a live run whose main pane is alive but one of its server or sidecar windows is
// not (exited or gone; a check that cannot tell is not "down").
func CheckLive(ctx context.Context, tc *tmux.Client, runs []*Run) {
	var panes map[string]tmux.PaneInfo
	var perr error
	for _, r := range runs {
		if r.Session == nil || r.Session.TmuxPane == "" {
			l := tmux.Liveness{State: tmux.Unknown, Detail: "no pane recorded"}
			r.Live = &l
			continue
		}
		if panes == nil && perr == nil {
			if !tc.Available() {
				perr = tmux.ErrNoTmux
			} else {
				panes, perr = tc.Panes(ctx)
			}
		}
		var l tmux.Liveness
		if perr != nil {
			l = tmux.Liveness{State: tmux.Unknown, Pane: r.Session.TmuxPane, Detail: perr.Error()}
		} else {
			l = tmux.CheckIn(panes, r.Session.TmuxPane, r.Session.PID)
		}
		r.Live = &l
		if l.State != tmux.Alive || !live(r.Status) {
			continue
		}
		for _, a := range r.Session.AuxPanes {
			if al := tmux.CheckIn(panes, a.Pane, a.PID); al.State == tmux.Exited || al.State == tmux.Gone {
				r.AuxDown = &AuxDown{Role: a.Role, Name: a.Name, Live: al}
				break
			}
		}
	}
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func jsonList(v any) string {
	b, _ := json.Marshal(v)
	if s := string(b); s != "null" {
		return s
	}
	return "[]"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
