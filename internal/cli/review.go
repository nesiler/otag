package cli

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nesiler/otag/internal/run"
)

// cmdReview records a review verdict:
// otag review <task> --accept ["note"] | --reject "reason". Inside a review run (its
// token verified) the run finishes with the verdict; the human and the coordinator
// review a task in review directly.
func cmdReview(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("review")
	var accept, reject bool
	fs.BoolVar(&accept, "accept", false, "accept the result: the task is done")
	fs.BoolVar(&reject, "reject", false, "reject it with a reason: the task goes back to ready")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) == 0 || accept == reject {
		return result{}, usageErr(`usage: otag review <task> --accept ["note"] | --reject "reason"`)
	}
	m, id, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	in := run.ReviewInput{Task: pos[0], Reason: strings.Join(pos[1:], " "), Verdict: run.Accept}
	if reject {
		in.Verdict = run.Reject
	}
	if id.RunID != "" {
		if err := run.CheckToken(ctx, m.DB, id.RunID, a.env.Getenv(run.TokenEnv)); err != nil {
			return result{}, err
		}
		in.RunID = id.RunID
		signal.Ignore(syscall.SIGHUP) // a review run's pane closes when it finishes (see cmdDone)
	}
	res, err := m.Review(ctx, in)
	if err != nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "review #%d: %s task %s", res.Review, res.Verdict, res.Task)
		if res.ReviewedRun != "" {
			_, _ = fmt.Fprintf(w, " (run %s, profile %s)", res.ReviewedRun, res.Profile)
		}
		_, _ = fmt.Fprintf(w, "; task %s is %s\n", res.Task, res.TaskStatus)
		if len(res.Cancelled) > 0 {
			_, _ = fmt.Fprintf(w, "cancelled review task(s): %s\n", strings.Join(res.Cancelled, ", "))
		}
		if d := res.Done; d != nil {
			_, _ = fmt.Fprintf(w, "run %s done; task %s is %s\n", d.Run, d.Task, d.TaskStatus)
		}
	}}, nil
}
