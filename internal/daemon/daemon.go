// Package daemon is otag's background service (`otag daemon`, run by launchd): every
// tick it runs the quota collectors, reaps runs whose pane died, stops stalled runs,
// reminds about unanswered questions, starts fallback runs on another profile after a
// usage limit or a harness that would not launch, and shows new notifications.
// Everything it does goes through the same transactions as the CLI, so it may be
// down at any time without losing state. See docs/DAEMON.md.
package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/event"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

// QuotaCollector refreshes quota_snapshots and account/profile cooldowns (phase 3b).
// Run calls every collector from a goroutine of its own, on its first round and then
// at most once per Config.QuotaInterval, so a slow network or Keychain call never
// delays the pane and stall checks; an error is logged.
type QuotaCollector interface {
	Collect(ctx context.Context) error
}

// Daemon runs the loops. Runs must use Actor{Name: "daemon", Source: "daemon"}.
type Daemon struct {
	DB         *db.DB
	Runs       *run.Manager
	Config     Config
	Collectors []QuotaCollector
	Now        func() time.Time                                    // default time.Now
	Desktop    func(ctx context.Context, title, body string) error // default osascript
	Log        *log.Logger                                         // default discards
	// Housekeeping, when set, runs after every tick of Run (log rotation).
	Housekeeping func()

	deadSince   map[string]time.Time
	retries     map[string]startRetry // ended run → failed fallback starts without a run
	orphanSince map[string]time.Time  // pane → first seen as an orphan window
	// resumeBackoff: parked task → failed resumes (park.go, watchParked)
	resumeBackoff map[string]startRetry

	collectMu sync.Mutex
	lastQuota time.Time
	lastPrune time.Time // raw payload retention (retention.go)
}

// Actor is how the daemon appears in events.
var Actor = run.Actor{Name: "daemon", Source: "daemon"}

func (d *Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Daemon) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf(format, args...)
	}
}

// TickTimeout bounds one tick (a fallback launch included). ExitTimeOut of the launch
// agent is longer, so launchd never kills the daemon in the middle of a tick.
const TickTimeout = 2 * time.Minute

// Run ticks until ctx is cancelled. A tick in progress is finished (bounded) before Run
// returns, so a fallback launch is never cut in half by a shutdown. The quota
// collectors run in a goroutine of their own and are cancelled at shutdown.
func (d *Daemon) Run(ctx context.Context) error {
	tick := d.Config.Tick
	if tick <= 0 {
		tick = DefaultConfig().Tick
	}
	cctx, stopCollect := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if len(d.Collectors) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.collectLoop(cctx, tick)
		}()
	}
	defer func() {
		stopCollect()
		wg.Wait()
	}()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), TickTimeout)
		d.Tick(tctx)
		cancel()
		if d.Housekeeping != nil {
			d.Housekeeping()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (d *Daemon) collectLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		d.Collect(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Collect runs every quota collector when they are due: on the first call, then at
// most once per QuotaInterval. Errors are logged. Run calls it from its own goroutine;
// `otag daemon --once` calls it before its tick.
func (d *Daemon) Collect(ctx context.Context) {
	d.collectMu.Lock()
	defer d.collectMu.Unlock()
	every := d.Config.QuotaInterval
	if every <= 0 {
		every = DefaultConfig().QuotaInterval
	}
	now := d.now()
	if len(d.Collectors) == 0 || (!d.lastQuota.IsZero() && now.Sub(d.lastQuota) < every) {
		return
	}
	d.lastQuota = now
	ctx, cancel := context.WithTimeout(ctx, TickTimeout)
	defer cancel()
	for _, c := range d.Collectors {
		if err := c.Collect(ctx); err != nil && ctx.Err() == nil {
			d.logf("quota collector %T: %v", c, err)
		}
	}
}

// Tick runs the run loops once (not the quota collectors, see Collect). Errors are
// logged; one failing loop does not stop the others.
func (d *Daemon) Tick(ctx context.Context) {
	runs, err := d.liveRuns(ctx)
	if err != nil {
		d.logf("list runs: %v", err)
	} else {
		d.watchPanes(ctx, runs)
		d.watchStopping(ctx, runs)
		d.watchLaunches(ctx, runs)
		d.watchStalls(ctx, runs)
		if err := d.watchWaiting(ctx, runs); err != nil {
			d.logf("unanswered: %v", err)
		}
	}
	if err := d.watchCleanups(ctx); err != nil {
		d.logf("cleanup: %v", err)
	}
	if err := d.watchOrphans(ctx); err != nil {
		d.logf("orphan windows: %v", err)
	}
	if err := d.fallbacks(ctx); err != nil {
		d.logf("fallback: %v", err)
	}
	if err := d.messages(ctx); err != nil {
		d.logf("messages: %v", err)
	}
	d.lifecycle(ctx, runs) // park, resume, idle reminders (park.go)
	if err := d.deliver(ctx); err != nil {
		d.logf("notifications: %v", err)
	}
	if err := d.pruneRawPayloads(ctx); err != nil {
		d.logf("raw payload retention: %v", err)
	}
}

// liveRuns lists runs that have not ended, with their pane checked (one tmux call).
func (d *Daemon) liveRuns(ctx context.Context) ([]*run.Run, error) {
	all, err := run.List(ctx, d.DB, run.Filter{Active: true, Limit: 100000})
	if err != nil {
		return nil, err
	}
	var out []*run.Run
	for i := range all {
		out = append(out, &all[i])
	}
	if len(out) > 0 {
		run.CheckLive(ctx, d.Runs.Tmux, out)
	}
	return out, nil
}

// watchPanes ends live runs whose pane stayed dead for DeadGrace (a session_end hook
// may still be on its way): crashed, or launch_failed (cause harness) when the harness
// died before its first event. A server or sidecar window that died while the main
// pane lives (an OpenCode server, a Codex app-server, an event listener) is the same
// failure: the harness lost its server, or otag lost the events. Such a window is not
// restarted, because tmux would respawn it without the launch environment, which otag
// does not keep (it carries account secrets). StopWith kills the run's other windows
// and requeues the task.
func (d *Daemon) watchPanes(ctx context.Context, runs []*run.Run) {
	now := d.now()
	dead := map[string]time.Time{}
	for _, r := range runs {
		var detail string // what died, e.g. "pane exited: exit status 3"
		switch {
		case r.Dead():
			detail = "pane " + r.Live.State + ": " + r.Live.Detail
		case r.AuxDown != nil:
			a := r.AuxDown
			detail = fmt.Sprintf("%s window %s %s: %s", a.Role, a.Name, a.Live.State, a.Live.Detail)
		default:
			continue
		}
		since, ok := d.deadSince[r.ID]
		if !ok {
			since = now
		}
		if now.Sub(since) < d.Config.DeadGrace {
			dead[r.ID] = since
			continue
		}
		o := run.StopOptions{}
		if !r.Dead() {
			o.EndReason, o.Reason = run.ReasonCrashed, detail+" while the harness ran"
		}
		kind, title := "run.crashed", "run "+r.ID+" crashed"
		if r.Status == run.Starting && r.FirstActivityAt == "" {
			o = run.StopOptions{EndReason: run.ReasonLaunchFailed, Cause: run.CauseHarness,
				Reason: "harness exited during startup (" + detail + ")"}
			kind, title = "run.launch_failed", "run "+r.ID+" failed to start"
		}
		want := run.ReasonCrashed
		if o.EndReason != "" {
			want = o.EndReason
		}
		o.Expect = &run.Expect{Status: r.Status, HeartbeatAt: r.HeartbeatAt}
		res, err := d.Runs.StopWith(ctx, r.ID, o)
		if errors.Is(err, run.ErrChanged) {
			// A hook arrived after the snapshot: look again on the next tick.
			d.logf("reap %s: skipped, the run changed meanwhile", r.ID)
			dead[r.ID] = since
			continue
		}
		if err != nil {
			d.logf("reap %s: %v", r.ID, err)
			dead[r.ID] = since
			continue
		}
		d.logf("%s: %s, ended %s", r.ID, detail, res.Run.EndedReason)
		if res.Run.EndedReason != want {
			continue // it ended some other way meanwhile
		}
		d.notify(ctx, r, kind, notify.Warn, title,
			fmt.Sprintf("%s%s (profile %s). Task %s is %s.", strings.ToUpper(detail[:1]), detail[1:], r.Profile, r.TaskID,
				res.Run.TaskStatus))
	}
	d.deadSince = dead
}

// stopRetryAfter: a run that has been stopping this long lost the process that stopped
// it (or killing its windows failed); the daemon finishes the stop.
const stopRetryAfter = time.Minute

// watchStopping finishes stops that were left half done: the run.status → stopping
// event carries the intent (reason, end reason, cause), and run.ResumeStop applies it.
func (d *Daemon) watchStopping(ctx context.Context, runs []*run.Run) {
	now := d.now()
	for _, r := range runs {
		if r.Status != run.Stopping {
			continue
		}
		var since string
		if err := d.DB.QueryRowContext(ctx, `SELECT coalesce(max(created_at), '') FROM events WHERE run_id = ?
			AND kind = 'run.status' AND to_status = 'stopping'`, r.ID).Scan(&since); err != nil {
			d.logf("stopping %s: %v", r.ID, err)
			continue
		}
		if t := parseTime(since); t.IsZero() || now.Sub(t) < stopRetryAfter {
			continue
		}
		res, err := d.Runs.ResumeStop(ctx, r.ID)
		if errors.Is(err, run.ErrChanged) {
			continue
		}
		if err != nil {
			d.logf("finish stop of %s: %v", r.ID, err)
			continue
		}
		d.logf("%s: finished an interrupted stop, ended %s", r.ID, res.Run.EndedReason)
		if res.Run.EndedReason == run.ReasonLaunchFailed {
			// The stop that watchLaunches began failed half way, so it did not report it.
			d.notify(ctx, r, "run.launch_failed", notify.Warn, "run "+r.ID+" never finished launching",
				fmt.Sprintf("%s (profile %s). Task %s is %s.", res.Run.Error, r.Profile, r.TaskID, res.Run.TaskStatus))
		}
	}
}

// watchLaunches ends queued or starting runs that still have no pane recorded after
// LaunchTimeout: whoever launched them (`otag run start`, or this daemon cut off by a
// shutdown) died in the middle. They end as launch_failed (cause interrupted, which is
// not a fallback reason), their recorded server/sidecar windows are killed and the
// task goes back to ready. A launch that is in fact still going loses the race to the
// stop and kills what it started (see run.launcher.record). A starting run that did
// record its pane but got no first event within LaunchTimeout is ended too
// (firstEventDeadline).
func (d *Daemon) watchLaunches(ctx context.Context, runs []*run.Run) {
	now := d.now()
	limit := d.Config.LaunchTimeout
	if limit <= 0 {
		limit = DefaultConfig().LaunchTimeout
	}
	for _, r := range runs {
		if r.Status != run.Queued && r.Status != run.Starting {
			continue
		}
		since := latest(r.QueuedAt, r.StartedAt)
		if since.IsZero() || now.Sub(since) < limit {
			continue
		}
		if r.Session != nil && r.Session.TmuxPane != "" {
			d.firstEventDeadline(ctx, r, limit)
			continue
		}
		res, err := d.Runs.StopWith(ctx, r.ID, run.StopOptions{EndReason: run.ReasonLaunchFailed, Cause: run.CauseInterrupted,
			Reason: fmt.Sprintf("launch did not record a pane within %s (the launching process was killed?)", limit),
			Expect: &run.Expect{Status: r.Status, HeartbeatAt: r.HeartbeatAt, NoPane: true}})
		if errors.Is(err, run.ErrChanged) {
			continue
		}
		if err != nil {
			d.logf("launch timeout %s: %v", r.ID, err)
			continue
		}
		if res.Run.EndedReason != run.ReasonLaunchFailed {
			continue
		}
		d.logf("%s: %s with no pane after %s, ended launch_failed", r.ID, r.Status, limit)
		d.notify(ctx, r, "run.launch_failed", notify.Warn, "run "+r.ID+" never finished launching",
			fmt.Sprintf("No pane was recorded within %s (profile %s); the launch was interrupted. Task %s is %s.",
				limit, r.Profile, r.TaskID, res.Run.TaskStatus))
	}
}

// firstEventDeadline ends a starting run whose pane was recorded LaunchTimeout ago but
// that never got its first hook event: the harness hangs at startup (a login or trust
// prompt nobody answers), or its hooks never reach otag. Neither the pane watcher (the
// pane lives) nor the stall watchdog (only running runs) would ever end it. It ends as
// launch_failed with cause harness, so the fallback may try another profile. A pane
// that already died is the pane watcher's.
func (d *Daemon) firstEventDeadline(ctx context.Context, r *run.Run, limit time.Duration) {
	if r.Status != run.Starting || r.FirstActivityAt != "" || r.Dead() {
		return
	}
	res, err := d.Runs.StopWith(ctx, r.ID, run.StopOptions{EndReason: run.ReasonLaunchFailed, Cause: run.CauseHarness,
		Reason: fmt.Sprintf("no hook event within %s of the launch (the harness hangs at startup, or its hooks do not reach otag)", limit),
		Expect: &run.Expect{Status: r.Status, HeartbeatAt: r.HeartbeatAt}})
	if errors.Is(err, run.ErrChanged) {
		return
	}
	if err != nil {
		d.logf("first event %s: %v", r.ID, err)
		return
	}
	if res.Run.EndedReason != run.ReasonLaunchFailed {
		return
	}
	d.logf("%s: no first event after %s, ended launch_failed", r.ID, limit)
	d.notify(ctx, r, "run.launch_failed", notify.Warn, "run "+r.ID+" never started working",
		fmt.Sprintf("No hook event within %s of the launch (profile %s); check the harness's login and hooks. Task %s is %s.",
			limit, r.Profile, r.TaskID, res.Run.TaskStatus))
}

// watchStalls stops running runs that produced no event for StallAfter (per harness;
// ToolStallAfter while a tool call runs): the adapter's Stopper gets its chance, the
// windows are killed, the run ends stalled and the task goes back to ready. A run
// waiting for an answer is never stalled.
func (d *Daemon) watchStalls(ctx context.Context, runs []*run.Run) {
	now := d.now()
	for _, r := range runs {
		if r.Status != run.Running || r.Live == nil || r.Live.State != tmux.Alive {
			continue
		}
		last := latest(r.HeartbeatAt, r.LastProgressAt, r.StartedAt)
		if d.Config.StallPaneActivity && r.Live.Activity.After(last) {
			last = r.Live.Activity
		}
		limit := d.Config.stallAfter(r.Harness, false)
		if last.IsZero() || now.Sub(last) < limit {
			continue
		}
		inTool, err := d.toolRunning(ctx, r.ID)
		if err != nil {
			d.logf("stall %s: %v", r.ID, err)
			continue
		}
		if inTool {
			if limit = d.Config.stallAfter(r.Harness, true); now.Sub(last) < limit {
				continue
			}
		}
		idle := now.Sub(last).Round(time.Second)
		res, err := d.Runs.StopWith(ctx, r.ID, run.StopOptions{EndReason: run.ReasonStalled,
			Reason: fmt.Sprintf("no event for %s (limit %s)", idle, limit),
			Expect: &run.Expect{Status: r.Status, HeartbeatAt: r.HeartbeatAt}})
		if errors.Is(err, run.ErrChanged) {
			d.logf("stall %s: skipped, an event arrived meanwhile", r.ID)
			continue
		}
		if err != nil {
			d.logf("stall %s: %v", r.ID, err)
			continue
		}
		if res.Run.EndedReason != run.ReasonStalled {
			continue // it ended some other way meanwhile
		}
		d.logf("%s: stalled after %s", r.ID, idle)
		d.notify(ctx, r, "run.stalled", notify.Warn, "run "+r.ID+" stalled",
			fmt.Sprintf("No event for %s (profile %s); stopped. Task %s is %s.", idle, r.Profile, r.TaskID, res.Run.TaskStatus))
	}
}

// toolRunning reports whether more tool calls started than ended in the run's current
// turn (activity events with data.tool_phase since the last turn_start/turn_end). The
// count never goes below zero, so a stray end (an end whose start was not reported)
// cannot hide a later tool call.
func (d *Daemon) toolRunning(ctx context.Context, runID string) (bool, error) {
	rows, err := d.DB.QueryContext(ctx, `
SELECT coalesce(json_extract(data, '$.tool_phase'), '')
FROM events
WHERE run_id = ? AND kind = 'hook.activity' AND json_extract(data, '$.tool_phase') IN ('start', 'end')
  AND id > coalesce((SELECT max(id) FROM events WHERE run_id = ? AND kind IN ('hook.turn_start', 'hook.turn_end')), 0)
ORDER BY id`, runID, runID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	open := 0
	for rows.Next() {
		var phase string
		if err := rows.Scan(&phase); err != nil {
			return false, err
		}
		if phase == "start" {
			open++
		} else {
			open = max(0, open-1)
		}
	}
	return open > 0, rows.Err()
}

// watchWaiting adds one "unanswered" notification per waiting episode that lasts
// UnansweredAfter.
func (d *Daemon) watchWaiting(ctx context.Context, runs []*run.Run) error {
	now := d.now()
	for _, r := range runs {
		if r.Status != run.Waiting || r.Dead() { // a dead one is reaped instead
			continue
		}
		var since, question string
		err := d.DB.QueryRowContext(ctx, `
SELECT coalesce((SELECT max(created_at) FROM events WHERE run_id = ? AND kind = 'run.status' AND to_status = 'waiting'), ''),
       coalesce((SELECT status_reason FROM tasks WHERE id = ?), '')`, r.ID, r.TaskID).Scan(&since, &question)
		if err != nil {
			return err
		}
		t := parseTime(since)
		if t.IsZero() || now.Sub(t) < d.Config.UnansweredAfter {
			continue
		}
		var n int
		if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM notifications
			WHERE run_id = ? AND kind = 'run.unanswered' AND created_at >= ?`, r.ID, since).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body := fmt.Sprintf("Waiting for %s.", now.Sub(t).Round(time.Minute))
		if question != "" {
			body += " " + question
		}
		d.notify(ctx, r, "run.unanswered", notify.Warn, "run "+r.ID+" waits for an answer", body)
	}
	return nil
}

// watchCleanups finishes the cleanup of ended runs that still have one pending
// (runs.cleanup_pending): the process that ended them died before it killed their
// windows, killing failed, or a process outlived its window. run.Manager.Cleanup kills
// what is left and clears the flag once nothing is. A failure is retried after
// run.CleanupRetry (runs.cleanup_after, so only due runs are picked and a restart keeps
// the spacing); the first one for a run makes a run.cleanup_failed notification.
func (d *Daemon) watchCleanups(ctx context.Context) error {
	if d.Runs == nil {
		return nil
	}
	ids, err := run.PendingCleanups(ctx, d.DB, d.now(), 50)
	if err != nil {
		return err
	}
	for _, id := range ids {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		closed, err := d.Runs.Cleanup(cctx, id)
		cancel()
		if err == nil {
			if len(closed) > 0 {
				d.logf("%s: cleanup killed %s", id, strings.Join(closed, ", "))
			}
			continue
		}
		d.logf("cleanup of %s: %v", id, err)
		var n int
		if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE run_id = ? AND kind = 'run.cleanup_failed'`,
			id).Scan(&n); err != nil || n > 0 {
			continue
		}
		if r, gerr := run.Get(ctx, d.DB, id); gerr == nil {
			d.notify(ctx, r, "run.cleanup_failed", notify.Warn, "run "+id+" left something running",
				fmt.Sprintf("Run %s ended (%s), but its cleanup failed: %s. The daemon keeps retrying.", id, r.EndedReason, err))
		}
	}
	return nil
}

// watchOrphans kills windows tagged for a run of this database whose run has ended or
// does not exist (run.Manager.Orphans): a launcher that died between creating a window
// and recording it leaves one behind, and nothing else knows its pane. A window is
// killed once it has been an orphan for DeadGrace (its launcher may be killing it).
func (d *Daemon) watchOrphans(ctx context.Context) error {
	if d.Runs == nil || d.Runs.Tmux == nil || !d.Runs.Tmux.Available() {
		return nil
	}
	orphans, err := d.Runs.Orphans(ctx)
	if err != nil {
		return err
	}
	now := d.now()
	seen := map[string]time.Time{}
	for _, o := range orphans {
		key := fmt.Sprintf("%s/%d", o.Pane, o.PID)
		since, ok := d.orphanSince[key]
		if !ok {
			since = now
		}
		if now.Sub(since) < d.Config.DeadGrace {
			seen[key] = since
			continue
		}
		if err := d.Runs.KillOrphan(ctx, o); err != nil {
			seen[key] = since
			d.logf("orphan window %s of run %s: %v", o.Pane, o.Run, err)
			continue
		}
		state := o.RunStatus
		if state == "" {
			state = "unknown"
		}
		d.logf("killed orphan window %s (pid %d) of run %s (%s)", o.Pane, o.PID, o.Run, state)
	}
	d.orphanSince = seen
	return nil
}

func (d *Daemon) notify(ctx context.Context, r *run.Run, kind, severity, title, body string) {
	_, err := notify.Add(ctx, d.DB, notify.New{ProjectID: r.ProjectID, TaskID: r.TaskID, RunID: r.ID,
		Kind: kind, Severity: severity, Title: title, Body: body})
	if err != nil {
		d.logf("notification %s for %s: %v", kind, r.ID, err)
	}
}

// desktopMaxAge: older notifications (made while the daemon was down) are not shown.
const desktopMaxAge = 15 * time.Minute

// deliver shows notifications nobody has handled yet as macOS notifications (when
// enabled) and marks them sent. A failing osascript never fails the loop.
func (d *Daemon) deliver(ctx context.Context) error {
	pending, err := notify.Pending(ctx, d.DB, 50)
	if err != nil {
		return err
	}
	show := d.Desktop
	if show == nil {
		show = Osascript
	}
	for _, n := range pending {
		channel := "none"
		if d.Config.DesktopNotify && d.now().Sub(parseTime(n.CreatedAt)) < desktopMaxAge {
			if err := show(ctx, n.Title, n.Body); err != nil {
				d.logf("desktop notification %d: %v", n.ID, err)
			} else {
				channel = "desktop"
			}
		}
		if err := notify.MarkSent(ctx, d.DB, n.ID, channel); err != nil {
			return err
		}
	}
	return nil
}

// Notification text limits (bytes, cut at a rune boundary).
const (
	maxTitle = 120
	maxBody  = 300
)

// Osascript shows a macOS notification.
func Osascript(ctx context.Context, title, body string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", osascriptScript(title, body)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// osascriptScript is the AppleScript of a notification: title and body as escaped
// string literals on one line, each clipped at a rune boundary (a cut in the middle of
// a UTF-8 sequence makes osascript fail).
func osascriptScript(title, body string) string {
	q := func(s string, n int) string {
		s = clip(strings.Join(strings.Fields(strings.ToValidUTF8(s, "?")), " "), n)
		s = strings.ReplaceAll(s, `\`, `\\`)
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "display notification " + q(body, maxBody) + " with title \"otag\" subtitle " + q(title, maxTitle)
}

// clip shortens s to at most n bytes plus "…", cutting at a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// --------------------------------------------------------------------------- fallback

// fallbacks handles ready tasks whose latest run ended with a reason in FallbackOn
// (launch_failed only when the harness was the cause, or when a daemon-started run's
// launch was interrupted) and that the daemon has not handled yet: it starts the next
// available profile of the chain's fallback list, or blocks the task. Each ended run
// is handled once: the decision (event dedupe key fallback:<run>) commits in the same
// transaction as the run it starts or the block it applies.
func (d *Daemon) fallbacks(ctx context.Context) error {
	if len(d.Config.FallbackOn) == 0 {
		return nil
	}
	reasons := strings.TrimSuffix(strings.Repeat("?,", len(d.Config.FallbackOn)), ",")
	args := []any{}
	for _, r := range d.Config.FallbackOn {
		args = append(args, r)
	}
	rows, err := d.DB.QueryContext(ctx, `
SELECT t.id, t.version, r.id FROM ready_tasks t
JOIN runs r ON r.id = (SELECT id FROM runs WHERE task_id = t.id ORDER BY attempt DESC LIMIT 1)
WHERE r.status = 'ended'
  AND (r.ended_reason IN (`+reasons+`) OR (r.ended_reason = 'launch_failed' AND r.fallback_origin_profile_id IS NOT NULL))
  AND (r.ended_reason <> 'launch_failed' OR EXISTS (
        SELECT 1 FROM events e WHERE e.run_id = r.id AND e.kind = 'run.status' AND e.to_status = 'ended'
          AND (json_extract(e.data, '$.cause') = 'harness' AND r.ended_reason IN (`+reasons+`)
               OR json_extract(e.data, '$.cause') = 'interrupted' AND r.fallback_origin_profile_id IS NOT NULL)))
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.source = 'daemon' AND e.dedupe_key = 'fallback:' || r.id)
ORDER BY t.priority DESC, t.created_at`, append(args, args...)...)
	if err != nil {
		return err
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.task, &c.version, &c.run); err != nil {
			_ = rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := d.now()
	for _, c := range cands {
		r, ok := d.retries[c.run]
		if !ok {
			if r, ok, err = d.loadRetry(ctx, c.run); err != nil {
				return err
			}
			if ok {
				if d.retries == nil {
					d.retries = map[string]startRetry{}
				}
				d.retries[c.run] = r
			}
		}
		if ok && now.Before(r.next) {
			continue // a transient start failure: wait for the back-off
		}
		if err := d.fallback(ctx, c); err != nil {
			d.logf("fallback for %s after %s: %v", c.task, c.run, err)
		}
	}
	return nil
}

// candidate is a ready task whose latest run may need a fallback, as the daemon saw it.
type candidate struct {
	task, run string
	version   int64 // tasks.version
}

// link is one run of a fallback chain.
type link struct {
	id, profile string
	profileID   int64
	origin      sql.NullInt64
	retryOf     string
	interrupted bool // its launch was interrupted (launch_failed, cause interrupted)
}

// chain walks back from runID through the runs the daemon started as fallbacks to the
// run that began the chain. The first element is runID.
func (d *Daemon) chain(ctx context.Context, runID string) ([]link, error) {
	var out []link
	for id := runID; id != "" && len(out) < 100; {
		var l link
		err := d.DB.QueryRowContext(ctx, `SELECT r.id, p.name, r.profile_id, r.fallback_origin_profile_id,
			coalesce(r.retry_of_run_id, ''),
			coalesce(r.ended_reason = 'launch_failed' AND (SELECT json_extract(e.data, '$.cause') FROM events e
				WHERE e.run_id = r.id AND e.kind = 'run.status' AND e.to_status = 'ended' ORDER BY e.id DESC LIMIT 1)
				= 'interrupted', 0)
			FROM runs r JOIN profiles p ON p.id = r.profile_id WHERE r.id = ?`, id).
			Scan(&l.id, &l.profile, &l.profileID, &l.origin, &l.retryOf, &l.interrupted)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
		if !l.origin.Valid {
			break
		}
		id = l.retryOf
	}
	return out, nil
}

type decision struct {
	Run       string   `json:"run"`
	Reason    string   `json:"ended_reason"`
	Origin    string   `json:"origin_profile"`
	Attempts  int      `json:"attempts"`
	Profile   string   `json:"profile,omitempty"` // the fallback started
	Skipped   []string `json:"skipped,omitempty"`
	Block     string   `json:"block,omitempty"` // why the task was blocked instead
	profileID int64
	originID  int64
}

func (d *Daemon) decide(ctx context.Context, runID string) (*decision, error) {
	ch, err := d.chain(ctx, runID)
	if err != nil {
		return nil, err
	}
	cur := ch[0]
	dec := &decision{Run: runID, Attempts: len(ch), originID: cur.profileID, Origin: cur.profile}
	if err := d.DB.QueryRowContext(ctx, `SELECT ended_reason FROM runs WHERE id = ?`, runID).Scan(&dec.Reason); err != nil {
		return nil, err
	}
	if cur.origin.Valid {
		dec.originID = cur.origin.Int64
		if err := d.DB.QueryRowContext(ctx, `SELECT name FROM profiles WHERE id = ?`, dec.originID).Scan(&dec.Origin); err != nil {
			return nil, err
		}
	}
	// A run whose launch was interrupted (the daemon died mid-start) says nothing about
	// its profile: that profile may be tried again.
	tried := map[int64]bool{}
	var names []string
	for i := len(ch) - 1; i >= 0; i-- {
		if !ch[i].interrupted {
			tried[ch[i].profileID] = true
		}
		names = append(names, ch[i].profile)
	}
	if len(ch) >= d.Config.MaxAttempts {
		dec.Block = fmt.Sprintf("%s after %d attempt(s) (%s); fallback limit is %d",
			dec.Reason, len(ch), strings.Join(names, " > "), d.Config.MaxAttempts)
		return dec, nil
	}

	rows, err := d.DB.QueryContext(ctx, `SELECT f.position, f.fallback_profile_id, p.name FROM profile_fallbacks f
		JOIN profiles p ON p.id = f.fallback_profile_id WHERE f.profile_id = ? ORDER BY f.position`, dec.originID)
	if err != nil {
		return nil, err
	}
	type fb struct {
		pos  int
		id   int64
		name string
	}
	var list []fb
	pos := 0 // the list positions not to try again (0 = the origin)
	for rows.Next() {
		var f fb
		if err := rows.Scan(&f.pos, &f.id, &f.name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		list = append(list, f)
		if f.id == cur.profileID && cur.origin.Valid {
			pos = f.pos
			if cur.interrupted {
				pos = f.pos - 1
			}
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	av := run.Availability{QuotaThreshold: d.Config.QuotaThreshold, Now: d.now}
	for _, f := range list {
		if f.pos <= pos || tried[f.id] {
			continue
		}
		if ok, why := av.Check(ctx, d.DB, f.id); !ok {
			dec.Skipped = append(dec.Skipped, f.name+": "+why)
			continue
		}
		dec.Profile, dec.profileID = f.name, f.id
		return dec, nil
	}
	switch {
	case len(list) == 0:
		dec.Block = fmt.Sprintf("%s on %s, which has no fallback profiles", dec.Reason, cur.profile)
	default:
		dec.Block = fmt.Sprintf("%s on %s and no fallback of %s is available", dec.Reason, cur.profile, dec.Origin)
		if len(dec.Skipped) > 0 {
			dec.Block += " (" + strings.Join(dec.Skipped, "; ") + ")"
		}
	}
	return dec, nil
}

// quiesceTimeout bounds waiting for the ended run's processes before a fallback.
var quiesceTimeout = 15 * time.Second

// errStale means a fallback decision no longer applies: the task was claimed,
// changed, got another run, or the decision was already recorded.
var errStale = errors.New("the task changed since the fallback decision")

// claimDecision checks, inside tx, that the task is still the one the decision was made
// for (ready, same version, the same latest run) and records the decision (dedupe key
// fallback:<run>). It returns errStale otherwise.
func (d *Daemon) claimDecision(ctx context.Context, tx *sql.Tx, c candidate, projectID int64, dec *decision) error {
	t, err := task.Get(ctx, tx, c.task)
	if err != nil {
		return err
	}
	var latest string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE task_id = ? ORDER BY attempt DESC LIMIT 1`, c.task).
		Scan(&latest); err != nil {
		return err
	}
	if t.Status != task.Ready || t.Version != c.version || latest != c.run {
		return errStale
	}
	ok, err := event.Record(ctx, tx, event.Event{Source: "daemon", DedupeKey: "fallback:" + c.run,
		Kind: "daemon.fallback", EntityType: "task", EntityID: c.task, ProjectID: projectID, TaskID: c.task,
		RunID: c.run, Actor: Actor.Name, Data: dec})
	if err == nil && !ok {
		err = errStale
	}
	return err
}

// fallback handles one ended run of a ready task.
func (d *Daemon) fallback(ctx context.Context, c candidate) error {
	taskID, runID := c.task, c.run
	dec, err := d.decide(ctx, runID)
	if err != nil {
		return err
	}
	r, err := run.Get(ctx, d.DB, runID)
	if err != nil {
		return err
	}
	if dec.Block != "" {
		err = d.DB.Tx(ctx, func(tx *sql.Tx) error {
			if err := d.claimDecision(ctx, tx, c, r.ProjectID, dec); err != nil {
				return err
			}
			if _, err := task.Transition(ctx, tx, taskID, task.Blocked, dec.Block, 0,
				task.Actor{Name: Actor.Name, Source: Actor.Source}); err != nil {
				return err
			}
			_, err = notify.Add(ctx, tx, notify.New{ProjectID: r.ProjectID, TaskID: taskID, RunID: runID,
				Kind: "task.blocked", Severity: notify.Error, Title: "task " + taskID + " blocked", Body: dec.Block})
			return err
		})
		if errors.Is(err, errStale) {
			return nil // someone else took it meanwhile
		}
		if err == nil {
			d.logf("%s: blocked: %s", taskID, dec.Block)
		}
		return err
	}

	// Nothing of the ended run may keep writing to the worktree the new run uses.
	qctx, cancel := context.WithTimeout(ctx, quiesceTimeout)
	err = d.Runs.Quiesce(qctx, runID)
	cancel()
	if err != nil {
		return d.startFailed(ctx, r, dec, fmt.Errorf("run %s is not fully stopped: %w", runID, err))
	}
	res, err := d.Runs.Start(ctx, run.StartInput{TaskID: taskID, Profile: dec.Profile, FallbackOrigin: dec.originID,
		Guard: func(tx *sql.Tx) error { return d.claimDecision(ctx, tx, c, r.ProjectID, dec) }})
	if errors.Is(err, errStale) {
		delete(d.retries, runID)
		d.logf("%s: fallback after %s skipped: %v", taskID, runID, err)
		return nil
	}
	if err != nil {
		return d.startFailed(ctx, r, dec, err)
	}
	delete(d.retries, runID)
	body := fmt.Sprintf("Run %s (%s) ended: %s. Started %s on profile %s (attempt %d of %d).",
		runID, r.Profile, dec.Reason, res.Run.ID, dec.Profile, dec.Attempts+1, d.Config.MaxAttempts)
	if len(dec.Skipped) > 0 {
		body += " Skipped: " + strings.Join(dec.Skipped, "; ") + "."
	}
	d.logf("%s: %s", taskID, body)
	_, err = notify.Add(ctx, d.DB, notify.New{ProjectID: r.ProjectID, TaskID: taskID, RunID: res.Run.ID,
		Kind: "run.fallback", Severity: notify.Info, Title: "task " + taskID + " falls back to " + dec.Profile, Body: body})
	return err
}

// startRetry is the back-off of a fallback whose start failed before it created a run.
type startRetry struct {
	n    int
	next time.Time
}

// maxStartRetries transient start failures (no run created) block the task.
const maxStartRetries = 5

// loadRetry restores the back-off of runID's fallback from its latest
// daemon.fallback_retry event (attempt, retry_in, written by startFailed), so a
// restarted daemon keeps counting instead of starting over.
func (d *Daemon) loadRetry(ctx context.Context, runID string) (startRetry, bool, error) {
	var n int
	var wait, at string
	err := d.DB.QueryRowContext(ctx, `SELECT coalesce(json_extract(data, '$.attempt'), 0),
		coalesce(json_extract(data, '$.retry_in'), ''), created_at
		FROM events WHERE source = 'daemon' AND kind = 'daemon.fallback_retry' AND run_id = ?
		ORDER BY id DESC LIMIT 1`, runID).Scan(&n, &wait, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return startRetry{}, false, nil
	}
	if err != nil {
		return startRetry{}, false, err
	}
	w, _ := time.ParseDuration(wait)
	return startRetry{n: n, next: parseTime(at).Add(w)}, n > 0, nil
}

// startFailed handles a failed fallback start. When Start created a run (the decision
// committed with it), its launch failure is that run's business (a harness cause is
// the next link of the chain on a later tick). Otherwise nothing was recorded: a
// transient error (the old run's windows would not die, tmux missing, database busy,
// ...) is retried by a later tick after a back-off of 30s doubling up to 10 min; a
// permanent one (profile gone, no adapter, project retired), or the fifth transient
// one, blocks the task with the reason. A task someone else started or changed
// meanwhile is left alone.
func (d *Daemon) startFailed(ctx context.Context, r *run.Run, dec *decision, startErr error) error {
	taskID, runID := r.TaskID, r.ID
	var created int
	if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE retry_of_run_id = ?`, runID).Scan(&created); err != nil {
		return errors.Join(startErr, err)
	}
	if created > 0 {
		delete(d.retries, runID)
		d.notify(ctx, r, "run.fallback", notify.Error, "fallback for task "+taskID+" failed",
			fmt.Sprintf("Run %s ended (%s); starting profile %s failed: %v", runID, dec.Reason, dec.Profile, startErr))
		return startErr
	}
	t, err := task.Get(ctx, d.DB, taskID)
	if err != nil {
		return errors.Join(startErr, err)
	}
	if t.Status != task.Ready {
		delete(d.retries, runID)
		d.logf("%s: fallback start failed (%v), but the task is %s now; leaving it", taskID, startErr, t.Status)
		return nil
	}
	rt := d.retries[runID]
	rt.n++
	if transientStart(startErr) && rt.n < maxStartRetries {
		wait := min(30*time.Second<<(rt.n-1), 10*time.Minute)
		rt.next = d.now().Add(wait)
		if d.retries == nil {
			d.retries = map[string]startRetry{}
		}
		d.retries[runID] = rt
		_, err := event.Record(ctx, d.DB, event.Event{Source: "daemon", Kind: "daemon.fallback_retry", EntityType: "task",
			EntityID: taskID, ProjectID: r.ProjectID, TaskID: taskID, RunID: runID, Actor: Actor.Name,
			Data: map[string]any{"run": runID, "profile": dec.Profile, "error": startErr.Error(), "attempt": rt.n,
				"retry_in": wait.String()}})
		d.logf("%s: fallback to %s failed to start (%v); retry %d in %s", taskID, dec.Profile, startErr, rt.n, wait)
		return err
	}
	delete(d.retries, runID)
	why := fmt.Sprintf("%s on %s; fallback to %s could not start: %s", dec.Reason, r.Profile, dec.Profile, oneLine(startErr.Error()))
	if rt.n > 1 {
		why += fmt.Sprintf(" (%d attempts)", rt.n)
	}
	c := candidate{task: taskID, run: runID, version: t.Version}
	err = d.DB.Tx(ctx, func(tx *sql.Tx) error {
		block := *dec
		block.Profile, block.Block = "", why
		if err := d.claimDecision(ctx, tx, c, r.ProjectID, &block); err != nil {
			return err
		}
		if _, err := task.Transition(ctx, tx, taskID, task.Blocked, why, 0,
			task.Actor{Name: Actor.Name, Source: Actor.Source}); err != nil {
			return err
		}
		_, err = notify.Add(ctx, tx, notify.New{ProjectID: r.ProjectID, TaskID: taskID, RunID: runID,
			Kind: "task.blocked", Severity: notify.Error, Title: "task " + taskID + " blocked", Body: why})
		return err
	})
	if errors.Is(err, errStale) {
		return nil
	}
	if err == nil {
		d.logf("%s: blocked: %s", taskID, why)
	}
	return err
}

// transientStart reports whether a Start error that created no run may go away by
// itself (tmux or git unavailable, database busy, unknown errors); invalid input,
// missing or retired profiles and projects, and missing adapters do not.
func transientStart(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	e, ok := apperr.As(err)
	if !ok {
		return true
	}
	switch e.Code {
	case apperr.NotFound, apperr.Invalid, apperr.InvalidTransition, apperr.ConfigInvalid, apperr.SchemaTooNew:
		return false
	}
	return true
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// --------------------------------------------------------------------------- helpers

func parseTime(s string) time.Time {
	t, err := time.Parse(db.TimeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func latest(ts ...string) time.Time {
	var out time.Time
	for _, s := range ts {
		if t := parseTime(s); t.After(out) {
			out = t
		}
	}
	return out
}
