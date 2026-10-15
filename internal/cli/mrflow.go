package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// callerRun is the run a command acts for, verified by its OTAG_RUN_TOKEN (nil = the
// human). It never takes the caller from flags: --run outside a run is a usage error,
// and a caller that still looks like a run without OTAG_RUN_ID (runResidue) is
// FORBIDDEN, so a child that dropped its identity does not pass for the human.
func (a *app) callerRun(ctx context.Context, cmd string) (*run.Run, error) {
	runID := a.env.Getenv("OTAG_RUN_ID")
	if runID == "" {
		if a.run != "" {
			return nil, usageErr("otag %s acts as its caller: --run works only inside that run", cmd)
		}
		if why := a.runResidue(); why != "" {
			return nil, apperr.New(apperr.Forbidden, "otag %s without OTAG_RUN_ID, but %s: a run acts only as itself", cmd, why)
		}
		return nil, nil
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return nil, err
	}
	if err := run.CheckToken(ctx, d, runID, a.env.Getenv(run.TokenEnv)); err != nil {
		return nil, err
	}
	return run.Get(ctx, d, runID)
}

// taskPush pushes a task branch (run.Manager.PushTask):
// otag task push <id> [--force]. Who may push is the project's push setting.
func taskPush(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task push")
	var force bool
	fs.BoolVar(&force, "force", false, "--force-with-lease against the commit otag pushed last")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag task push <id> [--force]")
	}
	caller, err := a.callerRun(ctx, "task push")
	if err != nil {
		return result{}, err
	}
	m, id, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	in := run.PushInput{Task: pos[0], Force: force, Caller: run.CallerHuman, CallerRun: caller}
	if caller != nil {
		in.Caller = run.CallerChild
		if caller.Role == "coordinator" {
			in.Caller = run.CallerCoordinator
		}
	}
	key := requestKey("task push", id, fs, pos)
	res, replayed, err := replay[*run.PushResult](ctx, a, key)
	if err != nil || replayed {
		return pushResult(res), err
	}
	res, err = m.PushTask(ctx, in)
	if res == nil || err != nil {
		if res != nil {
			return pushResult(res), err
		}
		return result{}, err
	}
	return pushResult(res), remember(ctx, a, key, res)
}

func pushResult(res *run.PushResult) result {
	return result{data: res, human: func(w io.Writer) {
		if res == nil {
			return
		}
		verb := "pushed"
		if res.Forced {
			verb = "force-pushed (with lease)"
		}
		_, _ = fmt.Fprintf(w, "%s %s %s to %s/%s\n", verb, res.Task, short(res.SHA), res.Remote, res.Branch)
		if res.PreviousSHA != "" && res.PreviousSHA != res.SHA {
			_, _ = fmt.Fprintf(w, "previous push: %s\n", short(res.PreviousSHA))
		}
		for _, s := range res.Warnings {
			_, _ = fmt.Fprintf(w, "warning: %s\n", s)
		}
	}}
}

// taskReopen sends a done task back to ready on the same worktree and branch
// (run.ReopenTask): otag task reopen <id> "reason" [--version V]. The human and the
// project's coordinator reopen; a child cannot (childGate).
func taskReopen(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task reopen")
	var version int64
	fs.Int64Var(&version, "version", 0, "expected task version (CAS)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) < 2 {
		return result{}, usageErr(`usage: otag task reopen <id> "reason"`)
	}
	caller, err := a.callerRun(ctx, "task reopen")
	if err != nil {
		return result{}, err
	}
	m, id, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	in := run.ReopenInput{Task: pos[0], Reason: strings.Join(pos[1:], " "), ExpectVersion: version}
	res, err := mutate(ctx, a, requestKey("task reopen", id, fs, pos), func(tx *sql.Tx) (*run.ReopenResult, error) {
		if caller != nil {
			t, err := task.Get(ctx, tx, in.Task)
			if err != nil {
				return nil, err
			}
			if caller.Role != "coordinator" || caller.ProjectID != t.ProjectID {
				return nil, apperr.New(apperr.Forbidden, "only the coordinator of project %s or the human reopens its tasks",
					t.Project)
			}
		}
		return run.ReopenTask(ctx, tx, in, m.Actor)
	})
	if err != nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "reopened %s: it is %s on branch %s\n", res.Task.ID, res.Task.Status, dash(res.Task.Branch))
		if res.StaleReviews > 0 {
			_, _ = fmt.Fprintf(w, "%d accepted review(s) no longer count; it is reviewed again when it is done\n", res.StaleReviews)
		}
		_, _ = fmt.Fprintf(w, "next: otag run start %s (its run gets the reason)\n", res.Task.ID)
	}}, nil
}
