package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/nesiler/otag/internal/run"
)

// runResume resumes a parked task by hand: otag run resume <task|run>. The answer to
// its question must be queued; the new run continues the parked harness session
// (same profile revision, workspace and an available account), else the error says
// why and the task keeps waiting. The daemon does the same on its own.
func runResume(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("run resume")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag run resume <task|run>")
	}
	m, _, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	res, err := m.Resume(ctx, pos[0])
	if err != nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) {
		r := res.Run
		_, _ = fmt.Fprintf(w, "resumed task %s as %s (%s, %s) — %s; it got %d message(s)\n", r.TaskID, r.ID, r.Profile, r.Harness,
			r.Status, len(res.Messages))
		_, _ = fmt.Fprintf(w, "attach: %s\n", res.Attach)
		if res.Warning != "" {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", res.Warning)
		}
	}}, nil
}

// runStopPark parks a run now (`otag run stop <run|task> --park`): its task must wait
// for the answer to the run's question. The run ends as parked through the normal stop
// (windows closed, cleanup), the task stays waiting, and the answer resumes it.
func runStopPark(ctx context.Context, a *app, ref, reason string) (result, error) {
	m, _, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	res, err := m.Park(ctx, ref, run.ParkOptions{Reason: reason})
	if err != nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) {
		r := res.Run
		_, _ = fmt.Fprintf(w, "%s: %s (%s); killed %d window(s); task %s is %s\n", r.ID, r.Status, r.EndedReason,
			len(res.Killed), r.TaskID, r.TaskStatus)
		if res.CleanupError != "" {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: cleanup incomplete (the daemon retries): %s\n", res.CleanupError)
		}
	}}, nil
}
