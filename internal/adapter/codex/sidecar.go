package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/proc"
)

func (*Adapter) RunSidecar(ctx context.Context, sc adapter.Sidecar, args []string) error {
	if len(args) == 3 && args[0] == "watchdog" {
		owner, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		root, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		return watchOwner(owner, root)
	}
	if len(args) == 2 && args[0] == "track" {
		return trackSession(ctx, sc, args[1])
	}
	if len(args) >= 3 && args[0] == "serve" {
		return runServer(ctx, args[1], args[2:])
	}
	if len(args) == 3 && args[0] == "rollout" {
		return watchRollout(ctx, sc, args[1], args[2])
	}
	if len(args) == 7 && args[0] == "bootstrap" {
		endpoint, method, idFile, model := args[1], args[2], args[4], args[5]
		if method != "thread/start" && method != "thread/resume" {
			return fmt.Errorf("invalid Codex bootstrap method %q", method)
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(args[3]), &params); err != nil {
			return err
		}
		c, err := connect(ctx, endpoint)
		if err != nil {
			return err
		}
		defer c.close()
		var result threadResult
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := c.call(ctx, method, params, &result)
			if err == nil {
				break
			}
			if method != "thread/resume" || !strings.Contains(err.Error(), "already has an active writer") || time.Now().After(deadline) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		if result.Thread.ID == "" {
			return fmt.Errorf("codex %s returned no thread id", method)
		}
		if method == "thread/resume" && result.Thread.ID != params["threadId"] {
			return fmt.Errorf("codex resumed a different thread")
		}
		workdir, _ := params["cwd"].(string)
		policy := turnPolicy(params)
		proxy, err := startTUIProxy(ctx, endpoint, filepath.Dir(idFile), result.Thread.ID, workdir, policy)
		if err != nil {
			return err
		}
		defer func() { _ = proxy.Close() }()
		// Deliveries get the same turn policy as TUI turns; the socket outlives reconnects.
		ctl := newController(filepath.Dir(idFile), result.Thread.ID, workdir, policy)
		stopCtl, err := ctl.serve(ctx)
		if err != nil {
			return err
		}
		defer stopCtl()
		if err := os.WriteFile(idFile, []byte(result.Thread.ID+"\n"), 0o600); err != nil {
			return err
		}
		ready := false
		err = listenRPC(ctx, sc, c, ctl, result.Thread.ID, model, &ready, &result, idFile, args[6], policy)
		if ctx.Err() != nil {
			return nil
		}
		if _, handoffErr := os.Stat(filepath.Join(filepath.Dir(idFile), "turn-ready")); !ready || handoffErr != nil {
			return err
		}
		c.close()
		return watchServer(ctx, sc, ctl, endpoint, result.Thread.ID, model, true)
	}
	if len(args) != 3 || args[1] == "" {
		return fmt.Errorf("codex sidecar needs endpoint, thread id, model")
	}
	return watchServer(ctx, sc, nil, args[0], args[1], args[2], false)
}

// watchServer follows the thread across reconnects; ctl (optional) accepts
// deliveries while a connection is reconciled and live.
func watchServer(ctx context.Context, sc adapter.Sidecar, ctl *controller, endpoint, sid, model string, ready bool) error {
	for ctx.Err() == nil {
		err := listen(ctx, sc, ctl, endpoint, sid, model, &ready)
		if ctx.Err() != nil {
			return nil
		}
		if !ready {
			return err
		}
		// Reconnect to the same thread, never choose an arbitrary loaded thread.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func listen(ctx context.Context, sc adapter.Sidecar, ctl *controller, endpoint, sid, model string, ready *bool) error {
	c, err := connect(ctx, endpoint)
	if err != nil {
		return err
	}
	defer c.close()
	return listenRPC(ctx, sc, c, ctl, sid, model, ready, nil, "", "", nil)
}

// listenRPC follows the thread; with idFile it also submits the first turn (prompt)
// under policy (turnPolicy). Once the thread is reconciled (and the first turn
// submitted) the connection goes live and ctl may deliver over it.
func listenRPC(ctx context.Context, sc adapter.Sidecar, c *rpc, ctl *controller, sid, model string, ready *bool, initial *threadResult, idFile, prompt string, policy map[string]any) error {
	if ctl != nil {
		ctl.reset()
		defer ctl.detach(c)
	}
	emit := func(kind string, p any) {
		b, _ := json.Marshal(p)
		if ctl != nil {
			ctl.apply(kind, b)
		}
		_, _ = sc.Emit(ctx, kind, b, "")
	}
	c.setHandler(func(msg envelope) {
		if ctl != nil && len(msg.ID) == 0 {
			ctl.apply(msg.Method, msg.Params)
		}
		var p payload
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		id := p.ThreadID
		if id == "" {
			id = p.Thread.ID
		}
		if id != sid && msg.Method != "account/rateLimits/updated" {
			return
		}
		if len(msg.ID) > 0 && msg.Method != "" {
			dir := ""
			if idFile != "" {
				dir = filepath.Dir(idFile)
			} else if sc.Getenv != nil {
				dir = sc.Getenv("OTAG_CODEX_RUN_DIR")
			}
			if dir != "" {
				if _, err := os.Stat(filepath.Join(dir, "tui-attached")); err != nil {
					if err := c.rejectEarly(ctx, sid); err == nil {
						emit("otag/earlyRequestRejected", map[string]any{"threadId": sid, "requestId": msg.ID, "method": msg.Method})
					}
				}
			}
			_, _ = sc.Emit(ctx, msg.Method, msg.Params, "")
			return
		}
		if msg.Method == "turn/started" {
			rememberTurn(sc, p.Turn.ID)
		}
		if msg.Method == "thread/tokenUsage/updated" {
			var data map[string]any
			if json.Unmarshal(msg.Params, &data) != nil {
				return
			}
			data["model"] = model
			emit(msg.Method, data)
			return
		}
		// Once subscribed, the TUI owns all approvals and questions.
		// Status flags and the question hook cover pending user requests.
		if msg.Method != "" && len(msg.ID) == 0 {
			switch msg.Method {
			case "turn/started", "turn/completed", "thread/status/changed", "error", "item/started", "item/completed", "account/rateLimits/updated":
				_, _ = sc.Emit(ctx, msg.Method, msg.Params, "")
			}
		}
	})
	var result threadResult
	if initial != nil {
		result = *initial
	} else if err := c.call(ctx, "thread/resume", map[string]any{"threadId": sid, "excludeTurns": true}, &result); err != nil {
		return err
	}
	if result.Thread.ID != sid {
		return fmt.Errorf("codex resumed a different thread")
	}
	if result.Model != "" {
		model = result.Model
	}
	if !*ready {
		source := "startup"
		if sc.Getenv != nil && sc.Getenv("OTAG_CODEX_SOURCE") == "resume" {
			source = "resume"
		}
		emit("SessionStart", map[string]any{"session_id": sid, "transcript_path": result.Thread.Path, "source": source})
	} else {
		if err := reconcileTurns(ctx, c, sc, sid, emit); err != nil {
			return err
		}
		if result.Thread.Path != "" {
			if err := reconcileUsage(ctx, sc, result.Thread.Path, sid, model, emit); err != nil {
				return err
			}
		}

	}
	emit("thread/status/changed", map[string]any{"threadId": sid, "status": result.Thread.Status})
	if !*ready {
		if err := sc.Ready(); err != nil {
			return err
		}
		*ready = true
	}
	if idFile != "" {
		dir := filepath.Dir(idFile)
		// Let otag create and record the pane before starting inference. The TUI
		// waits for a persisted first turn before trying to resume the thread.
		for {
			if _, err := os.Stat(filepath.Join(dir, "tui-ready")); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
		params := map[string]any{"threadId": sid, "input": []any{map[string]any{"type": "text", "text": prompt}}}
		for k, v := range policy {
			params[k] = v
		}

		var started payload
		if err := c.call(ctx, "turn/start", params, &started); err != nil {
			emit("error", map[string]any{"threadId": sid, "error": map[string]any{"message": err.Error()}, "willRetry": false})
			return err
		}
		rememberTurn(sc, started.Turn.ID)
		if ctl != nil {
			ctl.begin(started.Turn.ID)
		}
		if err := c.rejectEarly(ctx, sid); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "turn-ready"), nil, 0o600); err != nil {
			return err
		}
	}
	// Reconciled: dispatch what is still queued, then let the reader dispatch.
	c.goLive()
	if ctl != nil {
		ctl.attach(c)
	}
	return c.wait(ctx)
}

// watchdogWait bounds how long the supervisor waits for its watchdog after the
// server was reaped (the watchdog notices within a poll or two).
const watchdogWait = 5 * time.Second

// Supervise the server separately from the PTY: Codex can keep running after a
// tmux window closes. Shut down its owned process group and reap the child so no
// writer or code-mode host survives the run.
func runServer(ctx context.Context, logPath string, argv []string) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, log
	// The fallback TUI must remain in the foreground tty group to accept input.
	if filepath.Base(logPath) != "tui.log" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	known := map[int]process{}
	if all, err := processes(); err == nil {
		descendants(all, cmd.Process.Pid, known)
	}
	watchdog, err := startWatchdog(cmd.Process.Pid)
	if err != nil {
		signalOwned(known, cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	defer func() {
		// The watchdog ends once the server is reaped; never wait for it forever.
		if watchdog != nil {
			done := make(chan struct{})
			go func() { _ = watchdog.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(watchdogWait):
				_ = watchdog.Process.Kill()
				<-done
			}
		}
	}()
	rootBirth := proc.Birth(cmd.Process.Pid)
	// Do not call Wait until descendant/group cleanup is finished. In particular
	// there is no deferred signal to a group whose leader has been reaped.
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		// A failed snapshot says nothing about the server; its exit is then seen by
		// checking it alone (a zombie does not count as running).
		all, err := processes()
		if err == nil {
			descendants(all, cmd.Process.Pid, known)
		}
		p, live := all[cmd.Process.Pid]
		if err == nil && (!live || strings.HasPrefix(p.state, "Z")) || err != nil && !stillRuns(cmd.Process.Pid, rootBirth) {
			signalOwned(known, cmd.Process.Pid, syscall.SIGKILL)
			return cmd.Wait()
		}
		select {
		case <-ctx.Done():
			goto shutdown
		case <-tick.C:
		}
	}
shutdown:
	signalOwned(known, cmd.Process.Pid, syscall.SIGINT)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		all, err := processes()
		if err != nil {
			break
		}
		descendants(all, cmd.Process.Pid, known)
		p, live := all[cmd.Process.Pid]
		if !live || strings.HasPrefix(p.state, "Z") {
			break
		}
		<-tick.C
	}
	signalOwned(known, cmd.Process.Pid, syscall.SIGKILL)
	// The child is not reaped yet, so its pid (and group) cannot have been reused: kill
	// it directly too, in case no snapshot could be taken above.
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil
}
