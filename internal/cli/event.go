package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nesiler/otag/internal/adapter/builtin"
	"github.com/nesiler/otag/internal/daemon"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/run"
)

// maxPayload caps the hook payload read from stdin.
const maxPayload = 16 << 20

// hookMain implements `otag event <harness> <kind> [--event-id ID]`, the hook bridge.
// It never fails the hook: every problem is logged to stderr (and the hook log) and the
// adapter's no-op output is printed with exit status 0.
func (a *app) hookMain(ctx context.Context, args []string) (code int) {
	start := time.Now()
	var out []byte
	defer func() {
		if p := recover(); p != nil {
			a.hookLog("panic: %v", p)
		}
		_, _ = a.env.Stdout.Write(out)
		if a.env.Getenv("OTAG_HOOK_TIMING") != "" {
			_, _ = fmt.Fprintf(a.env.Stderr, "otag event: %.2fms\n", float64(time.Since(start).Microseconds())/1000)
		}
		code = ExitOK
	}()

	fs := a.flagSet("event")
	var eventID string
	fs.StringVar(&eventID, "event-id", "", "stable id of this event; repeated ids are ignored")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 2 {
		a.hookLog("usage: otag event <harness> <kind> [--event-id ID] < payload")
		return
	}
	// The whole hook, including reading stdin, has one budget; a harness that never
	// closes the pipe or a locked database cannot stall it.
	ctx, cancel := context.WithDeadline(ctx, start.Add(hookBudget))
	defer cancel()
	payload, err := readPayload(ctx, a.env.Stdin)
	if err != nil {
		a.hookLog("read payload: %v", err)
	}
	// Hooks never migrate (the CLI, the daemon and `otag daemon install` do): a schema
	// older or newer than this binary leaves d nil or read-only, and guarded hooks
	// refuse (run.HandleHook).
	var d *db.DB
	busy := time.Until(start.Add(hookBudget)) - 500*time.Millisecond
	if d, err = db.OpenWith(ctx, a.dbPath(), db.Options{BusyTimeout: max(busy, 100*time.Millisecond), NoMigrate: true}); err != nil {
		a.hookLog("open %s: %v", a.dbPath(), err)
		d = nil
	} else {
		a.db = d
	}
	runID := a.run
	if runID == "" {
		runID = a.env.Getenv("OTAG_RUN_ID")
	}
	resp, err := run.HandleHook(ctx, d, builtin.Registry(), run.HookRequest{
		Harness: pos[0], Kind: pos[1], RunID: runID, EventID: eventID, Payload: payload, Getenv: a.env.Getenv,
		Tmux: a.tmux(), RawPayloads: a.rawPayloads(),
	})
	out = resp.Output
	if resp.Delivered != nil {
		// The output carries messages claimed (dispatched) in the hook's transaction:
		// print it before anything else. The window between that commit and this write
		// cannot be closed: a hook killed in it (the harness's hook timeout) leaves the
		// messages dispatched without output, and their proof timeout makes them
		// outcome_unknown (shown in otag needs, never re-sent on their own).
		_, _ = a.env.Stdout.Write(out)
		out = nil
	}
	if err != nil {
		a.hookLog("%s %s (run %q): %v", pos[0], pos[1], runID, err)
	}
	if resp.MsgErr != nil {
		a.hookLog("%s %s (run %q): messages: %v", pos[0], pos[1], runID, resp.MsgErr)
	}
	if resp.Delivered != nil {
		// Printed: record them delivered (with what is left of the budget; a failure
		// leaves them to the proof timeout).
		if err := resp.Delivered(ctx); err != nil {
			a.hookLog("%s %s (run %q): settle delivered messages: %v", pos[0], pos[1], runID, err)
		}
	}
	return
}

// dispatcher is the delivery attempt send, reply and ask make after their commit
// (newDispatcher): injected channels only; busy Claude runs get their messages from
// their next hook, and the daemon retries what is left.
func (a *app) dispatcher() msg.Dispatcher {
	if a.db == nil {
		return nil
	}
	name := "user:" + a.env.Getenv("USER")
	if id := a.env.Getenv("OTAG_RUN_ID"); id != "" {
		name = "run:" + id
	}
	return &run.Dispatcher{DB: a.db, Adapters: builtin.Registry(), Getenv: a.env.Getenv,
		Actor: msg.Actor{Name: name, Source: "cli"}}
}

// hookBudget bounds one `otag event` from process start: stdin, database, everything.
const hookBudget = 5 * time.Second

// rawPayloads reports whether [daemon] raw_payloads asks for full (redacted) hook
// payloads; an unreadable config means no.
func (a *app) rawPayloads() bool {
	cfg, _, err := daemon.LoadConfig(daemon.ConfigFile(a.configDir()))
	return err == nil && cfg.RawPayloads
}

// readPayload reads stdin until EOF, maxPayload bytes or the context deadline,
// whichever comes first; what was read so far is returned.
func readPayload(ctx context.Context, r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	if f, ok := r.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			return nil, nil // a terminal, not a hook
		}
	}
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	var mu sync.Mutex
	var partial []byte
	go func() {
		buf := make([]byte, 64<<10)
		lr := io.LimitReader(r, maxPayload+1)
		for {
			n, err := lr.Read(buf)
			mu.Lock()
			partial = append(partial, buf[:n]...)
			mu.Unlock()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				mu.Lock()
				ch <- result{partial, err}
				mu.Unlock()
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if len(res.b) > maxPayload {
			return res.b[:maxPayload], fmt.Errorf("payload larger than %d bytes, truncated", maxPayload)
		}
		return res.b, res.err
	case <-ctx.Done():
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), partial...), fmt.Errorf("stdin still open after the hook budget; using %d bytes", len(partial))
	}
}

// hookLog reports a bridge problem on stderr and appends it to <data>/hook-errors.log,
// since harnesses usually hide hook stderr.
func (a *app) hookLog(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(a.env.Stderr, "otag event: %s\n", msg)
	dir := paths.DataDir(a.env.Getenv)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "hook-errors.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s %s\n", db.Now(), msg)
	_ = f.Close()
}
