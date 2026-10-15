// Package identity resolves which project and run a CLI invocation acts for.
//
// Precedence: --project/--run flags, then OTAG_PROJECT/OTAG_RUN_ID, then the
// working directory matched against registered project paths.
package identity

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/registry"
)

// Input carries the raw identity hints.
type Input struct {
	ProjectFlag string
	RunFlag     string
	EnvProject  string
	EnvRun      string
	Cwd         string
}

// Identity is the resolved identity. Project may be nil when nothing matched.
type Identity struct {
	Project       *registry.ProjectRow `json:"project"`
	ProjectSource string               `json:"project_source,omitempty"` // flag | env | run | cwd
	RunID         string               `json:"run_id,omitempty"`
	RunSource     string               `json:"run_source,omitempty"` // flag | env
}

// RequireProject returns the project or a NO_PROJECT error.
func (id Identity) RequireProject() (*registry.ProjectRow, error) {
	if id.Project == nil {
		return nil, apperr.New(apperr.NoProject,
			"no project: pass --project, set OTAG_PROJECT, or run inside a registered project directory")
	}
	return id.Project, nil
}

// Resolve applies the precedence rules against the synced projects.
func Resolve(ctx context.Context, q db.Querier, in Input) (Identity, error) {
	var id Identity
	projects, err := registry.ListProjects(ctx, q)
	if err != nil {
		return id, err
	}
	byName := func(name string) (*registry.ProjectRow, error) {
		for i := range projects {
			if projects[i].Name == name {
				return &projects[i], nil
			}
		}
		return nil, apperr.New(apperr.NotFound, "project %s not found (run `otag sync`?)", name)
	}
	byID := func(pid int64) (*registry.ProjectRow, error) {
		for i := range projects {
			if projects[i].ID == pid {
				return &projects[i], nil
			}
		}
		return nil, apperr.New(apperr.NotFound, "project of run is retired")
	}

	// Inside a run (OTAG_RUN_ID) the flags may not point at another run or project: a
	// child acts only for itself.
	if in.EnvRun != "" && (in.RunFlag != "" && in.RunFlag != in.EnvRun || in.ProjectFlag != "") {
		if in.RunFlag != "" && in.RunFlag != in.EnvRun {
			return id, apperr.New(apperr.Forbidden, "inside run %s, --run %s is not allowed: a child acts only for its own run",
				in.EnvRun, in.RunFlag)
		}
		pid, err := runProject(ctx, q, in.EnvRun)
		if err != nil {
			return id, err
		}
		if p, err := byName(in.ProjectFlag); err != nil || p.ID != pid {
			return id, apperr.New(apperr.Forbidden, "inside run %s, --project %s is not allowed: a child acts only in its own project",
				in.EnvRun, in.ProjectFlag)
		}
	}
	levels := []struct{ source, project, run string }{
		{"flag", in.ProjectFlag, in.RunFlag},
		{"env", in.EnvProject, in.EnvRun},
	}
	for _, lv := range levels {
		if id.RunID == "" && lv.run != "" {
			id.RunID, id.RunSource = lv.run, lv.source
		}
		if id.Project != nil || (lv.project == "" && lv.run == "") {
			continue
		}
		var fromRun *registry.ProjectRow
		if lv.run != "" {
			pid, err := runProject(ctx, q, lv.run)
			if err != nil {
				return id, err
			}
			if fromRun, err = byID(pid); err != nil {
				return id, err
			}
		}
		if lv.project != "" {
			p, err := byName(lv.project)
			if err != nil {
				return id, err
			}
			if fromRun != nil && fromRun.ID != p.ID {
				return id, apperr.New(apperr.Ambiguous, "run %s belongs to project %s, not %s", lv.run, fromRun.Name, p.Name)
			}
			id.Project, id.ProjectSource = p, lv.source
		} else {
			id.Project, id.ProjectSource = fromRun, "run"
		}
	}
	// The final run must exist and belong to the resolved project, wherever each came from
	// (e.g. --project from a flag with OTAG_RUN_ID inherited from the environment).
	if id.RunID != "" {
		pid, err := runProject(ctx, q, id.RunID)
		if err != nil {
			return id, err
		}
		if id.Project != nil && id.Project.ID != pid {
			return id, apperr.New(apperr.Ambiguous, "run %s (from %s) does not belong to project %s (from %s)",
				id.RunID, id.RunSource, id.Project.Name, id.ProjectSource)
		}
	}
	if id.Project != nil || in.Cwd == "" {
		return id, nil
	}

	p, err := matchCwd(projects, in.Cwd)
	if err != nil || p == nil {
		return id, err
	}
	id.Project, id.ProjectSource = p, "cwd"
	return id, nil
}

func runProject(ctx context.Context, q db.Querier, runID string) (int64, error) {
	var pid int64
	err := q.QueryRowContext(ctx, `SELECT project_id FROM runs WHERE id = ?`, runID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, apperr.New(apperr.NotFound, "run %s not found", runID)
	}
	return pid, err
}

// matchCwd returns the project whose path is the deepest ancestor of cwd. Two
// projects with the same deepest path are ambiguous.
func matchCwd(projects []registry.ProjectRow, cwd string) (*registry.ProjectRow, error) {
	cwd = realpath(cwd)
	var best []*registry.ProjectRow
	bestLen := -1
	for i := range projects {
		p := realpath(projects[i].Path)
		if cwd != p && !strings.HasPrefix(cwd, strings.TrimSuffix(p, "/")+"/") {
			continue
		}
		switch {
		case len(p) > bestLen:
			best, bestLen = []*registry.ProjectRow{&projects[i]}, len(p)
		case len(p) == bestLen:
			best = append(best, &projects[i])
		}
	}
	switch len(best) {
	case 0:
		return nil, nil
	case 1:
		return best[0], nil
	}
	names := make([]string, len(best))
	for i, p := range best {
		names[i] = p.Name
	}
	return nil, apperr.New(apperr.Ambiguous, "directory matches several projects (%s); pass --project",
		strings.Join(names, ", "))
}

func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
