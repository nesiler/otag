package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/builtin"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/run"
)

// cmdSidecar implements the hidden `otag sidecar <harness> [--ready-file F] [-- args]`:
// it runs the adapter's SidecarRunner in-process, feeding the hook bridge directly,
// until the run's window is killed.
func cmdSidecar(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("sidecar")
	var readyFile string
	fs.StringVar(&readyFile, "ready-file", "", "file created once the sidecar is subscribed")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) == 0 {
		return result{}, usageErr("usage: otag sidecar <harness> [--ready-file F] [-- args...]")
	}
	reg := builtin.Registry()
	ad, ok := reg.Get(pos[0])
	if !ok {
		return result{}, apperr.New(apperr.Invalid, "unknown harness %q", pos[0])
	}
	runner, ok := ad.(adapter.SidecarRunner)
	if !ok {
		return result{}, apperr.New(apperr.Invalid, "harness %s has no sidecar", pos[0])
	}
	runID := a.run
	if runID == "" {
		runID = a.env.Getenv("OTAG_RUN_ID")
	}
	if runID == "" {
		return result{}, apperr.New(apperr.NoRun, "sidecar needs OTAG_RUN_ID or --run")
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	raw := a.rawPayloads()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	sc := adapter.Sidecar{
		RunID: runID, Getenv: a.env.Getenv,
		Emit: func(ctx context.Context, kind string, payload []byte, eventID string) ([]byte, error) {
			resp, err := run.HandleHook(ctx, d, reg, run.HookRequest{Harness: pos[0], Kind: kind, RunID: runID,
				EventID: eventID, Payload: payload, Getenv: a.env.Getenv, Tmux: a.tmux(), RawPayloads: raw})
			if err != nil {
				a.hookLog("sidecar %s %s (run %s): %v", pos[0], kind, runID, err)
			}
			return resp.Output, err
		},
		Ready: func() error {
			if readyFile == "" {
				return nil
			}
			return os.WriteFile(readyFile, nil, 0o600)
		},
	}
	return result{}, runner.RunSidecar(ctx, sc, pos[1:])
}
