package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/guard"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/workspace"
)

// Manager starts and stops runs. Getenv provides the otag environment (paths and the
// OTAG_* variables passed on to children).
type Manager struct {
	DB       *db.DB
	Adapters *adapter.Registry
	Tmux     *tmux.Client
	Getenv   func(string) string
	Actor    Actor
}

// SessionName is the tmux session of a project.
func SessionName(project string) string { return "otag-" + project }

// StartInput selects what to start.
type StartInput struct {
	TaskID  string
	Profile string // "" = task profile, else project default
	// FallbackOrigin marks a run the daemon starts as a fallback: the profile whose
	// fallback list the chain walks (runs.fallback_origin_profile_id). 0 = a new chain.
	FallbackOrigin int64
	// Guard, when set, runs first inside the transaction that claims the task and
	// creates the run; an error aborts the start with nothing written. The daemon
	// checks that its fallback decision still holds there and records it, so the
	// decision and the run it started commit together.
	Guard func(tx *sql.Tx) error

	resume *resumeSpec // set by Resume: continue the parked run's harness session
}

// StartResult reports a started run.
type StartResult struct {
	Run       *Run   `json:"run"`
	Trust     string `json:"trust,omitempty"`  // accepted | timeout | ""
	FirstSeen bool   `json:"first_event_seen"` // a hook event arrived before start returned
	Attach    string `json:"attach_command"`   // shell command to attach
	Warning   string `json:"warning,omitempty"`
	// Messages are the queued deliveries the first prompt carries (launch_prompt).
	Messages []int64 `json:"messages,omitempty"`
}

type plan struct {
	runID, taskID, project, projectPath, mode, harness string
	role                                               string // child | review | coordinator
	window                                             string // tmux window name: the task id, or "coord"
	attempt                                            int
	rev                                                revision
	deny                                               []string // with the level's defaults (guard.DenyGlobs)
	guard                                              guard.Level
	reviewProfile                                      string
	title, body, extraPrompt                           string
	token                                              string // OTAG_RUN_TOKEN; the row keeps its hash
	reviewOf                                           string // a review run: the reviewed task
	// resumeSID continues the harness session of the parked run resumeOf.
	resumeSID, resumeOf string
	// messages are the queued deliveries claimed for the first prompt (launch_prompt),
	// nil when there are none; the launch settles them.
	messages *msg.Attempt
	prior    *prior // the task's previous attempt (retries, fallbacks), nil for the first run
}

type revision struct {
	Name        string           `json:"name"`
	AccountName string           `json:"account_name"`
	Profile     registry.Profile `json:"profile"`
	Account     registry.Account `json:"account"`
	id          int64
	hash        string
}

// Start claims the task (if ready), creates the run, prepares the workspace, launches
// the harness in a tmux window and returns once the pane runs (and, for adapters with
// a trust dialog, once the first hook event arrived or the startup timeout passed).
// Messages queued for the task go into the first prompt.
func (m *Manager) Start(ctx context.Context, in StartInput) (*StartResult, error) {
	if !m.Tmux.Available() {
		return nil, apperr.New(apperr.Unavailable, "tmux is required to start runs")
	}
	// Nothing of an earlier run may still work in the directory this run will use. A
	// run there that ends between this and the creation transaction is caught inside it
	// (errCleanupPending), and the cleanup runs again.
	var p *plan
	var ad adapter.Adapter
	var err error
	for try := 1; ; try++ {
		if err := m.clearWorkspace(ctx, in.TaskID); err != nil {
			return nil, apperr.Retryable(apperr.Unavailable, "task %s: %v", in.TaskID, err)
		}
		p, ad, err = m.create(ctx, in)
		if !errors.Is(err, errCleanupPending) || try == 3 {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return m.launchOrFail(ctx, p, ad)
}

// launchOrFail launches a created run. A launch that fails ends the run as
// launch_failed and releases its task.
func (m *Manager) launchOrFail(ctx context.Context, p *plan, ad adapter.Adapter) (*StartResult, error) {
	res, err := m.launch(ctx, p, ad)
	if err != nil {
		if isStopped(err) {
			return nil, err // `otag run stop` ends the run
		}
		if ferr := m.launchFailed(ctx, p.runID, err); ferr != nil {
			err = errors.Join(err, ferr)
		}
		// The launch killed its windows; this verifies their processes are gone too (a
		// failure stays on the run as cleanup_error, and the daemon retries).
		_, _ = m.Cleanup(ctx, p.runID)
		return nil, err
	}
	return res, nil
}

// loadProject reads the project of a new run into p (name, path, workspace mode, guard
// level and deny globs). A project inside otag's worktree root (synced before sync
// refused it) is refused: a run there could work in another task's worktree.
func (m *Manager) loadProject(ctx context.Context, tx *sql.Tx, projectID int64, p *plan) (*registry.ProjectRow, error) {
	var proj registry.ProjectRow
	var deny string
	err := tx.QueryRowContext(ctx, `
SELECT pr.name, pr.path, pr.workspace_mode, pr.deny, pr.guard, coalesce(c.name, ''), coalesce(d.name, ''), coalesce(rv.name, '')
FROM projects pr LEFT JOIN profiles d ON d.id = pr.default_profile_id LEFT JOIN profiles rv ON rv.id = pr.review_profile_id
LEFT JOIN profiles c ON c.id = pr.coordinator_profile_id
WHERE pr.id = ? AND pr.retired_at IS NULL`, projectID).Scan(&proj.Name, &proj.Path, &proj.WorkspaceMode, &deny,
		&proj.Guard, &proj.CoordinatorProfile, &proj.DefaultProfile, &proj.ReviewProfile)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.InvalidTransition, "project %d is retired", projectID)
	}
	if err != nil {
		return nil, err
	}
	if root := paths.WorktreeRoot(m.Getenv); paths.Within(proj.Path, root) {
		return nil, apperr.New(apperr.Invalid, "project %s: path %s is inside otag's worktree root %s; move it", proj.Name,
			proj.Path, root)
	}
	proj.ID = projectID
	_ = json.Unmarshal([]byte(deny), &p.deny)
	p.guard = guard.Level(proj.Guard)
	p.deny = guard.DenyGlobs(p.guard, p.deny)
	p.project, p.projectPath, p.mode, p.reviewProfile = proj.Name, proj.Path, proj.WorkspaceMode, proj.ReviewProfile
	return &proj, nil
}

// loadRevision reads the current revision of profile into p and returns the ids the
// run row needs and the harness adapter.
func (m *Manager) loadRevision(ctx context.Context, tx *sql.Tx, profile string, p *plan) (profileID, accountID int64, ad adapter.Adapter, err error) {
	if profile == "" {
		return 0, 0, nil, apperr.New(apperr.Invalid, "no profile: pass --profile, set the task profile or the project's default_profile")
	}
	var conf string
	err = tx.QueryRowContext(ctx, `
SELECT p.id, pv.id, pv.account_id, pv.harness, pv.config_hash, pv.config
FROM profiles p JOIN profile_revisions pv ON pv.profile_id = p.id AND pv.config_hash = p.config_hash
WHERE p.name = ? AND p.retired_at IS NULL`, profile).Scan(&profileID, &p.rev.id, &accountID, &p.harness, &p.rev.hash, &conf)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil, apperr.New(apperr.NotFound, "profile %s not found (run `otag sync`?)", profile)
	}
	if err != nil {
		return 0, 0, nil, err
	}
	if err := json.Unmarshal([]byte(conf), &p.rev); err != nil {
		return 0, 0, nil, fmt.Errorf("profile %s revision: %w", profile, err)
	}
	var ok bool
	if ad, ok = m.Adapters.Get(p.harness); !ok {
		return 0, 0, nil, apperr.New(apperr.Invalid, "no adapter for harness %s yet (available: %s)",
			p.harness, strings.Join(m.Adapters.Names(), ", "))
	}
	return profileID, accountID, ad, nil
}

// create claims the task and inserts the queued run in one transaction, together with
// the claim of the messages its first prompt carries.
func (m *Manager) create(ctx context.Context, in StartInput) (*plan, adapter.Adapter, error) {
	var p plan
	var ad adapter.Adapter
	err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		p = plan{}
		if in.Guard != nil {
			if err := in.Guard(tx); err != nil {
				return err
			}
		}
		t, err := task.Get(ctx, tx, in.TaskID)
		if err != nil {
			return err
		}
		proj, err := m.loadProject(ctx, tx, t.ProjectID, &p)
		if err != nil {
			return err
		}
		rs := in.resume
		if rs != nil {
			// The parked run fixes the profile; resumable() checks the rest below.
			if err := rs.load(ctx, tx, t); err != nil {
				return err
			}
			in.Profile = rs.prev.Profile
		}
		profile := in.Profile
		if profile == "" {
			profile = t.Profile
		}
		if profile == "" {
			profile = proj.DefaultProfile
		}
		profileID, accountID, a, err := m.loadRevision(ctx, tx, profile, &p)
		if err != nil {
			return err
		}
		ad = a

		switch {
		case t.Status == task.Ready && rs == nil:
			if _, err := task.Claim(ctx, tx, t.ID, 0, m.Actor.task("")); err != nil {
				return err
			}
		case t.Status == task.Claimed && rs == nil:
		case t.Status == task.Waiting && rs == nil:
			return apperr.New(apperr.InvalidTransition, "task %s is waiting for an answer; it resumes when the answer arrives "+
				"(or: otag run resume %s)", t.ID, t.ID)
		case rs != nil:
			// resumable checks the task is waiting on its parked run.
		default:
			return apperr.New(apperr.InvalidTransition, "task %s is %s; only ready or claimed tasks can start a run", t.ID, t.Status)
		}
		// One run per task at a time (also a unique index): a claimed task may still have
		// a run that is being launched.
		var active string
		err = tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE task_id = ? AND status <> 'ended' LIMIT 1`, t.ID).Scan(&active)
		if err == nil {
			return apperr.New(apperr.Conflict, "task %s already has run %s that has not ended", t.ID, active)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if rs != nil {
			if err := m.resumable(ctx, tx, rs, &p, t, profileID); err != nil {
				return err
			}
			if _, err := task.ClaimWaiting(ctx, tx, t.ID, 0, m.Actor.task("")); err != nil {
				return err
			}
		}
		if proj.WorkspaceMode == workspace.Same {
			if err := sameWorkspaceFree(ctx, tx, proj.Path); err != nil {
				return err
			}
		}
		pending, err := workspaceRuns(ctx, tx, t.ID, proj.WorkspaceMode, proj.Path, nil)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return fmt.Errorf("%w (run %s)", errCleanupPending, pending[0])
		}

		var prev sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(attempt), 0) + 1,
			(SELECT id FROM runs WHERE task_id = ? ORDER BY attempt DESC LIMIT 1) FROM runs WHERE task_id = ?`,
			t.ID, t.ID).Scan(&p.attempt, &prev); err != nil {
			return err
		}
		p.runID = fmt.Sprintf("%s-r%d", t.ID, p.attempt)
		p.taskID, p.window = t.ID, t.ID
		p.title, p.body, p.extraPrompt = t.Title, t.Body, t.ExtraPrompt
		p.role = "child"
		for _, d := range t.Deps {
			if d.Kind == "review_of" {
				p.role, p.reviewOf = "review", d.TaskID
			}
		}
		if rs != nil {
			p.role, p.resumeSID, p.resumeOf = rs.prev.Role, rs.prev.Session.HarnessSessionID, rs.prev.ID
		}
		var hash string
		if p.token, hash, err = newToken(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO runs (id, project_id, task_id, role, retry_of_run_id, attempt, profile_id, profile_revision_id,
                  account_id, harness, status, fallback_origin_profile_id, token_hash, guard)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)`,
			p.runID, t.ProjectID, t.ID, p.role, prev, p.attempt, profileID, p.rev.id, accountID, p.harness,
			sql.NullInt64{Int64: in.FallbackOrigin, Valid: in.FallbackOrigin != 0}, hash, p.guard.String()); err != nil {
			return err
		}
		if err := task.SetCurrentRun(ctx, tx, t.ID, p.runID); err != nil {
			return err
		}
		r := &row{ID: p.runID, TaskID: t.ID, Harness: p.harness, Status: Queued, ProjectID: t.ProjectID, Version: 1}
		data := map[string]any{"profile": profile, "harness": p.harness, "attempt": p.attempt, "retry_of": prev.String,
			"guard": p.guard.String(), "role": p.role}
		if in.FallbackOrigin != 0 {
			data["fallback_origin_profile_id"] = in.FallbackOrigin
		}
		if rs != nil {
			data["resume_of"] = rs.prev.ID
		}
		if err := r.record(ctx, tx, m.Actor, "run.created", "", Queued, data); err != nil {
			return err
		}
		// Messages queued for the task (and for its earlier runs) go into the first
		// prompt. A resumed run takes the answer it waited for, and only it: without that
		// answer it must not start (the task would run on without it).
		claim := msg.ClaimInput{RunID: p.runID, Channel: adapter.ChanLaunchPrompt}
		if rs != nil {
			claim.IDs = rs.answer
		}
		if p.messages, err = msg.Claim(ctx, tx, claim, msgActor(m.Actor)); err != nil {
			return err
		}
		if rs != nil && !carries(p.messages, rs.answer) {
			return blocked(t.ID, false, "the answer could not be claimed for the new run")
		}
		if rs == nil && prev.Valid {
			if p.prior, err = loadPrior(ctx, tx, prev.String); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return &p, ad, nil
}

// errCleanupPending: an earlier run of the workspace ended after Start cleared it, and
// its cleanup has not been confirmed yet.
var errCleanupPending = apperr.Retryable(apperr.Unavailable, "an earlier run of this workspace still has a cleanup pending")

// sameWorkspaceFree rejects a run in a project directory (workspace mode "same") that
// an active run of any project works in: agents sharing one checkout overwrite each
// other's changes, so such a directory takes one active run at a time. Directories
// compare by file identity (sameDir), so case and symlink aliases do not get around
// it. Runs that have no workspace row yet count with their project's directory when
// that project is in same mode, or when they are its coordinator (which works in the
// project checkout itself, so a live coordinator keeps same-mode children out).
func sameWorkspaceFree(ctx context.Context, tx *sql.Tx, path string) error {
	rows, err := tx.QueryContext(ctx, `
SELECT r.id, r.role, coalesce(w.path, CASE WHEN p.workspace_mode = 'same' OR r.role = 'coordinator' THEN p.path END, '')
FROM runs r JOIN projects p ON p.id = r.project_id
LEFT JOIN workspaces w ON w.id = (SELECT max(id) FROM workspaces WHERE run_id = r.id)
WHERE r.status <> 'ended'`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, role, p string
		if err := rows.Scan(&id, &role, &p); err != nil {
			return err
		}
		if !sameDir(p, path) {
			continue
		}
		if role == "coordinator" {
			return apperr.Retryable(apperr.Conflict, "workspace %s is the checkout of coordinator run %s; while it runs, "+
				"children work in worktrees (workspace_mode \"worktree\")", path, id)
		}
		return apperr.Retryable(apperr.Conflict, "workspace %s is in use by run %s (workspace_mode \"same\": one active run per directory)", path, id)
	}
	return rows.Err()
}

// Trust keys typed while a TUI is still starting can be dropped (Claude Code shows its
// dialog a moment before it reads input), so they are sent again while the dialog stays
// on screen, at most every trustResend and trustSends times in all. Once a capture after
// a send no longer matches, the dialog is gone and nothing is sent any more.
const (
	trustResend = time.Second
	trustSends  = 5
)

// watchStartup polls the pane until a hook event newer than the launch (event id
// `after`) arrives, accepting a trust dialog (see trustResend).
func (m *Manager) watchStartup(ctx context.Context, runID string, after int64, pane string, pid int, th adapter.TrustHandler, timeout time.Duration) (trust string, seen bool) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctl := m.Tmux.Control(pane)
	deadline := time.Now().Add(timeout)
	var sent time.Time
	sends := 0
	for time.Now().Before(deadline) {
		var n int
		_ = m.DB.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE run_id = ? AND id > ? AND kind LIKE 'hook.%'`,
			runID, after).Scan(&n)
		if n > 0 {
			return trust, true
		}
		if l := m.Tmux.Check(ctx, pane, pid); l.State != tmux.Alive {
			return trust, false
		}
		if sends < trustSends {
			if screen, err := ctl.Capture(ctx); err == nil {
				keys, ok := th.AcceptTrust(screen)
				switch {
				case !ok && sends > 0:
					sends = trustSends // the dialog is gone: never answer anything else
				case ok && time.Since(sent) >= trustResend:
					if err := ctl.SendKeys(ctx, keys...); err == nil {
						trust, sent = "accepted", time.Now()
						sends++
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return trust, false
		case <-time.After(100 * time.Millisecond):
		}
	}
	if trust == "" {
		trust = "timeout"
	}
	return trust, false
}

// launchFailed ends a run that could not be launched and releases its task.
func (m *Manager) launchFailed(ctx context.Context, runID string, cause error) error {
	ctx = context.WithoutCancel(ctx)
	return m.DB.Tx(ctx, func(tx *sql.Tx) error {
		r, err := load(ctx, tx, runID)
		if err != nil || r.Status == Ended {
			return err
		}
		if err := r.endSessions(ctx, tx, ReasonLaunchFailed); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = 'failed' WHERE run_id = ?`, runID); err != nil {
			return err
		}
		var extra map[string]any
		if c := causeOf(cause); c != "" {
			extra = map[string]any{"cause": c}
		}
		if err := r.end(ctx, tx, m.Actor, ReasonLaunchFailed, oneLine(cause.Error()), extra); err != nil {
			return err
		}
		if err := requeueLaunchPrompt(ctx, tx, runID, "launch failed", m.Actor); err != nil {
			return err
		}
		return r.releaseTask(ctx, tx, m.Actor, "run "+runID+" failed to launch")
	})
}

func (m *Manager) runSpec(p *plan, ws workspace.Result) adapter.RunSpec {
	getenv := m.Getenv
	otagBin := paths.OtagBin(getenv)
	// Absolute paths, so a child in another cwd talks to the same database and dirs.
	env := map[string]string{
		"OTAG_RUN_ID": p.runID, TokenEnv: p.token, "OTAG_PROJECT": p.project, "OTAG_TASK_ID": p.taskID, "OTAG_BIN": otagBin,
		"OTAG_DB": paths.DB(getenv), "OTAG_CONFIG_DIR": paths.ConfigDir(getenv), "OTAG_DATA_DIR": paths.DataDir(getenv),
		"OTAG_TMUX_SOCKET": paths.TmuxSocket(getenv),
	}
	if getenv("OTAG_TMUX_CONF") != "" {
		env["OTAG_TMUX_CONF"] = paths.TmuxConf(getenv)
	}
	// The base variables and the profile's env_passthrough are part of the run's env,
	// so adapters see them too: a passed-through credential (an OAuth token, an API
	// key) counts as set and survives their scrub lists.
	// The account's and profile's own env still win.
	for k, v := range m.runEnv(p) {
		_, otag := env[k]
		_, acc := p.rev.Account.Env[k]
		_, prof := p.rev.Profile.Env[k]
		if !otag && !acc && !prof {
			env[k] = v
		}
	}
	acc, prof := p.rev.Account, p.rev.Profile
	spec := adapter.RunSpec{
		RunID: p.runID, TaskID: p.taskID, Project: p.project, Role: p.role, Attempt: p.attempt,
		Harness: p.harness,
		Account: adapter.Account{Name: p.rev.AccountName, ConfigDir: acc.ConfigDir, Command: acc.Command, Env: acc.Env},
		Profile: adapter.Profile{Name: p.rev.Name, Model: prof.Model, Effort: prof.Effort, Mode: prof.Mode,
			Env: prof.Env, Args: prof.Args, RevisionID: p.rev.id, ConfigHash: p.rev.hash},
		Workdir: ws.Path, ProjectPath: p.projectPath, Branch: ws.Branch,
		RunDir: paths.RunDir(getenv, p.runID), CacheDir: paths.CacheDir(getenv, p.harness),
		OtagBin: otagBin, Env: env, Deny: p.deny, Guard: p.guard, ResumeSessionID: p.resumeSID,
	}
	spec.Prompt, spec.Instructions = prompts(p, ws)
	return spec
}

// baseEnv are the variables of otag's own environment every run gets; a trailing *
// matches a prefix. In a strict run everything else the tmux server or the launching
// shell holds (GITHUB_TOKEN, AWS_*, other providers' keys, ...) stays out: run windows
// start from a clean environment (tmux.Window.CleanEnv) with only these, the profile's
// env_passthrough, the account and profile env, the adapter's and the OTAG_* variables.
// A light run also inherits the rest of otag's environment except secrets
// (inheritedEnv).
var baseEnv = []string{"HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMUX_TMPDIR", "LANG", "LC_*"}

// inheritedEnv returns, for a light run, otag's environment without the variables
// runEnv already gives, otag's own (OTAG_*, PATH), the pane's (TMUX, TMUX_PANE) and
// secrets (secretName). They go on the window only, not into RunSpec.Env: the
// adapters' scrub lists (an inherited ANTHROPIC_BASE_URL, CODEX_HOME, ...) still apply
// to them, which keeps the account and profile of the run what they say.
func (m *Manager) inheritedEnv(p *plan) map[string]string {
	env := map[string]string{}
	if !p.guard.IsLight() {
		return env
	}
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || k == "PATH" || k == "TMUX" || k == "TMUX_PANE" || strings.HasPrefix(k, "OTAG_") || secretName(k) {
			continue
		}
		if v := m.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// secretName reports whether an environment variable name looks like it holds a
// credential: *_TOKEN, *_SECRET, *_KEY, *_PASSWORD, *PASSWD*, AWS_*, ... A light run
// inherits otag's environment without these (the profile's env_passthrough brings
// back the ones a harness needs).
func secretName(k string) bool {
	k = strings.ToUpper(k)
	for _, s := range []string{"_TOKEN", "_TOKENS", "_SECRET", "_SECRETS", "_KEY", "_KEYS", "_PASSWORD", "_PASS",
		"_PASSPHRASE", "_CREDENTIAL", "_CREDENTIALS", "_AUTH", "_COOKIE"} {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	for _, s := range []string{"PASSWD", "PASSWORD", "SECRET", "PRIVATE_KEY", "API_KEY", "ACCESS_KEY", "AUTH_TOKEN"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	for _, s := range []string{"AWS_", "AZURE_", "GOOGLE_APPLICATION_CREDENTIALS", "OP_SERVICE_ACCOUNT", "VAULT_"} {
		if strings.HasPrefix(k, s) {
			return true
		}
	}
	switch k {
	case "TOKEN", "SECRET", "PASSWORD", "GH_TOKEN", "GITHUB_TOKEN", "GITLAB_TOKEN", "NPM_TOKEN", "HF_TOKEN",
		"DATABASE_URL":
		return true
	}
	return false
}

// runEnv returns baseEnv and the profile's env_passthrough with their values in otag's
// environment (unset ones are left out).
func (m *Manager) runEnv(p *plan) map[string]string {
	env := map[string]string{}
	var names []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			names = append(names, k)
		}
	}
	for _, pat := range append(slices.Clone(baseEnv), p.rev.Profile.EnvPassthrough...) {
		keys := []string{pat}
		if prefix, ok := strings.CutSuffix(pat, "*"); ok {
			keys = nil
			for _, k := range names {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, k)
				}
			}
		}
		for _, k := range keys {
			if v := m.Getenv(k); v != "" && k != "PATH" && !strings.HasPrefix(k, "OTAG_") {
				env[k] = v
			}
		}
	}
	return env
}

// writeFiles writes adapter files under dir through an os.Root, so neither ".." nor a
// symlink planted in the run dir can redirect a write outside it. An existing file is
// replaced, never written through.
func writeFiles(dir string, files []adapter.File) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, f := range files {
		name := filepath.Clean(f.Path)
		if filepath.IsAbs(f.Path) || name == "." || strings.HasPrefix(name, "..") {
			return apperr.New(apperr.Internal, "adapter file %q escapes the run dir", f.Path)
		}
		if d := filepath.Dir(name); d != "." {
			if err := root.MkdirAll(d, 0o700); err != nil {
				return apperr.New(apperr.Internal, "adapter file %q: %v", f.Path, err)
			}
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o600
		}
		fh, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return apperr.New(apperr.Internal, "adapter file %q: %v", f.Path, err)
		}
		_, werr := fh.Write(f.Data)
		cerr := fh.Chmod(mode) // the umask may have narrowed it
		if err := errors.Join(werr, cerr, fh.Close()); err != nil {
			return err
		}
	}
	return nil
}

// waitReady polls a readiness probe (tcp:host:port, unix:/path, file:/path) until it
// succeeds, the timeout passes or abort returns an error.
func waitReady(ctx context.Context, probe string, timeout time.Duration, abort func() error) error {
	if probe == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	kind, addr, ok := strings.Cut(probe, ":")
	if !ok {
		return fmt.Errorf("bad readiness probe %q", probe)
	}
	check := func() bool {
		switch kind {
		case "file":
			_, err := os.Stat(addr)
			return err == nil
		case "tcp", "unix":
			c, err := net.DialTimeout(kind, addr, 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
			}
			return err == nil
		}
		return false
	}
	if kind != "file" && kind != "tcp" && kind != "unix" {
		return fmt.Errorf("bad readiness probe %q", probe)
	}
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s (%s)", timeout, probe)
		}
		if abort != nil {
			if err := abort(); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}
