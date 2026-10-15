package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/nesiler/otag/internal/run"
)

// cmdPrime prints the caller's orientation: who it is, its task or project, the otag
// commands open to it and its unread messages (not marked read).
func cmdPrime(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("prime")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) > 0 {
		return result{}, usageErr("usage: otag prime")
	}
	if _, err := a.openDB(ctx); err != nil {
		return result{}, err
	}
	c, err := a.msgCaller(ctx, "prime") // verifies the run token; a person is the human
	if err != nil {
		return result{}, err
	}
	in := run.PrimeInput{ProjectID: c.projectID()}
	if c.run != nil {
		in = run.PrimeInput{RunID: c.run.ID}
	}
	p, err := run.BuildPrime(ctx, a.db, in)
	if err != nil {
		return result{}, err
	}
	return result{data: p, human: func(w io.Writer) { _, _ = fmt.Fprint(w, p.Text()) }}, nil
}
