# Harness adapters

An adapter connects one harness (Claude Code, Codex CLI, OpenCode, ...) to otag. It is
**pure translation** and lives in `internal/adapter/<harness>`:

- `Prepare`: run description → how to launch the interactive session (argv, env, files, optional server).
- `ParseEvent`: one hook invocation → normalized events + what the hook prints.

Everything with side effects is done by otag (`internal/run`): claiming the task, the git
worktree, writing files, tmux windows, waiting for readiness, the database, liveness. An
adapter never opens the database, never writes global harness config (`~/.claude*`,
`~/.codex`, `~/.opencode-work`, `~/.gemini`), and never talks to tmux directly (it gets an
`adapter.Pane` where it needs one).

The reference implementation is the fake harness, `internal/adapter/fake` (a shell script
that reports its steps through `otag event fake <kind>`). Read it next to this document and
`spike/results/<harness>.md`.

## Registering

Add the adapter to `internal/adapter/builtin/builtin.go`. The harness name must also be valid
in `registry.ValidHarness` (`claude|codex|opencode`, plus `fake`). Schema v2 has no CHECK
on harness names, so a new harness needs no migration.

## The interface

```go
package adapter

type Adapter interface {
    Harness() string
    Prepare(ctx context.Context, spec RunSpec) (*LaunchSpec, error)
    ParseEvent(hc HookContext, kind string, payload []byte) (HookResult, error)
    NoopOutput(kind string) []byte
}

// Optional, detected with a type assertion.
type TrustHandler interface { AcceptTrust(screen string) (keys []string, ok bool) }
type ServerSetup  interface { SetupServer(ctx context.Context, spec RunSpec, launch *LaunchSpec) error }
type Stopper      interface { Stop(ctx context.Context, t Target) error }
type SidecarRunner interface { RunSidecar(ctx context.Context, sc Sidecar, args []string) error }
type Deliverer    interface { Deliver(ctx context.Context, t Target, msg Message) error } // phase 4
type ReceiptDeliverer interface { DeliverReceipt(ctx context.Context, t Target, msg Message) (Receipt, error) }
type HookDeliverer interface { RenderDelivery(kind string, base []byte, text string) []byte }

func SidecarProcess(spec RunSpec, name string, args ...string) Process // `otag sidecar` window
```

All types are in `internal/adapter/adapter.go`; field comments there are normative. Guards
use the shared `internal/guard` package (below).

## What `otag run start <task>` does

1. **Create** (one transaction): resolve the profile (`--profile` > task profile > project
   `default_profile`) and its *current* `profile_revisions` row (the run is bound to it), find
   the adapter for the revision's harness (no adapter → error, nothing changes), claim the task
   if it is `ready` (a `claimed` task is taken as is), refuse if the task already has a run
   that has not ended (also a unique index: **one active run per task**), insert the run
   `queued` with `id = <task>-r<attempt>`, `retry_of_run_id` = previous run of the task, and
   point `tasks.current_run_id` at it.
2. **Workspace**: mode `worktree` → `git worktree add` at
   `<data>/worktrees/<project>/<task>` on branch `otag/<task>` from the project's HEAD; an
   existing path is reused only if it is the root of a linked worktree of the project on that
   branch; a deleted directory is recreated from the surviving branch. Whenever the branch
   already existed, the task's first recorded `base_sha` is kept (else `git merge-base`).
   Mode `same` → the project directory. The `workspaces` row is written right away.
   `<data>` is `$OTAG_DATA_DIR`, else the directory of `$OTAG_DB`, else `~/.local/share/otag`;
   all otag paths are made absolute once and passed to children as such.
3. **`adapter.Prepare(ctx, spec)`** with the `RunSpec` below. `RunDir` (`<data>/runs/<run>`,
   0700) and `CacheDir` (`<data>/cache/<harness>`) exist when it is called.
4. otag writes `LaunchSpec.Files` under `RunDir` through an `os.Root` (default mode 0600; `..`,
   absolute paths and symlinks out of `RunDir` are refused; an existing file is replaced,
   never written through).
5. **Transaction**: insert an active `sessions` row (with `LaunchSpec.HarnessSessionID` if
   set), run `queued → starting`, task `claimed → running`. From here on hooks find the run.
6. If `LaunchSpec.Server` is set: start it in a hidden window `<task>~<name>`, poll
   `Server.Ready`, then call `ServerSetup.SetupServer` if implemented (it may still change
   `Argv`, `HarnessSessionID` or `Sidecars`).
7. Start each of `LaunchSpec.Sidecars` in a hidden window and wait for its `Ready` probe, so
   every stream subscription exists **before** the TUI can produce events.
8. Start `Argv` in window `<task>` of tmux session `otag-<project>` on otag's private server
   (`tmux -L otag`, override `OTAG_TMUX_SOCKET`), cwd `LaunchSpec.Cwd` or `spec.Workdir`, env =
   `PATH` (with the directory of `OtagBin` first, so the agent's `otag done` is this otag; it
   is also the tmux client's PATH, since tmux gives a new pane its client's PATH over `-e`) +
   `spec.Env` + `LaunchSpec.Env`. Argv is exec'd directly, never through a shell, and
   no argument is ever interpreted as tmux syntax. Windows keep their pane after exit
   (`remain-on-exit`), so exit codes are observable.
9. If the adapter is a `TrustHandler`: poll the pane (100 ms) until a hook event newer than
   the launch arrives, the pane dies, or `StartupTimeout` (default 20 s) passes. Each poll
   captures the visible text; when `AcceptTrust` matches, otag sends the returned keys, and
   sends them again if it still matches a second later (at most 5 sends): a TUI may draw
   its dialog before it reads input (Claude Code drops keys typed in the first ~0.3 s).
   Once a capture after a send no longer matches, nothing is sent again.
   This is the only screen reading in otag.
10. Attach the terminal unless `--no-attach`, `--json` or stdout is not a terminal.

Every window (server, sidecar, pane) is recorded with its pid on the session right after it
is created, in a transaction that checks the run is still starting. If `otag run stop` got
there first (status `stopping`/`ended`), the launch kills what it started and returns an
error; readiness waits also abort as soon as the run is stopped. Any other failure in 2–8
kills the windows already started, ends the run with `launch_failed` (error text in
`runs.error`) and puts the task back to `ready`.

### RunSpec

| field | meaning |
|---|---|
| `RunID`, `TaskID`, `Project`, `Role`, `Attempt` | identity; `Role` is `child` in phase 2 |
| `Harness`, `Account{Name, ConfigDir, Command, Env}` | frozen from the profile revision; `ConfigDir` is e.g. `~/.claude-work` → set `CLAUDE_CONFIG_DIR`; `Command` is the binary/wrapper (`""` = harness default, e.g. `claude`) |
| `Profile{Name, Model, Effort, Mode, Env, Args, RevisionID, ConfigHash}` | frozen profile; `Args` are extra harness arguments, `Mode` is `interactive\|headless` |
| `Workdir` | agent cwd (worktree or project dir) |
| `ProjectPath`, `Branch` | the repository; `otag/<task>` or `""` in same mode |
| `RunDir` | private dir for this run's files (settings, plugin dir, prompt files) |
| `CacheDir` | per-harness cache shared by runs (e.g. Codex hook trust hashes) |
| `OtagBin` | absolute path of otag; **use it in hook commands** |
| `Env` | `OTAG_RUN_ID`, `OTAG_PROJECT`, `OTAG_TASK_ID`, `OTAG_BIN`, `OTAG_DB`, `OTAG_CONFIG_DIR`, `OTAG_DATA_DIR`, `OTAG_TMUX_SOCKET` (+ `OTAG_TMUX_CONF` when set), all paths absolute. otag puts it on every window it starts (pane, server, sidecars); pass it to any *other* process you start so its hooks resolve the run |
| `Prompt` | first user message: task title, body, extra prompt, how to finish |
| `Instructions` | system-level identity + rules; inject with the harness's mechanism (Claude `--append-system-prompt-file`, Codex `-c developer_instructions=…`, OpenCode plugin `session.hook("context")`) |
| `Deny` | project globs the agent must not write (guards) |
| `ResumeSessionID` | resume this harness session instead of starting fresh (phase 4) |

### LaunchSpec

| field | meaning |
|---|---|
| `Argv` | required; exec'd in the pane |
| `Env` | added on top of `spec.Env` |
| `Cwd` | default `spec.Workdir` |
| `Files []File{Path, Data, Mode}` | written under `RunDir` before anything starts |
| `HarnessSessionID` | when otag picks the id up front (Claude `--session-id <uuid>`, OpenCode: the server-made `ses_…` id, set by `ServerSetup`); else bound by the first `session_start` event |
| `Server *Process` | started before the pane, **one per run** (also for OpenCode, for now); `Ready` probe `tcp:host:port`, `unix:/path`, `file:/path` or `""`; `ReadyTimeout` default 15 s; `URL` stored as `sessions.server_url` |
| `Sidecars []Process` | started after the server and before the pane; otag waits for each `Ready`. Build them with `SidecarProcess` (see "Sidecars"); killed with the run |
| `StartupTimeout` | trust watch duration (TrustHandler only) |

`Prepare` must be deterministic for a given spec and must not write anywhere: return files.
It may run local read-only probes (e.g. `codex app-server` `hooks/list` for trust hashes) and
cache their results under `CacheDir`.

## Hook bridge: `otag event`

```
<OtagBin> event <harness> <kind> [--event-id ID] [--run RUN] < payload
```

- `kind` is adapter-defined; use the harness's own hook name (`PreToolUse`, `session_start`,
  SSE event type, ...). It is stored as `raw_hook_payloads.hook` and passed to `ParseEvent`.
- The run is `--run`, else `$OTAG_RUN_ID` (inherited from the pane/server env). If neither is
  set, otag calls `ParseEvent` with an empty `HookContext` and looks up
  `HookResult.SessionRef` in `sessions(harness, harness_session_id)`; when found it calls
  `ParseEvent` again with the full context. **`ParseEvent` must therefore be pure.**
- One transaction: store the payload row (`raw_hook_payloads`, always, even for unknown runs
  or unparsable payloads), then for each event insert an `events` row `hook.<type>` with
  source `hook:<harness>` and apply it (below). `raw_hook_payloads.event_id` points at the first
  new events row. The row holds a compact extract (ids, tool name, kind; never tool input,
  output, prompts or messages, which carry file contents and secrets) unless `[daemon]
  raw_payloads = true` (docs/DAEMON.md), which stores the payload with secret-named fields
  and secret-looking text redacted and strings clipped at 4 KiB. The daemon deletes rows
  older than `raw_payload_retention` (7 days).
- The hook never migrates the database (`NoMigrate`); the CLI, the daemon and `otag daemon
  install` do. A missing, older or newer schema leaves the bridge without a database.
- **Run token.** Every run gets a secret `OTAG_RUN_TOKEN` in its environment; `runs.token_hash`
  keeps its SHA-256. A hook resolved to a run must carry that run's token, otherwise it is
  not recorded (and a guarded hook is refused). `otag done`/`otag fail` check it too. Runs
  created before the token (no hash) accept any caller for recording and done/fail, but their
  guarded hooks are refused (nothing proves where they come from). The run's own token is
  replaced by `[run token]` in every stored payload.
- **Dedupe**: key = `--event-id`, else `HookResult.EventID`, suffixed `#i` when one hook yields
  several events; `(source, key)` is unique, a repeated key is skipped entirely (no state
  change). No key = no dedupe. Use a key whenever the payload has a stable identity
  (Claude `tool_use_id`+kind, Codex `turn_id`+kind, OpenCode event id); do not hash payloads.
  Usage rows dedupe separately on `UsageData.DedupeKey`: one row per key, and a repeated
  report keeps the largest of each count (a transcript read mid-write may see partial ones).
- **Never fails the hook; guarded kinds fail closed.** The hook always exits 0 and logs
  every problem to stderr and `<data>/hook-errors.log`.
  - Guarded kinds (`adapter.Guarder.GuardKind`: Claude `PreToolUse`/`PermissionRequest`,
    Codex pre-tool and permission hooks, OpenCode `permission.evaluate`, fake `pre_tool`):
    the guard decision is computed before, and independently of, the write, so a failed
    transaction (BUSY, ...) never drops a deny. When the run's policy cannot be applied —
    no database, a schema newer than the binary, a load error, an unparsable payload, no
    run or an unknown one, a run that is no longer active (not starting/idle/running/
    waiting), a foreign token, a panic — the hook prints `DenyOutput(kind, reason)`
    ("… refusing the tool (fail closed)"). The reason reaches the agent.
  - Every other kind keeps the harness-safe no-op: `HookResult.Output`
    (`additionalContext`, ...) is printed only when the run is known and the payload and
    events were committed; otherwise `NoopOutput(kind)`. Make `NoopOutput` exactly what the
    harness treats as "no decision": empty for Claude/Codex command hooks,
    or whatever the harness documents as neutral.
  Hard denies in the harness config (Claude `permissions.deny`, OpenCode content
  `permissions`) and the native sandbox stay in front of the hook.
- **Budget: 5 s from process start**, covering the stdin read (a pipe that never closes is
  cut off) and the database: the hook's connection waits for locks only for the rest of the
  budget. Measured cost of one hook process (fresh `otag` process, open DB, one write
  transaction) on an M-series Mac: **p50 8–10 ms, p90 8–14 ms** depending on machine load
  (`TestRunEventsDriven` logs
  it). Keep `ParseEvent` allocation-light; never do network I/O; local file reads (a
  transcript tail for usage) are allowed but bound them.
- `OTAG_HOOK_TIMING=1` prints the bridge time on stderr.

Wiring examples (paths come from `spec.OtagBin`/`spec.RunDir`):

```jsonc
// Claude --settings <RunDir>/settings.json
{"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command",
  "command": "/abs/otag event claude PreToolUse"}]}]}}
```
```sh
# Codex
codex -c 'hooks.Stop=[{hooks=[{type="command",command="/abs/otag event codex stop"}]}]' ...
```

## `otag prime` and the skill

`otag prime [--json]` prints a short orientation (under 60 lines) for whoever calls it,
chosen by verified context (`run.BuildPrime`): a child run (identity, task and body,
the last review rejection, rules, the commands open to it, unread messages), a review
run (what to review, `otag review <task> --accept|--reject`), a coordinator run
(coordinator commands, task counts, live runs, needs) or, outside any run, the project
found from the working directory (summary and how to coordinate) or the list of
registered projects. Unread messages are listed by id and headline and never marked
read; `otag inbox` reads them. The text reuses the run notes (`Rules`, `ChildCommands`,
`coordGuide`).

Re-priming after a compaction goes through the hook bridge: an adapter sets
`HookResult.Prime`.

- Claude: otag's `SessionStart` hook (no matcher, so it also fires for `compact` and
  `resume`) answers with `hookSpecificOutput.additionalContext` = the prime text
  (`PrimeOutput`; `HookDeliverer.RenderDelivery` renders it). `startup`, `clear` and
  `fork` stay silent.
- OpenCode: the sidecar already forwards `session.compaction.ended`; the bridge queues
  the prime text as a `system` info message to the run (`PrimeMessage`), keyed by the
  frame id (a replayed frame queues nothing), and the normal delivery path (daemon
  dispatcher, OpenCode inbox) injects it.
- Codex: the app-server sidecar forwards `item/completed`; an item of type
  `contextCompaction` queues the same system message. Codex also has `PreCompact` /
  `PostCompact` hook events, which otag does not register: whether their output can add
  context is unverified. The deprecated `thread/compacted`-style notification is not used.

Gaps: a subagent's OpenCode compaction (the sidecar forwards subagent sessions too)
queues a prime message for the run as well; the system message is a normal inbox entry
(shows as unread until read), and it reaches the agent only when the run can take a
message (idle, or at its next hook), not in the compacted turn itself.

`otag skill install|uninstall|status [--force] [--dir HOME]` (human only; children are
refused by the child gate) writes the embedded `SKILL.md` (Agent Skills format) to
`~/.agents/skills/otag/SKILL.md`, the single copy Codex and OpenCode read, a marker file
`.otag-managed` next to it, and the symlink `~/.claude/skills/otag` pointing at that
directory for Claude. otag never installs it by itself. Both files go through an exclusive
random temp file and a rename.

Ownership is a content hash, never a parse of the skill text (the `managed-by` line in
its frontmatter is documentation only). The marker holds
`otag-skill v1 sha256:<hex of the SKILL.md bytes otag wrote>`:
- owned: the marker is a regular file (not a symlink) and its hash equals the current
  `SKILL.md`; install rewrites both when the embedded text is newer (upgrade);
- modified: marker present, hash differs (edited since): status says `modified`, install
  and uninstall refuse with CONFLICT unless `--force`;
- foreign: no marker, or one that is unparsable or a symlink: status says `foreign`,
  install and uninstall always refuse (CONFLICT), `--force` included.

Uninstall removes `SKILL.md`, the marker, the directory if then empty, and the Claude
symlink only if it points at otag's directory. Known limit: a directory swapped for a
symlink between the check and the rename (TOCTOU) is not guarded against; the threat
model here is accidents, not a racing attacker.

## Sidecars: `otag sidecar`

Events that come from a stream rather than hooks (OpenCode SSE, a Codex app-server client)
are read by a sidecar: a hidden window running

```
<OtagBin> sidecar <harness> --ready-file <RunDir>/sidecar-<name>.ready -- [args...]
```

which loads the adapter and calls its `SidecarRunner.RunSidecar(ctx, sc, args)` in-process.
Build the `Process` with `adapter.SidecarProcess(spec, name, args...)`; its `Ready` probe is
the ready file.

- `sc.Ready()` creates the ready file. Call it once the subscription is established; otag
  starts the TUI only after every sidecar is ready.
- `sc.Emit(ctx, kind, payload, eventID)` is `otag event <harness> <kind> --event-id <id>`
  without a process: same run resolution (`sc.RunID` from `OTAG_RUN_ID`), raw payload storage,
  `ParseEvent`, dedupe and state machine. Use the stream's event id as `eventID` so replays
  after a reconnect are deduplicated. Errors are logged to the hook log; keep going.
- Run until `ctx` is cancelled (the window gets SIGHUP when the run ends or stops). On
  reconnect, reconcile what the stream may have missed (OpenCode `GET /api/session/active`,
  pending permissions/forms; Codex `thread/read`) and emit the resulting state.
- The sidecar runs with the run's env and cwd; it must not write outside `RunDir`.

The fake adapter's sidecar (`FAKE_SIDECAR=1`) is the reference: ready, then one `activity`
per argument.

## Normalized events

| type | fields | run status | task |
|---|---|---|---|
| `session_start` | `HarnessSessionID`, `TranscriptPath`, `Reason` (`startup\|resume\|clear`) | `starting → idle`; binds the session id (a *different* id on the same run ends the old `sessions` row as `replaced` and opens a new one on the same pane: Claude `/clear`) | – |
| `turn_start` | – | `starting\|idle\|waiting → running` | `waiting → running` |
| `activity` | `Tool`, `ToolPhase` (`start\|end\|""`) | same as `turn_start` (progress inside a turn; also ends a wait). `ToolPhase` marks a tool call's start (PreToolUse, `item/started`, `tool.input.started`) and end (PostToolUse, `item/completed`, `tool.success\|failed`); stored as `data.tool_phase`, it lets the stall watchdog wait longer while a tool runs (docs/DAEMON.md) | `waiting → running` |
| `turn_end` | `Text` (last message), `Reason` (`answer\|aborted\|error`) | `→ idle` | `waiting → running` |
| `question` | `Text` | `starting\|running\|idle → waiting` | `running → waiting` (reason = question) |
| `permission` | `Text`, `Tool` | same as `question` | same |
| `usage` | `Usage` | – (row in `run_usage`, deduped on `harness:DedupeKey`) | – |
| `error` | `Text` | – (`runs.error`); **does not end the turn**: if the error ends it, also emit `turn_end` with `Reason: "error"` | – |
| `rate_limit` | `Text`, `ResetsAt`, `Data.retry` | `runs.error`; a future `ResetsAt` cools the account (Claude, Codex) or the profile (other harnesses) down until then and adds a `quota_snapshots` row (`quota.RecordHit`, docs/QUOTA.md), also after the run ended. Set `Data["retry"] = true` when the harness retries by itself. A later `turn_end{error}` in the same turn after a non-retry limit ends the run as `quota` (docs/DAEMON.md) | `→ ready` on quota |
| `session_end` | `HarnessSessionID`, `Reason` | closes the session; if no session is left and the run is live: `→ ended (exited)` | `→ ready` |

Every event sets `heartbeat_at` and `first_activity_at` (first time); all but
`session_start`/`session_end` set `last_progress_at`. Runs in `queued`, `parked`, `stopping`
or `ended` only record events (and late `usage`): a late `session_start` does not reopen a
session and a late `error` does not overwrite the run's final error.

One hook may return several events, in order. A failure that ends the turn must be
`[error, turn_end{Reason: "error"}]` (Claude `StopFailure`, OpenCode
`session.execution.failed`, Codex `turn/completed{status: failed}`); otherwise the run stays
`running` until the next turn.

Emit `session_end` only when the harness process is really exiting (Claude `SessionEnd`
with reason `clear` is *not* one). Emit nothing for noise (Claude's internal prompt-suggestion
`SubagentStop`, `*.delta` SSE frames): return no events; the raw payload is still stored.

## State machine

Run statuses (`runs.status`):

| status | meaning |
|---|---|
| `queued` | row created, nothing launched |
| `starting` | pane launched, no `session_start` yet |
| `idle` | harness alive, turn finished, waiting for input (a child that stopped without `otag done` sits here) |
| `running` | turn in progress |
| `waiting` | blocked on a question/permission |
| `parked` | session closed on purpose, resumable (phase 4) |
| `stopping` | `otag run stop` in progress; hooks do not end the run meanwhile |
| `ended` | see `ended_reason` (CHECK: set iff ended) |

`ended_reason`:

| reason | set by | task afterwards |
|---|---|---|
| `done` | `otag done "summary"` on a started run that owns its running/waiting task | `review` + a new linked review task if the project has `review_profile`, else `done` |
| `failed` | `otag fail "reason"` (`runs.error` = reason) | `ready` |
| `stopped` | `otag run stop` on a live pane | `ready` |
| `crashed` | `otag run stop` or the daemon when the pane was already exited/gone (`exit_code` when known) | `ready` |
| `exited` | `session_end` hook without done/fail | `ready` |
| `launch_failed` | `otag run start` failed after creating the run, or the daemon found the harness dead before its first event; `data.cause` = `harness\|workspace\|tmux` | `ready` (daemon fallback when the cause is `harness`) |
| `stalled` | daemon: `running` with no event for `stall_after` | `ready` |
| `quota` | a turn failed on a usage limit (hook, see `rate_limit`) | `ready` (daemon fallback) |
| `superseded` | migration v3 (duplicate active runs) | – |

Task coupling: start `ready → claimed → running`; question `running ⇄ waiting`; done
`running|waiting → review|done`; every other end `→ ready` with a reason. The task is only
touched while `tasks.current_run_id` is this run.

After `done` and `fail` otag closes the run's windows (pane, server, sidecars): nothing keeps
writing to a worktree the next attempt or the reviewer will use. The same holds for a
`session_end` that ends a live run (`exited`): the bridge kills the run's other windows
(a server may outlive its TUI and keep executing) *before* the transaction that releases the
task, and the caller's own pane (`$TMUX_PANE`, e.g. the hook running in the exiting pane, or
the sidecar) after it. Harness transcripts persist,
so a session can be resumed later (`ResumeSessionID`, phase 4). `otag done` run by the agent
from inside its own pane survives long enough to commit and print (it ignores SIGHUP).
On `done` with a review profile, otag creates the review task: `Review: <title>`, status
`ready`, profile = the project's review profile, a `review_of` edge to the reviewed task, and
a body with branch, base/head commits, workspace path and the agent's summary.

Every kill (`stop`, `done`, `fail`, launch cleanup) checks that the pane still runs the pid
recorded for it, so a pane id reused after a tmux server restart is never killed.

Liveness (`internal/tmux.Check`, used by `otag status`, `run list`, `run show`): `alive` (pane
exists, not dead, pid alive), `exited` (pane kept after exit, with `exit_status`), `gone` (no
pane/window/server, or the pane id now runs another pid), `unknown` (no pane recorded / tmux
missing). A live-status run whose pane is not alive is reported **DEAD**. The daemon ends it
(`crashed`) after `dead_grace`. Without the daemon, `otag run stop` ends it the same way.

## Optional interfaces

- **TrustHandler** (Claude): `AcceptTrust(screen)` sees the visible pane text and returns
  tmux key names (`"Down"`, `"Enter"`, `"y"`) when the folder-trust dialog is on screen. Return
  `ok=false` for anything else; otag calls it on every poll and re-sends at most once a second
  while it keeps matching, so derive the keys from the screen's current state (e.g. where the
  cursor is) — sending them twice must be harmless. This is the
  **only** allowed screen read; match the dialog text strictly (it may differ across harness
  versions — return false rather than guess). Prefer per-launch trust flags when the harness
  has them (Codex `-c projects={...}`) and skip this. Never pre-trust by writing the
  harness's global config (`~/.claude.json` etc.).
- **SidecarRunner**: see "Sidecars".
- **ServerSetup** (OpenCode, Codex app-server): called after `Server` is ready, before the
  pane. Typical: create the session over the local API with model/metadata and put its id in
  `launch.HarnessSessionID` and the TUI argv (`-s <id>`).
- **Stopper**: graceful exit before the kill (send `/exit`, API interrupt); otag waits ≤ 3 s for
  the pane to die, then kills the window anyway. Used by `otag run stop` and by the daemon's
  stall watchdog.
- **Deliverer** (phase 4): message delivery into a running session over the harness's API.
  nil = sent (not yet proven), an error wrapping `adapter.ErrNotSent` = definitely not sent
  (retried later, dead after the attempt limit), `adapter.ErrNotReady` (wraps ErrNotSent)
  = not now, the session is there but cannot take text yet (retried soon, no attempt used
  up), any other error = ambiguous (`outcome_unknown`, never re-sent).
  **ReceiptDeliverer** also returns the harness's id for it (`Receipt.Ref`), matched with
  `HookResult.Refs` as proof. Hook-based delivery goes through `HookResult.Slot` and
  **HookDeliverer**. Channels, markers and proofs: [MESSAGES.md](MESSAGES.md).

## Threat model

otag guards against **accidents**: a careless or confused agent (or one following a bad
instruction) that pushes, moves `main`, writes outside its worktree, reads `.env`, or
drives otag or tmux as if it were the coordinator. It does not try to contain a malicious
agent that sets out to escape.

**Guard levels.** A project's `guard` key (`projects/<name>.toml`) is `light` (the
default) or `strict`. It is validated, part of the project's `config_hash` and stored in
`projects.guard`; a run records the level it started with (`runs.guard`, schema v7;
runs from before levels are strict), and `otag status`, `otag run show`, `otag context` and
`otag doctor` show it. One guard (`internal/guard`) serves both: `Policy.Level` selects
the rule set. Light is for agents that must not be slowed down (Go/npm caches, docker,
brew, tools that read env vars); strict is the full phase-3.5 behavior below.

| | light (default) | strict |
|---|---|---|
| `git push` in every form the guard detects (options, path, `git-push`, `sh -c`, `$(...)`, force variants) | denied | denied |
| other refs and checkouts: `update-ref`, `branch -f/-d/-D/-m/-M/-C` and `checkout -B`/`switch -C` on another branch, `worktree remove/prune/move`, `-C`/`--git-dir`/`--work-tree`/`GIT_DIR=` outside the workspace | denied | denied |
| `reset`, `checkout`, `switch`, `restore`, `clean`, `stash`, `commit`, `merge`, `rebase`, `rm`, `mv`, `add`, `apply`, ... run (after a `cd` or `env -C`) in another checkout | denied | sandbox |
| file-tool writes (Write/Edit/MultiEdit/NotebookEdit, `apply_patch`, OpenCode write/edit, MCP tools named like writers) outside the workspace | denied; temp dirs allowed | denied |
| `rm -r`/`rm -rf` outside the workspace | denied; temp dirs allowed | denied |
| other shell writes outside (`>`, `sed -i`, `perl -i`, `cp`, `mv`, `tee`, `dd`, `curl -o`, ...) | allowed | denied |
| deny-glob files (project `deny`; light adds `.env*`): reads and writes | denied | denied |
| what the guard cannot inspect (`$VAR` targets, a shell fed by a pipe, `eval`, git aliases, `submodule foreach`, `rebase --exec`, `config --global`, MCP executor heuristics, unreadable tool input) | allowed | denied |
| `env -C dir cmd` / `--chdir` | followed: `cmd` is checked in `dir` | denied |
| native sandbox | none: Claude settings without `sandbox`; Codex `sandbox_mode = danger-full-access`, approval `never` | Claude sandbox (strict mode), Codex `permissions.otag` |
| environment | otag's environment without secret-looking names (below) | clean allowlist |

`rm -r` of a path whose last component is a symlink checks the link itself, not its
target (rm removes the link). At both levels a `cd` inside `( )`, `$( )` or `<( )` does
not move the commands after it.

**Push backstop (both levels).** With git ≥ 2.31 the run's windows get two git config
entries through `GIT_CONFIG_COUNT`/`KEY_n`/`VALUE_n`, appended after any slots the
environment already has: `url.otag-blocked://.pushInsteadOf=""` (an empty prefix
rewrites every push URL) and `protocol.otag-blocked.allow=never`, so a push started by a
script, a make target or a tool fails inside git; fetch URLs are not rewritten. Older
git is skipped silently. It is best effort: an explicit `remote.<name>.pushurl` or a
longer matching `pushInsteadOf` in the user's or repository's config wins, and an agent
can reset the variables. No hook and no `core.hooksPath` is set, so the project's own
hooks keep working.

**Known light gaps** (accepted to keep light low-friction): git commands that move
another ref through the run's own checkout or arguments (`git switch other && git reset
--hard`, `git rebase main other`, fetch refspecs such as `git fetch origin
main:main`); git commands that remove deny-glob files inside the workspace (`git clean
-fdx` deletes an ignored `.env`, `git restore .env`); pushes past the backstop above;
anything run through scripts, `python -c` and the like.

Both levels keep the run token, the child CLI allowlist (`FORBIDDEN`) and the

`--run`/`--project` refusal, the guard's protection of otag's database, tmux socket and
coordinator commands, all lifecycle work (window tags, cleanup, quiesce, cancel, the
first-event deadline), fail-closed guarded hooks, the 0600 env file, `disableAllHooks:
false`, raw payload redaction and the harness hard denies (Claude `permissions.deny`,
OpenCode config and session denies).

Verified live 2026-10-14 (light, isolated data/config dir and tmux socket, throwaway repo
with a bare remote): Claude Code 2.1.293 `claude-haiku-5-5` ran `go test ./...` with a
fresh build-cache write, `git push origin HEAD` was refused by the hook (the remote kept
only `main`), a Write to `~/…` was refused, `echo > /private/tmp/…` passed, and the agent's
commit plus `otag done` ended the run (settings without `sandbox`). Codex 0.162.1
`gpt-6.1-sol` low: the rollout's `turn_context` shows `approval_policy: never` and
`sandbox_policy: danger-full-access`, the hooks fired, `go test`, the commit and `otag
done` worked; the model refused to attempt a push itself (twice), so the Codex push
refusal is covered by `TestGuardLight` in `internal/adapter/codex` only.

The rest of this section describes strict; light differs as the table says.

- **The write boundary is the harness's native sandbox where it has one.** Claude: Claude
  Code's sandbox in strict mode (below); Codex: `permissions.otag` with only the run's own
  branch ref writable. **OpenCode has no native sandbox**: only the guard (plugin, failing
  closed) and the config hard denies apply there.
- **The guard is best effort**: fast, explanatory feedback in front of the sandbox. It
  parses the shell line itself and does not parse `python -c`, `node -e`, scripts or
  make targets.
- **Hooks fail closed** for guarded kinds (the bridge, above).
- **Child privileges**: a child acts only for its own run. The CLI refuses `--run`/`--project`
  pointing elsewhere inside a run (`OTAG_RUN_ID`), `otag done`/`otag fail` and `otag event`
  require the run's `OTAG_RUN_TOKEN`, and the guard refuses `sqlite3`/any access to otag's
  database, `tmux` on otag's socket (or plain `tmux` from a otag pane) and otag coordinator
  commands (a child may run `done`, `fail`, `task me`, `context`, `status`, `profiles`,
  `task|run list|show`, `notify list`, `prime`, and the messaging commands `ask`, `inbox`,
  `reply`, `send`; the CLI limits `send` to `coord` and `reply` to messages for the child's
  run or task, with the sender taken from the run token). The CLI
  enforces the same list itself inside a run (`FORBIDDEN`), so a script cannot get past the
  shell guard; a coordinator-role run is exempt. The token is
  a speed bump, not isolation: a child can still read the database file and, through the
  sandboxes' DB write grant (`otag done` needs it), write it. There is no daemon broker yet;
  the token is what a later broker would authenticate runs with.
- **Environment**: run windows start from a clean environment (docs/DAEMON.md): `HOME`,
  `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `TMUX_TMPDIR`, `LANG`, `LC_*`, the profile's
  `env_passthrough`, the account and profile `env`, the adapter's variables and `OTAG_*`.
  `GITHUB_TOKEN`, `AWS_*`, other providers' keys and `SSH_AUTH_SOCK` are not passed.
  Passed-through values are part of the run spec, so adapters keep them past their own
  scrub lists (account/profile `env` still wins). The window env reaches the pane through a
  0600 file in the run dir that the pane sources and deletes, never as tmux `-e`
  arguments (which `ps` shows).
  A light run also inherits the rest of otag's environment except otag's own (`OTAG_*`,
  `PATH`, `TMUX`, `TMUX_PANE`) and secret-looking names: `*_TOKEN(S)`, `*_SECRET(S)`,
  `*_KEY(S)`, `*_PASSWORD`, `*_PASS`, `*_PASSPHRASE`, `*_CREDENTIAL(S)`, `*_AUTH`,
  `*_COOKIE`, names containing `PASSWD`, `PASSWORD`, `SECRET`, `API_KEY`, `ACCESS_KEY`,
  `PRIVATE_KEY` or `AUTH_TOKEN`, `AWS_*`, `AZURE_*`, `VAULT_*`, `GH_TOKEN`,
  `GITHUB_TOKEN`, `NPM_TOKEN`, `DATABASE_URL`, ... (`run.secretName`); `env_passthrough`
  brings one back. Inherited variables are not part of the run spec, so the adapters'
  scrub lists (`ANTHROPIC_*`, `CODEX_HOME`, ...) still remove them and the run keeps its
  account and profile.
- **`otag` is the launching binary**: the pane PATH starts with otag's directory, and Codex
  runs commands in a non-login shell (`allow_login_shell=false`), whose PATH a login
  profile cannot reorder in front of it.

## Guards: `internal/guard`

All adapters use the same checks, so behavior does not depend on the harness:

```go
pol := hc.GuardPolicy()   // Level, Workdir, Deny (+ the level's defaults), Branch; for a child also DB, TmuxSocket, OtagBin

pol.CheckWrite(path)      // file tools: Write/Edit/apply_patch targets, write_to_file
pol.CheckRead(path)       // Read/Grep, OpenCode read: deny-glob files and otag's DB
pol.CheckCommand(cmd)     // shell tools: Bash command, run_command CommandLine
pol.CheckToolArgs(tool, input) // unknown (MCP) tools: JSON object input; path-like args pass CheckWrite,
                               // command-like args (command, cmd, script, shell) pass CheckCommand; a tool
                               // named like an executor (exec, shell, terminal, ...) without one is refused
// -> guard.Decision{Allow bool, Reason string}; turn !Allow into the harness's deny output
```

- `CheckWrite`: the path (absolute or relative to `Workdir`) must resolve inside `Workdir`
  (or an `AllowOutside` entry; `/dev/null` etc. always allowed), must not be under `.git`,
  and must not match a project deny glob. Symlinks are followed component by component,
  a dangling one included (its target is checked); a loop or an unreadable link is refused.
  A file with more than one hard link is refused (another name may be outside or
  protected). Globs are gitignore-style: no `/` → any depth (`.env*`, `*.pem`); with `/` →
  anchored (`infra/prod/**`); a matched directory covers its contents.
- `CheckRead`: deny-glob files inside the workspace (they mark secrets too) and otag's
  database are refused; everything else may be read.
- `CheckCommand`: splits the line into simple commands (quotes, `;`, `&&`, `||`, `|`,
  redirections) and denies:
  - `git push` (any global options, `git-push`), git aliases (`-c alias.*`, `git config
    alias.*`), `git config --global|--system`, `submodule foreach`, `rebase --exec|-x`,
    `update-ref`, `worktree remove|prune|move`, `branch -f/-d/-D/-m/-M/-C` and `checkout -B`/
    `switch -C` on any branch but the run's own (`Branch`), and `-C`/`--git-dir`/
    `--work-tree`/`GIT_DIR=`/`GIT_WORK_TREE=` pointing outside the workspace;
  - writes that fail `CheckWrite`: output redirections, `tee`/`rm`/`mv`/`cp`/`touch`/
    `mkdir`/`chmod`/`truncate`/`install` operands and `-t`/`--target-directory`, `sed -i`
    (BSD `-i ''` too), `perl -i`, `dd of=`, `curl -o/--output`, `wget -O/-P`, `ln` targets
    and hard-link sources; a write through a symlink made earlier on the same line;
  - reads of deny-glob files by common readers (`cat`, `head`, `tail`, `grep`/`rg` file
    operands, `< file`, ...);
  - for a child: any word naming otag's database (`$OTAG_DB`, its path, `-wal`/`-shm`),
    `tmux` without `-L` or on otag's socket, otag coordinator commands and `--run`/`--project`;
  - `DenyCommands` patterns.
  `sh|bash|zsh -c` scripts and `eval` arguments are checked recursively; a line with
  `$(...)`, backticks or `$VAR` that mentions a git push or ref change is refused as
  uninspectable. Relative paths resolve against `Cwd` (the shell's current directory if
  the harness reports it: Claude's payload `cwd`, absolute; OpenCode's shell `workdir`,
  which may be relative to `Workdir`; default `Workdir`) and follow `cd`/`pushd` within the
  line (`cd /tmp && touch x` is outside; a `cd` in `( )`/`$( )` ends with it); after a `cd` it cannot resolve (`cd $X`, `cd -`)
  only absolute targets pass. Write targets with `$`/backticks are refused, `~` is expanded.
  `Inside(dir)` tells whether a directory is in the workspace.
- The fake adapter shows the wiring: `pre_tool` with `{"command": ...}` answers
  `{"decision":"deny","reason":...}`.

## Per-harness notes from the spikes

**Claude** — implemented in `internal/adapter/claude`, verified live with Claude Code
2.1.293 (`OTAG_LIVE=claude go test ./internal/adapter/claude -run TestLive -v`; account
`OTAG_LIVE_CLAUDE_CONFIG_DIR`, default `~/.claude-work`, model `OTAG_LIVE_CLAUDE_MODEL`, default
`claude-haiku-5-5`; ~10 s, two short turns).

- `Argv` = `[/usr/bin/env, -u CLAUDECODE -u CLAUDE_CODE_SESSION_ID ..., Account.Command|claude,
  --model M, --effort E, --session-id <uuid> | --resume <ResumeSessionID>, --settings
  RunDir/settings.json, --plugin-dir RunDir/mod, --append-system-prompt-file
  RunDir/instructions.md, --dangerously-skip-permissions, <Profile.Args...>, --, <Prompt>]`.
  `env -u` drops, unless otag, the account or the profile sets them: the variables of a
  parent Claude session (otag is often started from a coordinator's Claude Code, and the
  tmux server keeps that environment), and every variable that picks credentials,
  provider, endpoint, model or effort (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
  `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_CODE_USE_BEDROCK|VERTEX|FOUNDRY`,
  `ANTHROPIC_MODEL`, `CLAUDE_CODE_EFFORT_LEVEL`, ...), which would otherwise override the
  account's login and the profile's `--model`/`--effort`. A gateway (LiteLLM) account sets
  them in its `env`. Without an account `config_dir`, or when it is the default `~/.claude`, `CLAUDE_CONFIG_DIR` is dropped too. `--` keeps a
  variadic profile flag (`--add-dir a b`) from eating the prompt. `--effort` must be one of
  `low|medium|high|xhigh|max`. A profile `--permission-mode`/`--dangerously-skip-permissions`
  replaces otag's bypass flag.
- Env: `CLAUDE_CONFIG_DIR=Account.ConfigDir` (not for `~/.claude`: Claude Code keys its
  Keychain item, `Claude Code-credentials` vs `…-<sha256(dir)[:8]>`, and its global
  config, `~/.claude.json` vs `<dir>/.claude.json`, on whether the variable is set; verified
  with `claude auth status` 2.1.293: unset → logged in, `CLAUDE_CONFIG_DIR=~/.claude` →
  logged out), then `Account.Env`, `Profile.Env`.
- Session id: a name-based UUID (v5) of RunDir+RunID+Workdir+ProjectPath, so `Prepare` stays
  deterministic and two databases never hand out the same id; `HarnessSessionID` is set.
- Settings (one file — Claude Code uses only the last `--settings`): a profile
  `--settings <json|file>` is removed from the args and merged: its keys are kept, otag's hooks
  are appended per event, `permissions.deny` gets otag's rules, `promptSuggestionEnabled` is
  `false` unless the profile sets it (no suggestion model call after every turn, no
  `SubagentStop` noise). `statusLine` is otag's quota wrapper
  (`<OtagBin> statusline --harness claude --account <account> --config-dir <dir>`), which
  records `rate_limits` and then runs the user's own statusLine with the same stdin; a
  profile `statusLine` keeps its other keys and becomes the wrapper's `--command`
  (docs/QUOTA.md). User/project settings hooks still merge in as usual. The profile's
  `Read|Edit|...(/path)` rules (allow/ask/deny) are anchored at their settings source (the
  file's directory; the workdir for inline JSON — permissions docs, "Read and Edit"), so
  they are rewritten to the absolute `//<anchor>/path` before they move into the run dir.
- Hooks (exec form: `command` = OtagBin, `args` = `[event, claude, <Event>]`, `timeout` 10,
  matcher `*` on tool hooks): SessionStart→`session_start` (Reason = `source`;
  `clear`/`resume` with a new id rebinds the run), UserPromptSubmit→`turn_start` + usage
  (covers the previous turn, which has no Stop if it was interrupted),
  PreToolUse→`activity` (`AskUserQuestion`/`ExitPlanMode`→`question`) + guard,
  PostToolUse/PostToolUseFailure→`activity`, PermissionRequest→`permission` except for
  `AskUserQuestion`, Elicitation→`question`, Stop→`turn_end{answer}` + usage,
  StopFailure→`rate_limit` (error `rate_limit`) or `error`, then `turn_end{error}` + usage,
  SubagentStop→only the subagent transcript's usage (Claude Code's own helpers — prompt
  suggestion, `/btw` — also fire it after the turn, with an empty `agent_type` or under
  `--agent` the main agent's, so it never changes state; real subagents show progress
  through their tool hooks), SessionEnd→usage + `session_end` (not for `clear` and `resume`:
  `/clear`, `/resume`, `/branch` switch the session, the process keeps running),
  Notification→raw only.
  Event ids: `<tool_use_id>:<Event>` on tool hooks, `mod:<run>:<turnId>` on the mod's report.
- Mod (`RunDir/mod`, plugin `otag`): `turn.complete` of the main agent (no `agentId`) runs
  `otag event claude mod-turn-complete --run <run>` with the otag env baked in;
  payload `{run, turnId, isAborted, reason, durationMs, answer}` → `turn_end{aborted|answer}`.
  This is the only signal for Esc (verified: `aborted`) and a refused permission. After a
  normal turn it follows Stop, so a turn has two `turn_end` events (harmless).
- Messages (phase 4, [MESSAGES.md](MESSAGES.md)). Busy: PostToolUse(Failure) and
  UserPromptSubmit of the main agent carry them as `additionalContext` (delivered once the
  bridge printed them); a Stop with messages queued answers `{"decision":"block","reason"}`
  (receipt `stop:<session>`, at most 3 blocks per turn, never when nothing is queued). The
  run stays `running`; the next Stop with `stop_hook_active` of that session proves it, a
  turn end without it (Esc: only the mod's `turn.complete`) makes it `outcome_unknown`.
  Idle: the dispatcher writes `RunDir/inbox/<attempt>.json` (only when the mod's
  `inbox/.mod-ready` is at most 5 s old: the mod writes it at `session.start` and on every
  1 s poll tick, so a hung or dead mod gets nothing; else `ErrNotReady`, retried soon
  without using up attempts); the mod polls the directory every second, empties each file
  it takes and `$.prompt.submit`s its text (framed "The otag plugin sent a message"),
  skipping files older than 2 min (below the 3 min mod_submit proof timeout); the
  UserPromptSubmit `prompt` carries the marker: the proof. The claim requires the run to
  be `idle` in its own transaction. A user prompt typed between the claim and the mod's
  submit is a residual race: the mod's submit then follows that turn. Once the run has
  ended and its cleanup is complete, otag removes `RunDir/inbox`. Verified live
  (2.1.293, Haiku 5.5): idle `OK-1` (mod, marker proof), busy `OK-2` (PostToolUse), Stop
  block `OK-3` (continued Stop; the mod's `turn.complete` follows it), and `otag ask` →
  `otag reply` delivered by the mod with the task back to `running`.
- Guards: PreToolUse checks Bash `command` (`CheckCommand`) and Write/Edit/MultiEdit/NotebookEdit
  paths (`CheckWrite`; Claude's own `scratchpad_dir` is allowed; relative Bash paths resolve
  against the payload `cwd` and follow `cd`) and answers
  `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",...}}`
  (verified live: the reason reaches the model, the bare remote stays empty). Hard rules
  in the settings (verified to hold in bypass mode with no hook, including `git -C . push`,
  `git -c push.default=current push` and `/usr/bin/git push`): `Bash(git push)`,
  `Bash(git push *)`, `Bash(git -* push)`, `Bash(git -* push *)` and the same with `*/git`;
  `Edit(//<workdir>/.git)`, `Edit(//<workdir>/.git/**)` and per project deny glob
  `Edit(//<workdir>/**/<glob>)` (no `/`) or `Edit(//<workdir>/<glob>)`, each also with
  `/**`. A rule's `*` spans words, so the option forms also refuse e.g.
  `git -C dir commit -m "... push ..."` (accepted price); quoted (`git 'push'`) and wrapped
  (`sh -c`) spellings are left to the hook guard — Bash rules are not a security boundary
  (permissions docs, "What a Bash rule doesn't match"). Each deny glob also gets
  `Read(...)` rules (secrets; Claude Code applies them to Read/Grep/Glob and, as sandbox
  `denyRead`, to Bash). In "same" mode the `.git` Edit rules are left out: Claude Code turns
  Edit denies into sandbox `denyWrite`, which would stop `git commit` in the project's own
  git dir (the hook guard still refuses Edit/Write on `.git`). The hook also refuses a
  Write/Edit/Bash input it cannot read, Read/Grep of deny-glob files and `mcp__*` tools
  whose path-like arguments point outside the workspace.
- Sandbox (`sandbox.go`; https://code.claude.com/docs/en/sandboxing), strict runs only (a
  light run's settings have no `sandbox` block; a profile's own is kept as given, and
  managed settings that relax the sandbox are not refused): the settings turn on
  Claude Code's own sandbox, the run's write boundary for Bash and everything it starts:
  `enabled`, `failIfUnavailable` (Claude Code exits at startup when Seatbelt cannot start,
  so the run fails to launch instead of running unsandboxed), `allowUnsandboxedCommands:
  false` (the `dangerouslyDisableSandbox` retry, which bypass mode would run unprompted, is
  ignored; from `--settings` this also makes the sandbox admin-required, so repository
  settings cannot loosen it). `filesystem.allowWrite`: the worktree, its own gitdir
  (`<common>/worktrees/<name>`), `<common>/objects`, the run's branch ref (+ `.lock`) and
  reflog dir, otag's DB and its `-wal`/`-shm` (`otag done` runs sandboxed). The per-user temp
  dir is writable by default. In a linked worktree Claude Code also opens the shared
  `.git` (minus `hooks`, `config`), so `denyWrite` lists every other existing branch ref,
  `refs/tags`, `refs/remotes`, `packed-refs` and the main checkout's `HEAD`/`index` and
  other worktrees' admin dirs (a failed ref listing, or more than 2000 branches, fails the
  launch rather than protect only some; branches made later are not covered);
  `filesystem.disabled` is set to `false` explicitly (user settings cannot turn it on). Network: left to the permission mode; in otag's bypass mode
  every host is allowed (a profile with its own permission mode gets prompts). Profiles
  may add (`excludedCommands`, more `allowWrite`, e.g. `~/Library/Caches/go-build` for Go
  tests), never switch `enabled`/`failIfUnavailable` off, set `allowUnsandboxedCommands`
  or `filesystem.disabled`. Built-in file tools and MCP servers run outside the sandbox;
  the permission rules and the hook cover them. `otag done` cannot reach tmux from inside
  the sandbox; the PostToolUse hook that follows (unsandboxed) closes the run's windows.
- Hooks stay on: the settings set `disableAllHooks: false`, which outranks user, project
  and local settings (hooks docs, "Disable or remove hooks"); a profile setting it `true`
  is refused. Only managed settings outrank `--settings`: `Prepare` reads the file-based
  managed settings (`/Library/Application Support/ClaudeCode/managed-settings.json` and
  `managed-settings.d/*.json`) and refuses the run when they set `disableAllHooks`,
  `allowManagedHooksOnly`, or switch the sandbox, its filesystem isolation or its strict mode
  off (`enabled: false`, `filesystem.disabled: true`, `allowUnsandboxedCommands: true`,
  `failIfUnavailable: false`). **Gap**: MDM profiles and
  server-managed settings are not visible to otag.
- Trust dialog ("Accessing workspace … ❯ No, exit / Yes, I trust this folder"): `AcceptTrust`
  matches only the active dialog at the bottom of the screen (footer "Enter to confirm" is the
  last non-empty line, the two options right above it in order with exactly one cursor, the
  title at most 20 lines higher), so stale dialog text with anything drawn below never
  matches. It answers `Down Enter` with the cursor on "No, exit", `Enter` with it on "Yes"; keys typed in
  the first ~0.3 s after the dialog appears are dropped, hence otag's re-send. In the live
  check a second worktree of an already trusted repository showed no dialog (`trust` is then
  empty); Claude Code appears to key trust on the repository.
  The bypass-mode confirmation dialog is not handled: the account must have accepted it once
  (`skipDangerousModePermissionPrompt` in its user settings; it is ignored in `--settings`).
- Stopper: `C-c`, 0.3 s, `C-c` → SessionEnd `prompt_input_exit` (verified with `otag run stop`).
- Usage: on UserPromptSubmit/Stop/StopFailure/SessionEnd (and SubagentStop, its own
  transcript) the transcript is read from the end — 2 MiB, growing 4x up to 32 MiB until the
  window reaches back past the reported turns, so a long turn keeps its early messages; the
  file is opened non-blocking and only if regular (a FIFO cannot stall the hook). Assistant
  messages of the last two prompts become `usage` events (input, output, cache read/write,
  thinking tokens; `<synthetic>` skipped), one per message id; otag keeps the largest counts
  per id, so a read during the write is corrected later. The
  turn that runs `otag done` usually gets its usage from the SessionEnd that follows the window
  kill (late usage still counts). Verified live.
- `kill -9` produces nothing — liveness covers it. Not verified live in this phase:
  StopFailure (fixture from the docs), Elicitation, ExitPlanMode, a real subagent's
  SubagentStop, the statusLine wrapper inside a live session (unit- and CLI-tested; its
  passthrough was checked against the user's real statusLine script).

**Codex** (`internal/adapter/codex`, `spike/results/codex.md`): mode B (server) is
the default; profile env `OTAG_CODEX_MODE="hooks"` selects mode A (`--no-daemon`).
Headless profiles are rejected. Profile args use an allowlist: `--no-alt-screen` and
`--no-daemon`; select model/effort through their profile fields. Arbitrary config,
feature, profile, permission, remote, cwd and bypass flags are unsupported.

Mode B runs a per-run app-server at `RunDir/as.sock`, supervised by a otag sidecar.
`ServerSetup` wires an owning listener which creates/resumes the exact thread with
workspace, model, effort, developer instructions, `approvalPolicy: on-request` and
`permissions: otag`. Metadata-only resume (`excludeTurns: true`, the 0.161.0 schema
field; `thread/start` has none) avoids hydrating full history. The listener
publishes `thread-id`, binds the exact rollout via `session_start`, and reports ready
before the pane starts. The pane signals startup; the listener submits the initial
turn with an explicit policy, then releases the TUI once a rollout exists. CLI 0.161.0
cannot resume an empty thread from another client. Writer conflicts retry for five
seconds while the preceding server shuts down.

Remote resume rejects argv permission overrides. The TUI instead receives a private
`RunDir/tui-home/config.toml` with the account's settings and otag's required defaults;
no account configuration is modified. It attaches through a private Unix-WebSocket
relay (`RunDir/t.sock`) to the exact thread. The relay pins every TUI `turn/start` to
`otag`/`on-request` and `cwd` = workspace, removes competing `sandboxPolicy`,
`runtimeWorkspaceRoots` and `environments` (all three are sticky "for this turn and
subsequent turns"), and records successful TUI subscription. A TUI `thread/resume` loses
`sandbox`, `permissions` and `runtimeWorkspaceRoots` and has its `cwd` pinned; a request
for any other thread closes the relay. Thus typed turns remain constrained even under account defaults of
`never`/`danger-full-access` or a TUI permission change. Requests received before
subscription are tracked by their RPC IDs and resolved with an explicit tool error
asking for a retry after attachment; they cannot remain stuck awaiting an absent UI.
After subscription, the TUI owns approvals/questions; the listener never approves
those requests. Resolved request IDs are removed, and disconnect clears subscription.

**Delivery (mode B, `Deliverer`).** The listener's app-server client is an RPC
multiplexer: one reader goroutine owns the socket, outgoing calls wait for their
response by id (15 s, ctx), server requests (tracked until `serverRequest/resolved`)
and notifications are dispatched apart from responses, and no call path reads the
socket. While a connection is being reconciled, incoming messages are queued and each
call dispatches those that arrived before its response, so reconciliation sees the
stream in order; then the connection goes live and the reader dispatches. Only a
live, reconciled connection (after the initial turn, and again after every reconnect)
accepts deliveries.

The listener serves `RunDir/c.sock` (0600; shorter than `as.sock`, so the sun_path
check covers it) for the life of the sidecar, across reconnects. One JSON line in,
one out, deliveries serialized:

```
→ {"op":"deliver","attempt":"<nonce>","thread":"<thread id>","text":"[otag#d1] ...","steer":true}
← {"ok":true,"kind":"start|steer","turn_id":"<turn id>"}
← {"ok":false,"reason":"busy|busy_mismatch|waiting_dialog|not_ready|rejected|unknown|bad_request","error":"..."}
```

Idle → `turn/start` with the same pins the relay applies to TUI turns (`turnPolicy`:
strict `on-request`/`approvalsReviewer: user`/`permissions: otag`, light
`never`/`dangerFullAccess`; `cwd` = workspace) and `clientUserMessageId:
otag-<attempt>`; the receipt is `result.turn.id`. Busy and `steer` → `turn/steer` with
`expectedTurnId` = the active turn; the receipt is `result.turnId`. Busy without
`steer` → `busy` (retry later). A refused steer (0.162.1: "expected active turn id
`A` but found `B`", "no active turn to steer") is `busy_mismatch`: the attempt fails,
it never falls back to `turn/start`. An open approval/question (`activeFlags` or an
unresolved server request) → `waiting_dialog`; no live connection or no attached TUI
→ `not_ready`. `unknown` means the request was written but not answered (it may have
been delivered); delivered and unknown outcomes are remembered per attempt, so a
repeated attempt is never injected twice. That memory is in-process only and holds the
last 256 attempts: a restarted listener forgets them, so callers must still use a new
nonce per dispatch and never resend an unknown one. An incomplete or oversized
(> 1 MiB) request line is answered `bad_request` (never acted on). `Deliver(ctx, Target, Message)` dials the
socket derived from `Target.ServerURL` (`Message.ID` = attempt);
`codex.DeliverReceipt` also returns `Receipt{Kind, TurnID}`; errors are
`*codex.DeliveryError` (`Unknown()` only for `unknown`). The adapter's
`adapter.ReceiptDeliverer` wraps it: receipt `{api_start|api_steer, turn id}`; `busy`,
`not_ready`, `busy_mismatch`, `waiting_dialog` wrap `adapter.ErrNotReady` (retried soon,
no attempt used up), `rejected`, `bad_request` wrap `adapter.ErrNotSent` (retried with
backoff, dead after the attempt limit), `unknown` and any other reason are ambiguous.
otag's dispatcher steers a running run and starts a turn in an idle one. Mode A has no control socket.
Residual race: a TUI turn starting between the idle check and `turn/start`. The
0.162.1 schema (`turnTrigger`: "ignored when this request steers an already-active
turn") suggests Codex then steers the active turn and the receipt names it; not
verified live.

Mode B also installs `UserPromptSubmit` (`otag event codex UserPromptSubmit`; it maps
to `turn_start`, deduplicated with `turn/started`). Codex fires it for app-server
`turn/start` **and** `turn/steer` input, with the delivered text in `prompt` and
`turn_id` equal to the receipt's turn id (live, 0.162.1). A steer's hook carries the
active turn's id, so its event id equals that turn's start event: a marker scan must
read the payload, not the deduplicated event. Only full raw payloads keep `prompt`.

Live, 2026-10-14, **Codex CLI 0.162.1**, `gpt-6.1-sol` low, strict guard, isolated
data dir/tmux/repo (`OTAG_LIVE=codex-deliver go test ./internal/adapter/codex -run
TestLiveDeliver -v`): idle `[otag#d1] reply OK-1` → `start`, answered `OK-1`; a
delivered turn running `sleep 20` refused a plain delivery (`busy`) and accepted a
steer (`steer`, same turn id), answer `DONE-2 OK-4`; UserPromptSubmit fired with each
marker and the receipt's turn id; every turn's context was `on-request`/`user`/`otag`
with the workspace cwd.

Both modes scrub inherited coordinator/account/provider variables with `/usr/bin/env
-u`, including default-account `CODEX_HOME`, API keys/endpoints, thread identity and
sandbox variables. A runtime prefix filter also removes unknown `CODEX_SANDBOX*`
variables retained by tmux. Explicit account/profile/run env wins. Preflight uses the
same scrub semantics; server, fallback TUI and remote TUI use the launch wrappers.
A bounded stdio `hooks/list` preflight trusts only otag's exact `sessionFlags` commands
through `hooks.state`. Cache keys include CLI version, binary, account home and hook
declarations. Session tracking uses a stable `otag sidecar codex -- track <kind>`
command and `OTAG_CODEX_RUN_DIR`, so run directories do not create new trust cache keys.
No account-home hook trust is written. Server/listener/TUI diagnostics stay in RunDir.
Prepare rejects server socket paths exceeding the portable 103-byte limit.

**Light runs** (guard level light) drop all of the native policy below: the argv and the
TUI home set `sandbox_mode = "danger-full-access"` and `approval_policy = "never"`
(both accepted by 0.162.1), `thread/start` carries `sandbox: danger-full-access` and
`approvalPolicy: never`, and the listener's first turn and the relay pin every TUI
`turn/start` to `sandboxPolicy: {type: dangerFullAccess}`, approval `never` and the
workspace cwd (`turnPolicy`, derived from the thread's parameters). The pre-tool hooks
still run the guard, now with the light rules; no ref is protected natively, and the
reftable refusal does not apply.

Native `permissions.otag` (strict) permits workspace writes plus only what an agent commit and
`otag done`/`otag fail` need, because the coordinator later runs git unsandboxed in the
same repository. In the shared git-common-dir of a linked worktree on its own branch
(`RunSpec.Branch`): `objects`, `refs/heads/<branch>` and its `.lock`, and the branch's
reflog directory (`logs/refs/heads/otag`) — every other ref, `packed-refs` and the main
checkout stay read-only, so the agent cannot move `main` or another task's branch (a ref
that is only packed would be written as a new loose file, which read-only `refs/` also
refuses). A worktree run on a `reftable` repository is refused (one shared ref store cannot
be narrowed to a branch). Same mode or a run without a branch keep the shared
`objects`, `refs`, `logs`, `packed-refs(.lock)` and `reftable` writable. The worktree's own `worktrees/<name>` (HEAD, index,
locks, merge state) is writable except its `commondir`, `gitdir` and `config.worktree`
pointers. Common `config`, `hooks/`, `info/`, `modules/` and other worktrees stay
read-only (`:root`), so `core.hooksPath`/`core.fsmonitor`/hook scripts cannot be planted.
For workspace mode `same` (git-dir = common dir) the directory is writable with
explicit `read` rules for `config`, `config.worktree`, `hooks`, `info`, `worktrees`,
`modules`, `remotes`, `branches` and `commondir`. In the data directory only the DB file
and its `-wal`/`-shm` siblings are writable; `runs/` (other runs, `tui-home`, `tui.sh`)
and `worktrees/` (other tasks) are read-only. Codex resolves overlapping rules by
specificity: a nested `read` inside a `write` root holds, also against rename-over and
not-yet-existing paths; the worktree's `.git` file is read-only by Codex default.
`.codex` and `.agents` stay read-only and command network access stays disabled.
Direct reported git metadata
edits are still rejected by the shared guard. Protected exact/subtree paths use native
`read` access; wildcard globs use native `deny` because **0.161.0 rejects wildcard
`read` rules** (other than trailing subtree `/**`). Those globs also prohibit reads;
this is an upstream limitation, not a claim of write-only enforcement. Leading `/`
is normalized for native project patterns. Native permissions remain the backstop
for writes whose cwd or shell internals are invisible to hooks.

All reported file/patch destinations and shell commands pass through `internal/guard`.
The adapter resolves relative destinations using tool `workdir`, tool `cwd`, then
payload `cwd`, while retaining the workspace boundary and project deny globs. Relative
subdirectories, trailing slashes and equivalent symlink paths are allowed. The shared
guard follows `cd`, checks combined shell flags (`-lc`/`-ec`, `-o opt`), and unwraps
only up to the command word: assignments, `env` (its own options only; `-C` refused in strict, followed in light,
`-S` re-checked, `-u`/`-P`/`-a` values skipped, clusters such as `-iP`), `exec -a`,
`command`, `builtin`, `nohup`, `sudo`, `time`, `nice`, `stdbuf`, `timeout` (and its
duration), `!`, `{`, `if/then/else/elif/do/while/until`; `( )`, `$( )` and `<( )` bodies
are separate commands. xargs value-taking options (GNU and BSD: `-a -d -E -I -J -L -n
-P -R -S -s`) are skipped exactly, and find `-exec` bodies are checked. `push`,
`send-pack`, `http-push` (also as `git-*` binaries) are denied; git `alias.*` config is
rejected only from global `-c`/`--config-env` values before the subcommand. A shell
that reads its commands from a pipe, `<`, `<<`/`<<<` or `-s` (including through
wrappers, `sh -c`, `env -S` and `find -exec`) is refused, as is a script-less
`sh -c` from xargs. Shared
`Policy.CheckPatch` checks add/update/delete/move headers after trimming whitespace,
including patches embedded in any shell command containing `*** Begin Patch`.
Claude and OpenCode use those shared checks too. This is a best-effort textual filter:
expansion, eval, arbitrary scripts and commands constructed from input remain outside
its parsing scope. There is no per-launch native git-push execpolicy; the hook deny
and restricted network provide the available protection. Escalations remain interactive.

`turn/started` maps to `turn_start`; completion maps to `turn_end{answer|aborted|error}`.
Failed turns, terminal errors and thread `systemError` also emit `error`; retrying
errors do not finish the turn. Limit errors emit `rate_limit`. Active wait flags map
to `permission`/`question`; idle alone is not a completion. Tool hooks/items emit
activity, and request-user-input emits the question text. On reconnect, bounded
`thread/turns/list` pages reconcile **all turns since this launch's first turn**; with
no recorded first turn reconciliation is skipped rather than replaying the thread.
The exact rollout supplies missed usage measurements; canonical IDs/cumulative counters
prevent duplicate completions and usage. WebSocket responses have a bounded 128 MiB
limit, with a regression covering the former 16 MiB failure.

Usage records subtract cached input from `inputTokens`, keeping cache reads separate
and consistent with Claude. Reasoning tokens are a **subset of output tokens** and
must not be added to output again. Model comes from the resolved thread response or
rollout `turn_context`, including default-model runs. No cost is invented.

Mode A's stable session tracker publishes the exact rollout and preserves the first
submitted prompt per session before emitting its hook. Rapid second prompts cannot
replace the resume anchor. The tailer skips historical turns, retains partial lines,
resets turn/anchor state when transcript paths change (`/new`/`/clear`), and reads model,
completion/abort, usage and rate limits. Null-answer completions are reported as errors;
provider error details are not persisted in that rollout. Successful Stop and rollout
completions share canonical dedupe IDs. `/exit` waits for Codex's paste interval before
Enter.

Both server and fallback TUI have supervisors. Teardown snapshots descendants,
including PTY/setsid process groups; it signals only owned processes/groups whose
leaders belong to that tree, checks recorded process birth times, and sends kills
before reaping the child. Supervisor and watchdog `ps` snapshots run every 500 ms. SIGINT gets a three-second grace period. An independent
watchdog cleans up after supervisor SIGKILL/panic. There is no deferred kill of a
reaped pgid. Snapshot cleanup is best effort: descendants that double-fork/reparent
before sampling, unavailable `ps`, and sub-second PID reuse are not a kernel-level
containment guarantee. Shared run hooks also retry pane cleanup for already-ended
runs: sandboxed `otag done` can persist completion while lacking tmux IPC, and its next
host-side event closes the recorded windows after committing the event.

Verification, 2026-10-10, **Codex CLI 0.161.0**, `gpt-6.1-sol`, low effort: four small
turns per mode, with a real temporary git worktree, copied authentication, hostile
account defaults in a temporary CODEX_HOME, a dedicated test tmux socket and `/dev/null`
tmux config. No user Codex configuration or user tmux server was modified.

| Live behavior | Mode B (server) | Mode A (hooks) |
|---|---|---|
| Session bind, answer → idle/usage | PASS | PASS |
| TUI-typed second turn | PASS: rollout confirms otag/on-request, low effort, restricted network | PASS: same |
| Operator `otag done` | PASS: 3 panes closed | PASS: 2 panes closed |
| Resume + guard | PASS: same session, prior answer recalled, `.env-smoke` blocked/absent | PASS: same; no historical completion replay |
| Stop | PASS: ended/stopped, task ready, panes closed, server socket unavailable | PASS: ended/stopped, task ready, panes closed |
| Agent commit + `otag done` | PASS: real worktree commit, task done, all panes closed | PASS: same |
| Approval before delayed TUI subscription | PASS: request resolved, TUI later subscribes, turn completes | Not applicable to separate-client handoff |

Run `OTAG_LIVE=codex go test -v ./internal/adapter/codex -run '^TestLiveSmoke$' -count=1`;
add `OTAG_CODEX_LIVE_MODE=hooks` for mode A. Initial acknowledgement and resume prompts
use test adapters with the real run manager; the final agent-finish task uses the
normal CLI instructions. Resume is injected through `RunSpec.ResumeSessionID` because
the phase-2 CLI does not expose `otag run resume` yet.

Recorded protocol fixtures and offline regressions cover all twelve review findings,
including cwd reasons, shell/patch bypasses, environment scrub, argument rejection,
real worktree roots, stable trust caching, large responses, paginated reconciliation,
usage, prompt anchors, policy relay, early request resolution, separate process groups,
supervisor SIGKILL and terminal hook cleanup. `gofmt`, `go vet ./...` and
`go test -race ./...` pass, including Claude/OpenCode. Remaining live gaps: provider
errors/rate limits, approvals/questions **after** TUI attachment, stream reconnection,
independent native-deny testing with hooks disabled, and supervisor crash recovery
(the latter is covered by real local process tests, not an inference run).

Post-review hardening (2026-10-10, after the live run above; Codex quota exhausted until
2026-10-11) is **unit-verified only, not yet run live**: the restricted git/data-dir
native rules, `excludeTurns`, the relay's `cwd`/`runtimeWorkspaceRoots`/`environments`
pinning and TUI-resume stripping, reconcile skipping without an anchor, the 500 ms
`ps` interval, and the shared-guard wrapper/pipe changes. The native rules are also
checked against the real seatbelt with `OTAG_CODEX_SANDBOX=1 go test
./internal/adapter/codex -run TestNativeProfileSandbox` (`codex sandbox -P otag`, no
model call): a worktree commit and DB/WAL writes succeed; common `config`, `hooks/`,
the worktree's `commondir`, another worktree's metadata and files, `runs/`, the data
directory and the worktree `.git` file are not writable. Field names were checked
against `codex app-server generate-json-schema [--experimental]` of 0.161.0. Re-run the
live smoke in both modes before relying on them.

**OpenCode** (`internal/adapter/opencode`, OpenCode v2.0.20; `spike/results/opencode.md`).
Verified live (`OTAG_LIVE=opencode go test ./internal/adapter/opencode -run TestLive -v`, oc
account, `litellm/deepseek-flash-deepseek`, ~25 s, a few thousand tokens):

- **Server** (one per run): `/bin/sh RunDir/opencode/serve.sh opencode serve --hostname
  127.0.0.1 --port P` (free port probed in `Prepare`; `serve.sh` puts `dirname $OTAG_BIN` first
  on `PATH`, because the agent's shell tool runs inside the server and must reach the same
  `otag`). Cwd = `Workdir`. Env: the account's XDG dirs exactly as an account wrapper sets them
  (`XDG_{CONFIG,DATA,STATE,CACHE}_HOME = ConfigDir/{config,data,state,cache}`, plus
  `GH_CONFIG_DIR=~/.config/gh`), a random `OPENCODE_PASSWORD` (also in
  `RunDir/opencode/password`, 0600, for Stop/Deliver) and `OPENCODE_CONFIG_CONTENT` =
  `{plugins: [{package: "file://RunDir/opencode/plugin/otag", options: {otag, run,
  instructions}}], permissions: [hard denies]}`. `OTAG_OPENCODE_BIN` (account/profile env)
  overrides the server binary. Nothing is written to `~/.opencode-work`.
- The recorded `server_url` is `http://127.0.0.1:P#run_dir=<RunDir>`: Stop and Deliver read
  the password from the run dir recorded there, not from the caller's environment.
- **ServerSetup**: waits for `GET /api/info`, checks `GET /api/config` has a layer with otag's
  plugin (the config is never logged: it holds provider keys), creates the session with
  `POST /api/session {title, location: {directory: Workdir}, model, metadata: {otag_run,
  otag_task, otag_project}, permissions: hard denies}` **without an id** (the server makes a
  native `ses_…` id; a made-up one breaks OpenCode Zen), then polls `GET
  /api/plugin?location[directory]=Workdir` until plugin `otag` is `active` (plugins load lazily
  per location; `failed` aborts the launch with the plugin's error). Resume
  (`ResumeSessionID`): `GET /api/session/<id>` on the new server (404 aborts), refuses a
  session whose `location.directory` is not `Workdir`, re-applies today's hard denies
  (`PATCH /api/session/<id> {permissions}`), switches the model if the profile's differs, no
  new session. Verified: a second server reopened the
  first run's session.
- **Pane**: `/bin/sh RunDir/opencode/tui.sh <cmd> --server URL -s <id> [--prompt <Prompt>]
  <Profile.Args...> <Workdir>`. `<cmd>` = `Account.Command` (resolved in `ConfigDir/bin/`
  when it is not on `PATH`: `oc` is a shell alias) or `opencode`. The TUI has no `--model`;
  the model lives on the session. `--prompt` auto-submits once the model catalog is ready.
  `tui.sh` reports the TUI's exit as `otag event opencode otag.tui.exit` (`session_end`):
  OpenCode itself sends nothing when the TUI quits.
- **Sidecar** `events` (`--url --dir --session --model [--resume]`): follows `GET /api/event`
  (Basic auth `opencode:<password>`; response headers and then a line at least every 45 s,
  heartbeats come every 15 s, else the stream is reopened), ready on the first
  `server.connected`, then emits `otag.session` (`session_start`, reason `startup|resume`,
  event id `<run>:<session>:session`). It forwards only the run's session and its subagent
  sessions (`session.created` with a known `parentID`, and on every (re)connect the tree read
  from `GET /api/session?parentID=…`, bounded); `session.execution.*` and `session.deleted`
  only for the run's own session (a subagent's turn is not the run's). Per frame (event id =
  frame id): `execution.started`→`turn_start`, `…succeeded`→`turn_end{answer, Text}`,
  `…interrupted`→`turn_end{aborted}`, `…failed`→`error`|`rate_limit` + `turn_end{error}`,
  `retry.scheduled`→`rate_limit` (`provider.rate-limit|quota`, `ResetsAt` = retry time) or
  `error` (e.g. `provider.transport` while LiteLLM is unreachable), `permission.asked`→
  `permission`, `form.created`→`question`, `permission.replied`/`form.replied|cancelled`/
  `step.started`/`tool.input.started` (tool name)/`tool.success|failed`→`activity`,
  `step.ended|failed`→`usage` (dedupe key = assistant message id), `compaction.ended|failed`
  →`usage` (key `compaction:<frame id>`, the event's own model), source `sse`, cost only when
  > 0; `session.deleted`→`session_end`; deltas, reasoning, inbox, catalog updates→nothing.
  The sidecar adds `"otag": {"text"|"model"}` to `execution.succeeded` (last
  `session.text.ended`) and step frames (the step's model): the stream says these only in
  earlier frames and `ParseEvent` is pure. A failing bridge (locked database) is retried in
  order, the stream waiting; after five attempts the stream is reopened so the reconcile
  restores state. **Reconcile** after a reconnect: the local server keeps no replayable event
  log (`/api/experimental/session/<id>/log` needs `events.persist`, which `opencode serve`
  does not set; verified: it returns only `log.synced`), so the sidecar reads state instead:
  usage of assistant messages completed since the drop (`GET /api/session/<id>/message?
  type=assistant`, same dedupe key as the live frames), `GET /api/session/<id>` (404 →
  `session_end{deleted}`; `outcome` keeps how a turn that ended meanwhile really ended:
  `failed` → `error` + `turn_end{error}`, `interrupted` → `turn_end{aborted}`), `GET
  /api/session/active`, and the location-scoped `GET /api/permission/request` and `GET
  /api/form` (both answer for the server's cwd without `location[directory]`), emitted as
  one `otag.reconcile` that `ParseEvent` compares with the run's status. Per-session state is
  bounded (500 sessions; step models dropped when a turn ends or a session is deleted).
- **Guards**: hard denies in both the config content and the session's own rules (decided
  before any plugin hook, win over the user's `* allow` and saved approvals, and hold when
  otag is unreachable): shell `git push *`, `git -* push *`, `* git push *`, `* git -* push
  *`, `*/git push *`, `*/git -* push *` (options, `env`/`sudo`/`VAR=` wrappers, a git path;
  OpenCode's `*` spans spaces, so a rare false positive such as `git -C . commit -m "a push
  b"` is possible), edit `.git`, `.git/*`, `*/.git/*`, and each project deny glob as edit
  resources widened to a superset (`[...]` → `*`, `**/` and `/**/` → `*` so zero
  directories match too, no `/` → any depth). The plugin's `permission.hook("evaluate")` asks
  `otag event opencode permission.evaluate` for `shell` (resources = the line's simple
  commands, checked as one script from the shell tool's `workdir`, remembered from
  `tool.hook("execute.before")`; a workdir outside the workspace is denied), `edit`
  (location-relative or absolute paths → `CheckWrite`) and `external_directory` (denied for
  edit tools outside the workspace and for a shell whose workdir is outside; reads stay
  OpenCode's decision) and `read` (deny-glob files → `CheckRead`; the config also carries
  `read` hard denies for them). A denial answers `{"effect":"deny","message":"otag guard: …"}` and
  records an `activity` with `{"guard":"deny"}`. The plugin fails closed: otag missing, a
  6 s timeout, a nonzero exit, a signal or an empty answer deny (the bridge answers `{}` for
  "no opinion"). A light run's guard answers with the light rules (a shell `workdir`
  outside the workspace is then allowed). **OpenCode has no native sandbox**: the guard and

  these hard denies are all that applies (threat model above). Verified live: `touch ../x` was refused and recorded; `git push`
  and a deny-glob write were refused by the config rules alone (spike probe).
- **System note**: the plugin's `session.hook("context")` pushes `RunDir/opencode/
  instructions.md` into `system` on every model call (the `instructions` config key is a
  no-op in v2). Verified live: the model answered with the run id from the note.
- **Stopper**: `POST /api/session/<id>/interrupt`, then `C-c` twice in the pane (one quits an
  idle TUI); otag kills the server and the pane afterwards. Verified live: stop in ~0.5 s,
  `otag.tui.exit` recorded. **Deliverer** (phase 4 plumbing): `POST /api/session/<id>/prompt
  {text, delivery: queue|steer, id: msg_… if the message id has that form}`; verified live
  (step 2 of the smoke test is delivered this way). **ReceiptDeliverer** (phase 4): the same
  request with a fresh native `msg_` id per attempt (queue when idle = `api_start`, steer when
  running = `api_steer`); the receipt is that inbox id. The sidecar forwards
  `session.inbox.delivered` of the root session and ParseEvent reports its `data.inboxID` in
  `HookResult.Refs`: the correlated proof (`inbox.enqueued` is not one). 4xx, a refused
  connection, no session or no password are not-sent; a timeout or 5xx is unknown. A
  `delivered` frame missed while the stream was down is not reconciled: that delivery ends
  `outcome_unknown` at its proof timeout. Verified live (2.0.25, DeepSeek flash): idle `OK-1`
  and a steer into a running `sleep` turn (`DONE OK-2`), both proven by receipt.
- `NoopOutput` is empty for every kind.
- TUI quit (verified live): `tui.sh` reports `otag.tui.exit`, the run ends `exited`, the bridge
  kills the server and sidecar before releasing the task, then the pane.
- Gaps: a crashed server produces no event (the sidecar keeps reconnecting; liveness shows
  the pane); `Profile.Effort` is not mapped (use `provider/model#variant`); the TUI may switch
  to another session (`/new`), which otag does not follow; `retry.scheduled` errors that
  happened while the stream was down are not recovered (the turn's outcome is).

## Quota

Adapters report limits only as `rate_limit` events (with `ResetsAt` when the harness says
it); the bridge turns them into cooldowns. Quota levels come from collectors in
`internal/quota` (docs/QUOTA.md), not from adapters, with one exception: a harness whose
quota is only visible in a statusLine payload gets otag's `statusline` wrapper in its
per-launch settings (Claude). An adapter must not read credentials for quota purposes.

## Testing an adapter

- Unit: `Prepare` with a fixed spec (argv, env, files, no writes); `ParseEvent` against the
  payload samples in `spike/results/<h>.md` (and `spike/*/` logs), including noise and broken
  JSON; `NoopOutput` for every kind you wire.
- Integration without the real CLI: copy the pattern of `internal/cli/run_test.go` (temp git
  repo, `OTAG_DATA_DIR`, test tmux socket via `OTAG_TMUX_SOCKET`, a built `otag` binary as
  `OTAG_BIN`) and drive events with `otag event <harness> <kind>`.
- With the real CLI: use a cheap model, a temp repo and a test socket; never touch the user's
  config dirs or tmux server.
