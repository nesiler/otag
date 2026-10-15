package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/workspace"
)

// CoordinatorHarnesses are the harnesses a coordinator may run on (v1: Claude only).
var CoordinatorHarnesses = []string{"claude"}

// CoordInput selects the coordinator to start.
type CoordInput struct {
	Project string
	Profile string // "" = the project's coordinator_profile, else its default_profile
}

// LiveCoordinator returns the project's coordinator run that has not ended ("" when
// there is none).
func LiveCoordinator(ctx context.Context, q db.Querier, projectID int64) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM runs WHERE project_id = ? AND role = 'coordinator' AND status <> 'ended'
		ORDER BY queued_at DESC LIMIT 1`, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// StartCoordinator starts the project's coordinator: a task-less run (role
// coordinator) whose workspace is the project checkout itself. A project has at most
// one coordinator that has not ended (CONFLICT; also a unique index). While it runs,
// the checkout is its own: same-mode children are refused (sameWorkspaceFree), and
// it cannot start while one works there. Messages to coord that wait unrouted go into
// its first prompt.
func (m *Manager) StartCoordinator(ctx context.Context, in CoordInput) (*StartResult, error) {
	if !m.Tmux.Available() {
		return nil, apperr.New(apperr.Unavailable, "tmux is required to start runs")
	}
	// An earlier coordinator's leftovers must be gone before a new one uses the checkout.
	rows, err := m.DB.QueryContext(ctx, `SELECT r.id FROM runs r JOIN projects p ON p.id = r.project_id
		WHERE p.name = ? AND r.role = 'coordinator' AND r.status = 'ended' AND r.cleanup_pending = 1`, in.Project)
	if err != nil {
		return nil, err
	}
	var pending []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		pending = append(pending, id)
	}
	_ = rows.Close()
	for _, id := range pending {
		if err := m.Quiesce(ctx, id); err != nil {
			return nil, apperr.Retryable(apperr.Unavailable, "coordinator of %s: %v", in.Project, err)
		}
	}
	p, ad, err := m.createCoordinator(ctx, in)
	if err != nil {
		return nil, err
	}
	return m.launchOrFail(ctx, p, ad)
}

func (m *Manager) createCoordinator(ctx context.Context, in CoordInput) (*plan, adapter.Adapter, error) {
	var p plan
	var ad adapter.Adapter
	err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		p = plan{}
		var projectID int64
		var prefix string
		err := tx.QueryRowContext(ctx, `SELECT id, prefix FROM projects WHERE name = ? AND retired_at IS NULL`, in.Project).
			Scan(&projectID, &prefix)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.NotFound, "project %s not found", in.Project)
		}
		if err != nil {
			return err
		}
		proj, err := m.loadProject(ctx, tx, projectID, &p)
		if err != nil {
			return err
		}
		if live, err := LiveCoordinator(ctx, tx, projectID); err != nil {
			return err
		} else if live != "" {
			return apperr.New(apperr.Conflict, "project %s already has coordinator run %s", in.Project, live)
		}
		profile := in.Profile
		if profile == "" {
			profile = proj.CoordinatorProfile
		}
		if profile == "" {
			profile = proj.DefaultProfile
		}
		if profile == "" {
			return apperr.New(apperr.Invalid, "no coordinator profile: pass --profile or set the project's coordinator_profile")
		}
		profileID, accountID, a, err := m.loadRevision(ctx, tx, profile, &p)
		if err != nil {
			return err
		}
		ad = a
		if !slices.Contains(CoordinatorHarnesses, p.harness) {
			return apperr.New(apperr.Invalid, "a coordinator on %s is not supported yet (profile %s); use a %s profile",
				p.harness, profile, strings.Join(CoordinatorHarnesses, " or "))
		}
		if err := sameWorkspaceFree(ctx, tx, proj.Path); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(attempt), 0) + 1 FROM runs
			WHERE project_id = ? AND role = 'coordinator'`, projectID).Scan(&p.attempt); err != nil {
			return err
		}
		p.runID = fmt.Sprintf("%s-coord-r%d", prefix, p.attempt)
		p.role, p.window, p.mode = "coordinator", "coord", workspace.Same
		var hash string
		if p.token, hash, err = newToken(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO runs (id, project_id, task_id, role, attempt, profile_id, profile_revision_id, account_id, harness,
                  status, token_hash, guard)
VALUES (?, ?, NULL, 'coordinator', ?, ?, ?, ?, ?, 'queued', ?, ?)`,
			p.runID, projectID, p.attempt, profileID, p.rev.id, accountID, p.harness, hash, p.guard.String()); err != nil {
			return err
		}
		r := &row{ID: p.runID, Harness: p.harness, Status: Queued, ProjectID: projectID, Version: 1}
		if err := r.record(ctx, tx, m.Actor, "run.created", "", Queued, map[string]any{"profile": profile,
			"harness": p.harness, "attempt": p.attempt, "role": p.role, "guard": p.guard.String()}); err != nil {
			return err
		}
		// Messages to coord that nobody took while no coordinator ran.
		p.messages, err = msg.Claim(ctx, tx, msg.ClaimInput{RunID: p.runID, Channel: adapter.ChanLaunchPrompt}, msgActor(m.Actor))
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return &p, ad, nil
}

// coordPrompts are the first message and system instructions of a coordinator.
func coordPrompts(p *plan) (prompt, instructions string) {
	var in strings.Builder
	fmt.Fprintf(&in, "You are the otag coordinator of project %s (run %s), working in %s. "+
		"You coordinate AI child agents with the `otag` CLI; children implement the tasks.\n", p.project, p.runID, p.projectPath)
	for _, l := range coordGuide() {
		fmt.Fprintf(&in, "- %s\n", l)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are now the coordinator of project %s. Start with `otag status` and `otag needs`, "+
		"then wait for instructions from the human.\n", p.project)
	if p.messages != nil {
		fmt.Fprintf(&b, "\nMessages waiting for the coordinator:\n\n%s\n", p.messages.Text(msg.DefaultMaxChars))
	}
	return b.String(), in.String()
}

// coordGuide are the lines about how a coordinator works, shared by its first
// instructions and `otag prime`.
func coordGuide() []string {
	return []string{
		"Plan work as tasks: `otag task add \"title\" --body \"...\" [--base <ref>] [--review <profile|none>]`; start one " +
			"with `otag run start <task> --no-attach` (each child works in its own git worktree on branch " +
			"<branch_prefix><task>, from the project's base unless --base says otherwise).",
		"Follow up with `otag status`, `otag needs` and `otag inbox`; answer a question with `otag reply <msg-id> \"answer\"`; " +
			"message a run with `otag send <run|task> \"text\"`.",
		"Children report with `otag done \"summary\"`; review a result with `otag review <task> --accept|--reject \"reason\"`.",
		"A done task is ready to deliver (`otag task list --deliverable`; you get a message for each). otag never merges: " +
			"deliver it as the project does, then `otag task deliver <task> [--delete-branch]` removes its worktree.",
		"Merge/pull requests: `otag task push <task> [--force]` pushes the task branch when the project's push setting " +
			"allows it; open the MR/PR with the forge's CLI (glab mr create, gh pr create). Review feedback or red CI: " +
			"`otag task reopen <task> \"reason\"` sends it back to ready on the same branch; the next run gets the reason.",
		"Never run git push or force anything yourself. Ask the human (`otag send human \"...\"`) before merging or deleting work.",
	}
}
