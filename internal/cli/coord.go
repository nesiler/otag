package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
)

// coordResult is the data of otag coord.
type coordResult struct {
	Run     *run.Run `json:"run"`
	Started bool     `json:"started"` // false: the project's live coordinator was found
	Attach  string   `json:"attach_command"`
	Warning string   `json:"warning,omitempty"`
}

// cmdCoord starts the project's coordinator, or finds the one that runs:
// otag coord [<project>] [--profile P] [--attach]. A project has at most one live
// coordinator (a Claude run in the project checkout, role coordinator).
func cmdCoord(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("coord")
	var profile string
	var attach bool
	fs.StringVar(&profile, "profile", "", "profile (default: the project's coordinator_profile, then default_profile)")
	fs.BoolVar(&attach, "attach", false, "attach the terminal to the coordinator's window")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) > 1 {
		return result{}, usageErr("usage: otag coord [<project>] [--profile P] [--attach]")
	}
	m, id, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	project := ""
	if len(pos) == 1 {
		project = pos[0]
	} else {
		p, err := id.RequireProject()
		if err != nil {
			return result{}, err
		}
		project = p.Name
	}
	var projectID int64
	err = a.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ? AND retired_at IS NULL`, project).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return result{}, apperr.New(apperr.NotFound, "project %s not found", project)
	}
	if err != nil {
		return result{}, err
	}
	res := coordResult{}
	live, err := run.LiveCoordinator(ctx, a.db, projectID)
	if err != nil {
		return result{}, err
	}
	if live != "" {
		if res.Run, err = run.Get(ctx, a.db, live); err != nil {
			return result{}, err
		}
		if profile != "" && profile != res.Run.Profile {
			res.Warning = fmt.Sprintf("the coordinator runs on profile %s; stop it (otag run stop %s) to use %s", res.Run.Profile,
				live, profile)
		}
	} else {
		started, err := m.StartCoordinator(ctx, run.CoordInput{Project: project, Profile: profile})
		if err != nil {
			return result{}, err
		}
		res.Run, res.Started, res.Warning = started.Run, true, started.Warning
	}
	s := res.Run.Session
	if s != nil && s.TmuxPane != "" {
		res.Attach = shellQuoteAttach(a, s.TmuxSession, s.TmuxPane)
	}
	out := result{data: res, human: func(w io.Writer) {
		r := res.Run
		verb := "coordinator"
		if res.Started {
			verb = "started coordinator"
		}
		_, _ = fmt.Fprintf(w, "%s %s of %s (%s, %s) — %s\n", verb, r.ID, r.Project, r.Profile, r.Harness, r.Status)
		if res.Attach != "" {
			_, _ = fmt.Fprintf(w, "attach: %s\n", res.Attach)
		}
		if res.Warning != "" {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", res.Warning)
		}
	}}
	if !attach || a.json || !isTerminal(a.env.Stdout) || s == nil || s.TmuxPane == "" {
		return out, nil
	}
	out.human(a.env.Stdout)
	return result{}, execAttach(ctx, a, m.Tmux, s.TmuxSession, s.TmuxPane)
}

func shellQuoteAttach(a *app, session, pane string) string {
	return tmux.ShellQuote(a.tmux().AttachArgs(session, pane))
}
