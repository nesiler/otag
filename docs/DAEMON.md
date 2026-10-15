# The daemon

`otag daemon` is otag's background service (phase 3a). It watches runs and acts on them:
it ends runs whose pane died, stops stalled runs, reminds you about unanswered questions,
starts a fallback run on another profile when a run ran out of quota or its harness would
not start, and shows notifications. Code: `internal/daemon`, `internal/notify`, and the
quota/cooldown parts of `internal/run` (`quota.go`).

The daemon holds no state of its own. Everything it does is a normal transaction on the
database (the same CAS updates and `events` rows as the CLI), so it can be stopped,
restarted or not running at all without losing anything. Without it, hooks and the CLI
still work. The only differences are that a dead pane stays `DEAD` until `otag run stop`,
nothing is stalled, and no fallback runs start.

## Running it

```
otag daemon [--once]        # foreground; --once: one collector round and one tick, then exit
otag daemon --service       # what launchd runs (see "Launch agent")
otag daemon install         # write and load the launch agent
otag daemon uninstall       # unload and remove it
otag daemon status          # running/pid, agent installed/loaded, config, log, unread notifications
```

- **Single instance**: an exclusive `flock` on `<data>/daemon.lock`, which also holds the
  pid. A second `otag daemon` fails with `CONFLICT`. `otag status`, `otag daemon status` and
  `otag doctor` probe the lock and do not take it.
- **Shutdown**: SIGTERM (launchd's `bootout`) or SIGINT stops the loop after the current
  tick. A tick in progress, such as a fallback launch, is finished first (it has a
  2-minute limit, `daemon.TickTimeout`); the quota collectors are cancelled.
- **Launch agent**: `~/Library/LaunchAgents/io.github.nesiler.otag.plist` (mode 0600), label
  `io.github.nesiler.otag`, `ProgramArguments = [<otag>, daemon, --service]`, `RunAtLoad`,
  `KeepAlive = {SuccessfulExit = false}` (restarted only after a failure),
  `ThrottleInterval 10`, `ExitTimeOut 150` (launchd waits that long after SIGTERM before
  SIGKILL, longer than a tick), `ProcessType Background`. stdout and stderr go to
  `<data>/daemon.log`.
  - `--service`: permanent startup problems (invalid `otag.toml`, a database schema newer
    than the binary, another daemon holding the lock) are logged with a hint and exit 0,
    so launchd does not restart the daemon every 10 s. Fix the cause, then re-run
    `otag daemon install` (or `launchctl kickstart gui/$UID/io.github.nesiler.otag`). Everything
    else (database busy, a crash) exits non-zero and launchd restarts it.
  - **Log rotation**: at start and then at most once a minute, a `daemon.log` larger than
    10 MiB is renamed to `daemon.log.1` (replacing the older one) and a new file is
    pointed at stdout/stderr with `dup2`, because launchd holds the descriptors.
  - `EnvironmentVariables` (captured by `install`; launchd gives agents almost nothing.
    Runs do not inherit it wholesale: every run window starts from a clean environment
    with only `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `TMUX_TMPDIR`, `LANG`, `LC_*`
    and the profile's `env_passthrough` taken from the launching process (the daemon for
    a fallback, the CLI for `otag run start`), plus account/profile `env`, the adapter's
    variables and `OTAG_*`; docs/ADAPTERS.md, "Threat model"):
    - `PATH`: the directory of otag first, then the installing shell's PATH without
      short-lived entries: relative ones (`.`, empty), fnm's per-shell
      `…/fnm_multishells/<pid>_<ts>/bin` (replaced by `$FNM_DIR/aliases/default/bin`,
      default `~/.local/share/fnm/...`, when that exists) and anything under `/tmp`,
      `/private/tmp`, `/var/folders`, `/private/var/folders` or `$TMPDIR`. `install`
      prints what it dropped.
    - `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `LANG`, `LC_*` when set, and
      `SSH_AUTH_SOCK` unless it lives under a temporary directory (launchd gives GUI
      agents their own per-boot socket; a stable custom agent such as 1Password's is
      kept).
    - the variables named in `[daemon] env_passthrough` (a trailing `*` matches a
      prefix), e.g. the API keys an OpenCode config reads with `{env:VAR}`. Their values
      are stored in the plist, which is why it is 0600. `PATH` and `OTAG_*` are reserved
      and never taken from there (a pattern like `P*` would otherwise bring back the
      unsanitized PATH).
    - every `OTAG_DB`, `OTAG_CONFIG_DIR`, `OTAG_DATA_DIR`, `OTAG_TMUX_SOCKET`, `OTAG_TMUX_CONF`
      and `OTAG_BIN` that is set at install time, as absolute paths.

    `otag doctor` reads the installed plist and warns about every `{env:VAR}` in an
    OpenCode account's `opencode.json[c]` that it does not set, and about every one an
    OpenCode profile's `env_passthrough` (or account/profile `env`) does not cover: such
    a variable must be in both to reach a run started by the daemon.
  - `install` and `uninstall` hold an exclusive `flock` on `~/Library/LaunchAgents`
    (waiting at most 30 s), so concurrent runs never interleave. `install` writes the
    plist to a fresh `os.CreateTemp` file (0600 from the start, never an existing file
    or a symlink), renames it over the agent, and runs `launchctl bootout
    gui/$UID/io.github.nesiler.otag`. "Not loaded" (exit 3/113) is fine; any other bootout error
    is reported. After a successful bootout it polls `launchctl print` until the old
    service is gone (at most 10 s), then runs `launchctl bootstrap gui/$UID <plist>`,
    retrying up to 5 times while launchd answers `5: Input/output error` or `37:
    Operation already in progress`. `uninstall` runs the same bootout and removes the
    file. Re-run `install` after you move the binary, change the OTAG_* variables or
    `env_passthrough`, or upgrade from a version without `--service`.
  - `OTAG_LAUNCHCTL` overrides the `launchctl` binary; the tests use a fake one and a
    temporary HOME.

## Configuration: `<config>/otag.toml`, section `[daemon]`

The file is optional, and so is every key in it. Other sections are left for other
parts of otag. `otag doctor` reports invalid values and unknown keys.

```toml
[daemon]
tick = "3s"                  # loop interval (min 100ms)
dead_grace = "5s"            # a pane must be dead this long before its run is reaped
stall_after = "20m"          # running with no event this long = stalled
tool_stall_after = "45m"     # the same while a tool call runs (started, not ended)
stall_pane_activity = false  # count pane output (tmux window_activity) as a heartbeat
unanswered_after = "10m"     # waiting this long = one reminder
launch_timeout = "5m"        # queued/starting with no pane (or no first event) this long = launch failed (min 30s)
max_attempts = 3             # runs per fallback chain before the task is blocked
quota_threshold = 90         # quota used_percent that makes a profile unavailable
quota_interval = "60s"       # how often the quota collectors run (min 1s; docs/QUOTA.md)
fallback_on = ["quota", "launch_failed"]   # also allowed: "stalled", "crashed"
desktop_notifications = true # macOS notifications via osascript
env_passthrough = []         # read by `otag daemon install`: extra variables for the launch agent
raw_payloads = false         # store hook payloads (redacted, clipped) for debugging; off: a compact extract
raw_payload_retention = "168h"  # the daemon deletes raw_hook_payloads rows older than this ("0s" keeps them)

[daemon.stall_after_harness] # per-harness stall_after
codex = "20m"

[messages]                   # the keys the daemon reads (others belong to delivery)
park_after = "30m"           # park an idle run whose question is unanswered this long ("0s" = only ask --park)
idle_after = "10m"           # remind an idle child without a question ("0s" = off); 2 reminders, then the human
```

The 30-minute default cooldown (below) is set by the hook bridge, which reads no config
file, so it is a constant (`run.DefaultCooldown`).

## Quota collectors

The quota collectors run in a goroutine of their own, not in the tick, so a slow
LiteLLM endpoint never delays the pane watcher or the stall watchdog: on the first round
and then at most once per `quota_interval`, every registered `daemon.QuotaCollector` runs
`Collect(ctx)` (one round is bounded by 2 minutes and cancelled at shutdown). An error
is logged. `otag daemon` registers one long-lived `quota.New(...)` (statusLine files,
Codex rollouts, LiteLLM, optional Claude OAuth; docs/QUOTA.md); its network sources keep
their own spacing (LiteLLM 2 min, OAuth 5 min). The Keychain (`/usr/bin/security`, which
may show a macOS dialog) and `claude --version` are only used for accounts with
`quota.oauth_usage = true`. `otag daemon --once` runs one collector round, then one tick.

## A tick

In order:

1. **Pane watcher**: for every run that has not ended, one `tmux list-panes` call. A live
   run (`starting|idle|running|waiting`) whose pane is `exited` or `gone` for
   `dead_grace` is ended with `Manager.StopWith`, which is the `otag run stop` path: the
   Stopper is skipped because the pane is dead, every other window (server, sidecars) is
   killed after its pid is checked, the workspace is kept, and the task goes back to
   `ready`. The run ends as:
   - `crashed`, with `exit_code` when tmux knows it;
   - `launch_failed` with `data.cause = "harness"` when the run was still `starting` and
     had no event yet. The harness died during startup (wrong binary, bad flags), which
     makes the run eligible for fallback.

   The same call checks the run's **server and sidecar windows** (`aux_panes`: the
   OpenCode server, the Codex app-server, event listeners). One that is `exited` or
   `gone` while the main pane lives for `dead_grace` ends the run as `crashed` (or
   `launch_failed`, cause `harness`, before the first event), with `runs.error` naming
   the window, e.g. `sidecar window events exited: exit status 1 while the harness ran`.
   Such a window is not restarted: `tmux respawn-pane` would run it without the launch
   environment, which otag does not keep (it carries account secrets).

   `unknown` liveness (no pane recorded yet, tmux missing, or a `list-panes` answer that
   cannot be parsed) is never reaped. The grace period gives a `session_end` hook that is
   still on its way the chance to end the run as `exited`.

   **Snapshot precondition** (pane watcher and stall watchdog): the decision is made on
   the tick's snapshot, so `StopWith` gets `StopOptions.Expect{Status, HeartbeatAt}` and
   checks it inside the transaction that moves the run to `stopping`. If a hook changed
   the run in between (a new event, `running → waiting`, or the run ended), nothing is
   stopped (`run.ErrChanged`) and the next tick looks again. A notification is only
   written when the run really ended with the reason the daemon chose, so a run that
   ended `exited`/`done` meanwhile never gets a "crashed" notification.
2. **Launch timeout**: a `queued` or `starting` run that still has no pane recorded
   `launch_timeout` after it was queued/started lost its launcher (`otag run start` or
   this daemon killed mid-launch). It is ended with `StopWith{EndReason: launch_failed,
   Cause: interrupted}` (with the snapshot precondition, which here also requires that
   still no pane is recorded inside the transaction, so a launcher that records its pane
   after the snapshot wins): recorded server/sidecar windows are killed, the task goes
   back to `ready`, and a `run.launch_failed` notification is written. For a run a
   person or coordinator started, `interrupted` is not a fallback cause (the profile was
   not at fault); for a fallback run the daemon started, the chain goes on and tries the
   same profile again (see Fallback). A launch that is in fact still running sees
   `stopping` and kills what it started.
   **First event**: a `starting` run whose pane *was* recorded but that got no hook
   event `launch_timeout` after it started (the harness hangs at a login or trust
   prompt, or its hooks never reach otag) is ended the same way, as `launch_failed` with
   cause `harness` (so it may fall back), with the snapshot precondition and a
   `run.launch_failed` notification. A dead pane is left to the pane watcher.
   **Interrupted stops**: `StopWith` records its intent (`reason`, `end_reason`, `cause`)
   on the `run.status → stopping` event before it kills anything. A run still `stopping`
   a minute later lost the process that stopped it, or killing its windows failed; the
   daemon finishes the stop with that intent (`Manager.ResumeStop`).
3. **Stall watchdog**: a `running` run with a live pane and no event (latest of
   `heartbeat_at`, `last_progress_at`, `started_at`) for `stall_after`, or for the
   harness's override, is stopped with `StopWith{EndReason: stalled}`. The adapter's
   `Stopper` gets its graceful exit (interrupt / `/exit`, at most 3 s), then the windows
   are killed and the task goes back to `ready`. `runs.error` says how long it was quiet.
   `waiting`, `idle` and `starting` runs are never stalled.
   - **Tool calls**: one long tool call (a Claude Bash command of up to 10 min, a build,
     a test suite) emits no events while it runs. Adapters mark tool starts and ends
     (`activity` events with `data.tool_phase = start|end`: PreToolUse/PostToolUse,
     Codex `item/started|completed`, OpenCode `tool.input.started`/`tool.success|failed`).
     While more tools started than ended in the current turn (since the last
     `turn_start`/`turn_end`), the limit is `tool_stall_after` (or the harness override,
     whichever is longer). The count never drops below zero, so an end without a start
     cannot hide a later tool; Claude's question tools (`AskUserQuestion`,
     `ExitPlanMode`) report neither.
   - **Pane activity** (`stall_pane_activity = true`, off by default): the run pane's
     last output (`#{window_activity}`, from the same `list-panes` call; otag gives every
     run pane its own window) counts as a heartbeat. Off because TUIs that animate a
     spinner while they wait on the network would then never stall; turn it on if your
     runs have tool calls longer than `tool_stall_after` that keep printing.
4. **Unanswered**: a `waiting` run gets one `run.unanswered` notification once it has been
   waiting for `unanswered_after`. The time counts from its latest `run.status → waiting`
   event, and the notification body includes the question (the task's `status_reason`).
   A new wait gets a new reminder.
4a. **Cleanups** (see "Run cleanup" below): ended runs with `cleanup_pending` set that
   are due get `Manager.Cleanup` (at most 15 s each, up to 50 per tick, longest
   waiting first). A failure stays in `runs.cleanup_error` and sets
   `runs.cleanup_after` 30 s ahead, so failing runs never crowd out the others and a
   restart keeps the spacing; the first one makes a `run.cleanup_failed` notification.
4b. **Orphan windows**: windows tagged for a run of this database (`@otag_db`) whose run
   ended or does not exist (`Manager.Orphans`) are killed with their processes once
   they have been orphans for `dead_grace`. Untagged windows, windows of another
   database and windows of active runs (recorded or not) are left alone.
   A stop finished this way that ends `launch_failed` gets the `run.launch_failed`
   notification the interrupted stop could not write.
5. **Fallback** (below).
5a. **Lifecycle** (`park.go`, see "Park and resume" below): park idle askers, resume
   parked tasks whose answer is queued (at most 3 launches per tick), remind idle
   children. The messaging step (delivery, proof timeouts) is P3's and runs next to it.
6. **Notifications**: rows with `sent_at IS NULL` are shown with `osascript -e 'display
   notification …'` when `desktop_notifications` is on and the row is less than 15 minutes
   old (by the daemon's clock), so rows written while the daemon was down do not pop up
   later. Title (120 bytes) and body (300 bytes) are clipped at a rune boundary, put on
   one line and escaped as AppleScript strings. Every row is then marked `sent_at`
   (`channel` = `desktop` or `none`). A failing osascript is logged and never fails the
   loop.
7. **Raw payload retention**: at most once an hour, `raw_hook_payloads` rows older than
   `raw_payload_retention` (default 7 days, `0s` keeps them) are deleted. Events and run
   state are not touched.

## Park and resume (phase 4)

Messaging itself is in docs/MESSAGES.md. A child that needs an answer runs `otag ask "q"`:
the task goes `running → waiting` and the session stays open (idle) for the answer.

**Park** (`run.Manager.Park`): the run ends through the normal stop path (`StopWith`:
Stopper, windows killed, cleanup) with `ended_reason = parked`; the task stays `waiting`
and the ask stays open. Nothing else is closed. It happens when:
- the ask was `otag ask --park` and the asker is `idle` (its turn ended): the daemon
  parks it on the next tick;
- the ask is older than `[messages] park_after` and the asker is idle (auto-park);
- a person runs `otag run stop <run|task> --park`.

The daemon decides on its snapshot, so the stop carries `Expect{idle, heartbeat}`;
`StopOptions.Guard` checks inside the `→ stopping` transaction that the task still waits
on this run and the ask is still open (an answer that arrived first wins). A coordinator
run, or a run without an open ask of its own, is never parked. An interrupted park is
finished by `ResumeStop` with the same intent, and also leaves the task waiting.
Notification `run.parked` (info).

**Resume** (`run.Manager.Resume`, by `watchParked` or `otag run resume <task|run>`): for a
waiting task whose current run ended `parked` and whose answer is queued. In the
creation transaction:
1. the profile's current revision must be the one the parked run started with (same
   account, harness and configuration), the profile and account available
   (`run.ProfileAvailable`: no cooldown, quota under the threshold), the parked run must
   have a harness session id, and the new run's workspace path must be the parked one;
2. `task.ClaimWaiting` (`waiting → claimed`), a new run `<task>-r<n+1>` (`retry_of` the
   parked run, same role) with `ResumeSessionID` = the parked session;
3. `msg.Claim(launch_prompt)` of the answer's deliveries. Only if the claim carries the
   answer does the run exist; the launch then moves the task to `running`, and the
   answer (with its marker) is the resumed session's first prompt.

Anything else is `run.ResumeBlocked`: nothing is written, the task keeps waiting, `otag
needs` lists the answer, and the daemon writes one `run.resume_blocked` warning per
parked run and tries again after a back-off (30 s doubling to 10 min; a cooldown ends by
itself). Nothing starts a fresh run instead; a person fixes the cause and runs `otag run
resume`, or moves the task to `ready` (`otag task edit T --status ready`) so a fresh run
gets the answer in its first prompt. `otag run start` refuses a waiting task.

**First-prompt messages.** Every new run of a task (fresh, retry, fallback) claims the
queued deliveries of its task and of the task's gone runs as `launch_prompt` (up to 10)
and appends them, with markers, to its prompt; a coordinator takes the unrouted `coord`
messages. The launch settles them as sent with the pane; their proof is the marker in
the run's first prompt event. A harness whose launch prompt reaches no hook otag can
scan and has no receipt (OpenCode's TUI `--prompt`, `adapter.LaunchTurnProver`) proves
them with the run's turn start instead: a weaker proof (the turn that started is the one
launched with that prompt; no other injected attempt can be in flight meanwhile). A run that ends before its harness reported anything
(`launch_failed`, stopped while starting) puts them back in the queue (not sent).

**Retry context.** A retry or fallback run's prompt starts with a short summary of the
task's previous run: its ended reason and error, its summary, a review rejection, the
commits on the branch since its base and whether uncommitted changes are left.

**Idle reminders** (`[messages] idle_after`): a `child` or `review` run idle that long
with no open ask and nothing queued for it gets a system `command` message ("finish with
otag done, or ask with otag ask"), at most 2 (`client_msg_id = nudge:<run>:<n>`), each
after another `idle_after`. Then one `run.idle` warning; `otag needs` lists it too.

## Coordinator (phase 4)

`otag coord [<project>] [--profile P] [--attach]` starts the project's coordinator, or
shows (and with `--attach` attaches to) the one that runs. It is a task-less run, role
`coordinator`, id `<prefix>-coord-r<n>`, tmux window `coord`, on `--profile`, else the
project's `coordinator_profile`, else its `default_profile`; Claude only in v1. Its
workspace is the project checkout itself, with the project's guard level. A project has
at most one coordinator that has not ended (checked, and a unique index). While it
runs, the checkout is its own: a same-mode child is refused (children work in
worktrees), and it does not start while a same-mode child works there. It gets its own
system note (the otag commands to coordinate with) instead of the child's. Messages to
`coord` go to the human while no coordinator is live (docs/MESSAGES.md). It ends with
`otag run stop`; the daemon applies the generic pane and cleanup checks to it.

## Review (phase 4)

A task that finishes in a project with a `review_profile` goes to `review` with a linked
review task (`review_of`). That task's runs get `role = review`; their prompt says to
inspect the branch read-only and finish with `otag review <task> --accept ["note"] |
--reject "reason"`. Finishing a review run, with `otag review` or `otag done`, never
creates another review. `otag review` (`run.Manager.Review`) writes, in one transaction,
a `reviews` row linked to the reviewed task's latest `done` run (so to its profile:
`v_profile_stats.reject_rate`) and to the reviewer run or actor, moves the reviewed task
to `done` or back to `ready` ("review rejected: …", which its next run sees), and from a
review run completes that run through the same transaction-aware helper as `otag done`
(`Manager.complete`; nothing opens a second `BEGIN IMMEDIATE`). The human or the
coordinator may review directly: unstarted review tasks are cancelled, one with a live
run is a CONFLICT. `otag done` and `otag fail` close their task's open ask
(`asker_done`, `asker_failed`); cancelling a task closes it as `cancelled`.

`otag stats` shows token usage per reported model (`v_model_usage`); per profile, runs,
tokens, durations, reviews and the reject rate (`v_profile_stats`); and per project,
the delivery wait (`v_task_delivery`, see "Delivery").

## Delivery (phase 5b)

otag never merges: how a result reaches the project (merge, PR, patch) is the project's
business, done by the coordinator, the project's own tooling or the human.

- **`done` means ready to deliver** (after the review accepted it, when the project has a
  `review_profile`). The branch and the worktree stay.
- **Done notice** (`run.NotifyDone`): in the transaction that moves a task to `done`
  (`otag done`, an accepting `otag review`, or `otag task edit T --status done`), an `info`
  message from `system` goes to `coord`, so to the human while no coordinator is live.
  It names the task and title, the branch with the commits since its base (from the done
  run's `commit` output), the worktree, the run and its summary, the accepting review,
  and the deliver command. One per task (`client_msg_id = done:<task>`); none for a
  review task; the body is clipped to the message limit, so a long summary never fails
  `otag done`.
- **`otag task list --deliverable`**: done tasks without a `review_of` edge (review tasks
  are delivered with the task they review). The JSON is the same task list as `task list`.
- **`otag task deliver <task> [--note "..."] [--delete-branch] [--force]`**
  (`run.Manager.DeliverTask`), for the human and the coordinator; a child (or a caller
  with run residue) gets `FORBIDDEN`, as for every command outside the child list:
  1. Refused, changing nothing (`CONFLICT`), while a run of the task or of one of its
     done review tasks has not ended, or while any run that has not ended (any project,
     any role, queued ones too) works in one of their worktrees or under it. A run's
     directory is its latest workspace; a queued run without one will work in the project
     checkout (a coordinator, a same-mode project) or in its own task's worktree path.
     Paths compare by containment with symlinks resolved and case ignored
     (`paths.Within`).
  2. Refused (`CONFLICT`) while one of the worktrees has uncommitted changes, untracked
     files included; `--force` removes it anyway and the changes are lost.
  3. What is left of the ended runs of these tasks, and only of them (pending cleanups,
     their orphan windows), is killed first. If that fails, or tmux is not available so
     it cannot be verified, the delivery is refused with a retryable `UNAVAILABLE`.
  4. One transaction checks step 1 again and moves the task and its done review tasks
     `done → delivered` (`task.Deliver`: CAS, `delivered_at`, the note as
     `status_reason`, a `task.delivered` event; a review task's note is `delivered with
     <task>`). `delivered` is reachable only from `done`, and only this way: `task edit
     --status delivered` is `INVALID_TRANSITION`. After the commit no run can take the
     worktrees before they are removed: a worktree path belongs to its task, a start
     takes only a ready or claimed task, and no project lives inside otag's worktree root
     `<data>/worktrees`: `otag sync` refuses such a project path (symlinks resolved;
     `CONFIG_INVALID`), and a start (child or coordinator) in one synced earlier is
     `INVALID_ARGUMENT`. So a coordinator or same-mode run can never work in a task's
     worktree.
  5. Each worktree is removed with `git worktree remove` only if, with symlinks
     resolved, it is inside otag's worktree root of the project
     (`<data>/worktrees/<project>`) and `git worktree list` shows it as a linked worktree
     on the task's branch `otag/<task>`. A path outside the root, the main checkout,
     anything else that is not such a worktree, and a detached HEAD are refused (the
     worktree `failed`; remove it by hand and run deliver again). A path that is another
     worktree of the repository now (another branch) is not the task's any more: it is
     left alone (`replaced`, with a warning). A path already gone is pruned, and an
     empty directory (what an interrupted removal leaves) deleted (`gone`). Every case
     but `failed` turns the path's `workspaces` rows `cleaned` with `cleaned_at` (event
     `task.worktree_removed`).
  6. The branch stays. The default branch is the local branch `origin/HEAD` names, else
     the branch the project checkout is on (a checkout on the task branch itself says
     nothing about the merge). An unmerged task branch is a warning (`delivered
     unmerged`), not an error. With `--delete-branch` a branch the default branch fully
     contains is deleted (`git branch -d` semantics against that branch): both commits
     are read once, the merge is checked on them, and the deletion is one `git update-ref
     --no-deref --stdin` transaction (`verify` the default branch at its commit, `delete`
     the task branch at its commit), so a branch or default branch that moved meanwhile
     keeps the branch. A symbolic task ref and a branch checked out in a worktree are
     kept too (event `task.branch_deleted` on deletion). An unmerged
     branch, or one whose worktree could not be removed, is kept with a warning.

  The result is `{task, task_status, delivered_at, delivered, review_tasks,
  target_branch, worktrees: [{task, path, result: removed|gone|replaced|failed, error}],
  branches: [{task, branch, exists, unmerged, deleted}], warnings}`; `delivered` is false
  when the task was delivered before. A removal that fails after the commit returns
  that result with a retryable `UNAVAILABLE`: the task stays delivered, and running
  `otag task deliver` again finishes the cleanup (and `--delete-branch`).
- **Analytics**: `tasks.closed_at` is when the task became done, `tasks.delivered_at`
  when it was delivered; `v_task_delivery` has one row per done or delivered task (no
  review tasks) with `wait_s`, NULL until delivered. `v_status_durations` has the same
  wait as the time spent in `done`.

## Usage limits: quota end and cooldown (hook side)

Adapters report limits as `rate_limit` events (Claude `StopFailure{error: rate_limit}`,
OpenCode `provider.rate-limit|quota`, Codex `limitExceeded` errors and
`account/rateLimits/updated` at 100%). The run state machine (`internal/run/state.go`,
`quota.go`) applies them inside the hook transaction, so this works while the daemon is
down:

- Every `rate_limit` records `runs.error`. If it carries a reset time in the future,
  `quota.RecordHit` writes a `rate_limit` quota snapshot and picks the level: Claude and
  Codex accounts (`account_state`) cool down as a whole, other harnesses only the run's
  profile (`profile_state`), until the reset. Cooldowns only grow (`run.SetCooldown`).
- Retries are the harness's job. A `rate_limit` the harness retries by itself is marked
  `data.retry = true` (OpenCode `retry.scheduled`, Codex `error` with `willRetry`, the fake
  harness's `ratelimit` step) and never ends anything.
- **Quota end**: a `turn_end{Reason: error}` that follows a non-retry `rate_limit` in the
  same turn (since the last `turn_start`/`turn_end`, in this hook or an earlier one) means
  the harness gave up. The run ends as `quota` and its sessions are closed. The workspace
  is kept, the task goes back to `ready`, and the limit's target cools down for
  `run.DefaultCooldown` (30 min) unless a known reset already cooled that same target.
  The target is the one `quota.RecordHit` uses (`quota.HitTarget`): the account for
  Claude and Codex, the profile for every other harness, so a 429 on one LiteLLM route
  does not block the other profiles of the account. After the
  commit, the bridge closes the run's windows (its own pane last). Adapters emit exactly
  that sequence when a turn fails on a limit: Claude `[rate_limit, turn_end{error}]`,
  OpenCode `execution.failed`, Codex `turn/completed{failed}`.

## Fallback

A task qualifies when it is `ready` (and unblocked, per the `ready_tasks` view), its
latest run ended with a reason in `fallback_on`, and the daemon has not handled that run
yet. `launch_failed` counts only when `data.cause = "harness"`: the adapter's `Prepare`,
the harness server or sidecar (start, readiness or setup), or a harness that died during
startup. git worktree and tmux failures do not count, because another profile would not
help. A fallback run the daemon started whose launch was interrupted (`cause =
interrupted`, the daemon died mid-start; Launch timeout above) always qualifies: the
chain goes on, and its profile counts as not tried, so it is started again (the
interrupted run still counts as an attempt). For each such run:

1. **Chain**: walk back from the run through `retry_of_run_id` while the run was started
   by the daemon (`runs.fallback_origin_profile_id` set). The chain begins at the first
   run started by a person or a coordinator. Its profile is the **origin**. Fallback is
   **not transitive**: the chain only ever walks the origin's ordered `fallback` list, never
   the fallbacks of a fallback.
2. **Attempts**: once the chain has `max_attempts` runs, the task is blocked.
3. **Next profile**: the first entry of the origin's list that comes after the current
   run's position, has not been tried in this chain, and is available
   (`run.Availability{QuotaThreshold}.Check`, below). Unavailable ones are skipped, and
   the reason is recorded.
4. **Decision**: one `events` row, `source = daemon`, `kind = daemon.fallback`,
   `dedupe_key = fallback:<run>`, so each ended run is handled exactly once. Its data:
   `{run, ended_reason, origin_profile, attempts, profile | block, skipped[]}`. It is
   written in the same transaction as what it decides (the block, or the claim of the
   task and the new run, through `StartInput.Guard`), after checking that the task is
   still `ready`, has the version the daemon saw and the same latest run. If anything
   changed (someone claimed or edited the task, another run started), nothing is
   written and the next tick decides again on the current state. A daemon that dies
   between deciding and starting leaves nothing behind; one that dies after the run was
   created leaves a run without pane, which the launch timeout ends (`interrupted`).
   - **Start**: nothing of the ended run may keep writing to the worktree the new run
     reuses: `Manager.Quiesce` is the run's cleanup (below) with a 15 s limit: every
     recorded window and every recorded process, also one whose window is gone, must be
     gone. If tmux cannot list or kill the windows, a process cannot be verified gone, or
     one survives even SIGKILL, the start is not attempted and counts as a transient
     failure (below). `Start` itself also refuses (retryably, before it creates
     anything) while an earlier run of the same workspace has a cleanup pending that
     fails, or an orphan window. Then the daemon calls
     `Manager.Start{TaskID, Profile, FallbackOrigin, Guard}`. The new
     run is `<task>-r<n+1>`, with `retry_of_run_id` = the ended run, `attempt` = the next
     task attempt, and `fallback_origin_profile_id` = the origin. A `run.fallback`
     notification follows (info). If the start fails:
     - after it created the run (a launch failure), the notification is an error and the
       task stays as `Start` left it. A launch failure caused by the harness is handled
       on a later tick, as the next link of the chain;
     - before it created a run, and the error may go away by itself (old processes
       still running, tmux missing, database busy, any error without a permanent code):
       nothing was decided yet; a `daemon.fallback_retry` event (error, attempt,
       `retry_in`) records the try, and a later tick decides again after a back-off of
       30 s doubling up to 10 min (a restarted daemon restores the count and the
       back-off from the latest `daemon.fallback_retry` event);
     - before it created a run, with a permanent error (profile not found, no adapter
       for the harness, project retired, invalid input), or on the 5th transient one:
       the task is blocked (`… fallback to <profile> could not start: <error>`) with a
       `task.blocked` notification;
     - a task that someone else started or changed meanwhile is left alone.
   - **Block**: the task goes `ready → blocked`, and `status_reason` holds the reason (for
     example `quota after 3 attempt(s) (main > second > third); fallback limit is 3`, or
     `quota on solo and no fallback of solo is available (samef: account f cooling down
     until …)`). A `task.blocked` notification (error) is created in the same transaction.
     `otag task block <id> --clear` puts the task back to `ready`, and the next run is
     started by hand. The daemon does not touch that run's chain again.

`runs.parent_run_id` is kept for coordinator → child links. A fallback is linked by
`retry_of_run_id` plus `fallback_origin_profile_id`.

## Availability: contract for quota collectors (phase 3b)

```go
// internal/run
func ProfileAvailable(ctx context.Context, q db.Querier, profileID int64) (ok bool, reason string)

type Availability struct {
    QuotaThreshold float64          // default 90 (DefaultQuotaThreshold)
    SnapshotMaxAge time.Duration    // default 6h
    Now            func() time.Time // default time.Now
}
func (Availability) Check(ctx, q, profileID) (bool, string)

func SetCooldown(ctx context.Context, q db.Querier, accountID, profileID int64, until time.Time, reason string) error
// accountID 0 = only the profile, profileID 0 = only the account; the implementation
// is quota.SetCooldown (internal/quota cannot import internal/run).

// internal/daemon
type QuotaCollector interface{ Collect(ctx context.Context) error }
// registered with daemon.Daemon{Collectors: ...}; cli/daemon.go builds the Daemon.
```

A profile is **unavailable** when any of these holds:

- the profile or its account is retired, or unknown;
- `profile_state.cooldown_until` or `account_state.cooldown_until` is later than now;
- the latest `quota_snapshots` row (by `observed_at`, then `id`) of some
  `(source, quota_window)` of the profile's account has `used_percent >= threshold` and
  has not reset. The rows considered are account-wide ones (`profile_id IS NULL`) and
  rows for this profile; the latter form a separate series per window. "Not reset" means
  `resets_at` is in the future, or `resets_at IS NULL` and the row was observed within
  `SnapshotMaxAge`.

A database error also counts as unavailable, and the reason says so. Collectors only
write `quota_snapshots`, and `account_state`/`profile_state` cooldowns through
`SetCooldown`. They should fill `resets_at` whenever it is known, and keep `quota_window`
stable per source (`5h`, `7d`, …).

## Notifications

Notifications are rows in `notifications` (`internal/notify`). Kinds written by the
daemon:

| kind | severity | when |
|---|---|---|
| `run.crashed` | warn | pane watcher ended a run as crashed (its pane, or a server/sidecar window, died) |
| `run.launch_failed` | warn | harness died during startup, a launch never recorded its pane, or no first event (`launch_timeout`) |
| `run.cleanup_failed` | warn | an ended run's windows or processes could not be killed (first failure only) |
| `run.stalled` | warn | stall watchdog stopped a run |
| `run.unanswered` | warn | a run has waited `unanswered_after` |
| `run.fallback` | info, or error if the start failed | a fallback run was started |
| `task.blocked` | error | the fallback chain gave up |

```
otag notify list [--all] [--limit N]   # unread (or all), newest first
otag notify ack ID... | --all          # sets read_at
```

`otag status` shows `daemon: running (pid N)` or `not running`, plus the unread count.
JSON: `daemon {running, pid}` and `unread_notifications`.

## Run cleanup and workspace ownership (phase 3.5)

These hold with or without the daemon; the daemon only retries what was left over.

- **Tagged windows**: every window otag creates (main pane, server, sidecars) gets the
  window options `@otag_run` (run id) and `@otag_db` (database path) in the same tmux
  command list that creates it (`new-window -a` after `{end}`, then `set-option -t
  {end}`), so even a launcher killed right after `new-window` leaves a window that can
  be attributed. Windows of older versions are untagged and never touched as orphans.
- **Durable cleanup**: every transaction that ends a run (done, fail, stop, crash,
  quota, exit, launch failure) sets `runs.cleanup_pending`. After the commit, whoever
  ended it runs `Manager.Cleanup`: it takes one process snapshot (macOS `sysctl
  kern.proc`, no `ps`), kills the windows (the caller's own pane last), sends SIGKILL
  to anything still running 2 s later, waits up to 2 s more, and then clears the flag,
  or stores the failure in `runs.cleanup_error` (also in the JSON of `otag done`/`otag
  run stop` as `cleanup_error`, and in `otag run show`) and `runs.cleanup_after`. `otag
  done` run inside the pane dies with it, so the daemon confirms that cleanup.
- **What belongs to a run** (`proc.Family`): a recorded process (pid + birth) with its
  descendants and the rest of its session. tmux makes every pane process a session
  leader, and macOS never reuses a pid while it is still a session id, so children
  reparented to launchd after their parent died are still found; only a child that
  left the session (`setsid`) and whose parent is gone escapes. This needs nothing more
  than the pid and birth that are recorded anyway, which is why it was chosen over
  persisting every descendant. A window is the run's only when its tags name this run
  and database (an untagged one only for a record without a birth, from before v5) and
  the process it runs has the recorded birth. A reused pid or pane, a window of another
  run, this process and its ancestors are never touched. The launch's own cleanup after
  a failure uses the same rules while the windows still run.
- **Process identity**: `sessions.pid_started_at` and `aux_panes[].birth` hold the real
  start time of the process (UTC, ms), read with sysctl right after the window is
  created. v5 clears the older values (recording times); such records are trusted
  only through a live, untagged pane that still runs the pid.
- **Before a start**: `Manager.Start` finishes pending cleanups of earlier runs in the
  same workspace (the task's runs; in same mode every run in that directory) and kills
  their orphan windows. If something survives, the start fails with a retryable
  `UNAVAILABLE` before any run is created. The creation transaction checks again; a
  run of the workspace that ended in between sends Start back to the cleanup (up to
  three times, then a retryable `UNAVAILABLE`).
- **Same mode**: a project with `workspace = "same"` shares one directory; a second
  active run there (any project) is refused with a retryable `CONFLICT` (`… one active
  run per directory`). Directories compare by file identity (device and inode), so case
  and symlink aliases on APFS do not get around it.
- **Cancel and block**: `otag task cancel` and `otag task block --reason` change the task
  and move its active run to `stopping` in one transaction (`run.StopTaskRun`, intent
  `end_reason = stopped`), then stop it like `otag run stop`. The task keeps its new
  status; the run's own `otag done` meanwhile is refused (`run … is being stopped`). If
  the stop fails, the command reports it (retryable) and the daemon finishes it.
  `otag task block --on` only adds a dependency and stops nothing.
- **Unknown is not gone**: a `tmux list-panes` answer that cannot be parsed, a failed or
  cancelled tmux call (including a socket otag may not open), or a process table that
  cannot be read is `unknown`, never "exited"; only "no server running", a missing
  socket or a refused connection mean the server is gone. The Codex supervisor's `ps`
  snapshots run with `LC_ALL=C`; a malformed one is ignored, and while snapshots fail
  the supervisor and its watchdog check the server and the owner one by one (sysctl),
  so they still notice an exit. The supervisor waits at most 5 s for its watchdog.
- **Not persisted**: the `dead_grace` timer of the pane watcher and orphan windows
  starts again after a daemon restart. That only delays a reap by one `dead_grace`
  (5 s by default), so it stays in memory.

## Schema

Migration **v4** (`0004_fallback.sql`) adds `runs.fallback_origin_profile_id INTEGER
REFERENCES profiles(id)`. `ended_reason` has no CHECK, so `stalled` and `quota` need no
migration. No other table changed: the daemon uses `account_state`, `profile_state`,
`quota_snapshots`, `notifications` and `events` from v1.

Migration **v5** (`0005_cleanup.sql`) adds `runs.cleanup_pending` (0/1),
`runs.cleanup_error` and `runs.cleanup_after`, and clears the old
`sessions.pid_started_at` values (recording times, not births). Ended runs with a
recorded pane or aux window are marked pending once, so the daemon checks them after
the upgrade.

Migration **v7** (`0007_guard.sql`) adds `projects.guard` (`light` by default; a project
whose synced config already said `strict` keeps it) and `runs.guard`, the level a run
started with (NULL for older runs, which are strict). See docs/ADAPTERS.md, "Threat model".

Migration **v9** (`0009_lifecycle.sql`) adds the unique index
`runs_one_live_coordinator` (one coordinator that has not ended per project) and the
view `v_model_usage` (run_usage per reported model and harness; the profile's configured
model when a harness reported none).

Migration **v10** (`0010_delivery.sql`) rebuilds `tasks`: status `delivered` and
`delivered_at` (CHECK: one iff the other). It recreates `ready_tasks` (a delivered
blocker is finished) and adds the view `v_task_delivery` (see "Delivery").


## Not in phase 3a

- Run leases and fencing tokens (`lease_*`, `fencing_token` columns are unused). The
  pane check plus the stall watchdog cover the single-machine case.
- Coordinator runs are not watched beyond the generic checks (phase 4 has no
  coordinator resume).
- A stalled or crashed run is not retried unless `stalled`/`crashed` is in `fallback_on`;
  otherwise the task waits in `ready` for a person or a coordinator.
