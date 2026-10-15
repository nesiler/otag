# otag: design summary

A single Go binary (launchd daemon + CLI + TUI) running on one Mac. It coordinates AI coding agents running in
several existing projects, on different harnesses (Claude Code, Codex CLI, OpenCode) and models.

## Problem and success criteria
Problem: handing out work to agents on different models and tools across several projects, and tracking it, is
scattered and unreliable.
Success: (1) the state of every child is known at any moment; (2) automatic fallback when a model is unavailable;
(3) coordinators use otag as the only way to start agents; (4) nothing is lost when the service restarts;
(5) integrating a project takes under 5 minutes.

## Glossary
- **Project**: a registered repo; its own CLAUDE.md/AGENTS.md/skills/rules still apply. otag never WRITES files into the repo.
- **Harness**: claude | codex | opencode.
- **Account**: a harness login/config directory (e.g. a second Claude config dir); quota lives here.
- **Profile**: account + model + effort + env + mode (interactive|headless). Defined explicitly (for example a
  `high` and a `med` variant of the same model are separate profiles). Tags (intelligence/speed/cost/context/review)
  are informational only; there is no automatic selection. Every profile has an explicit, ordered fallback list.
- **Coordinator**: the project's long-lived session; itself a run (role=coordinator).
- **Task**: a unit of work. **Run**: a single attempt at a task; fallback/retry = a new run.
- **Message**: question/info/reply/command; `ask --park` closes the session, and the reply resumes it.

## Task lifecycle
`draft -> ready -> claimed -> running <-> waiting -> review -> done -> delivered`; any unfinished state can go to
`blocked` / `cancelled`. `done` = ready to deliver; `delivered` only via `otag task deliver`.
If a run ends early the task returns to `ready`. For the reasons in `fallback_on` (default: `quota`,
`launch_failed`; `stalled` and `crashed` can be added) the daemon starts a new run with the fallback profile. A review is a separate
task linked with `review_of` (ideally a different model).

## Storage
- Desired state in TOML: `~/.config/otag/registry.toml` (accounts, profiles) + `~/.config/otag/projects/<name>.toml`.
  `otag sync` is an atomic, idempotent upsert; every row has a `config_hash`; removed rows get `retired_at` (foreign
  keys stay intact). A run is bound to the profile revision (hash) in effect when it started.
- Live state lives only in SQLite (WAL, `foreign_keys=ON`, `busy_timeout`, `BEGIN IMMEDIATE` for writes, embedded
  numbered migrations tracked with `PRAGMA user_version`; refuse to write to a newer schema). Pure Go driver
  (`modernc.org/sqlite`). The CLI and hooks can write even while the daemon is down.
- Status columns are authoritative (not event sourcing). Every state transition is a CAS update plus an `events`
  insert in the same transaction. `events` has a UNIQUE (source, dedupe_key); raw hook payloads go in a separate table.

## Tables
projects, accounts, profiles (account FK), profile_fallbacks (ordered), profile_state (cooldown), quota_snapshots,
tasks (status, current_run_id, version, external_ref, metadata JSON, extra_prompt), task_deps
(blocks|review_of|parent|related; cycle check; `ready` = a view of ready tasks with no open blockers),
runs (role coordinator|child|review, parent_run_id, lease_expires_at, heartbeat_at, last_progress_at, fencing token,
queued/started/first_activity/ended timestamps, ended_reason), sessions (several harness sessions per run; tmux pane,
server URL, pid + start time), workspaces (worktree path, branch, base/head sha, cleanup), run_outputs
(commit|summary|artifact|diff_stat|test_result), run_usage (tokens/cache/cost, source, measured/estimated),
review result (accept/reject + reason, bound to the reviewed run/profile), messages (+ delivery/ack), notifications,
events, raw_hook_payloads. Every table has created_at/updated_at.

Analytics goal: "which model used how many tokens, whose work was rejected more often in review, which took how long,
how long was each status waited in" must be answerable with a single query.

## CLI
Identity resolution: `--project/--run` -> `OTAG_PROJECT/OTAG_RUN_ID` -> cwd; an ambiguous result is an error.
`otag context`. `--json`: versioned envelope, stable error codes, `retryable`; stdout carries data, stderr
diagnostics; no prompts in automation. Mutations take `--request-id` (idempotency); hook events carry `event_id`.

- Human + coordinator: `status`, `task add|list [--deliverable]|show|edit|cancel|ready|claim|block|deliver`,
  `run start|list|show|stop|attach|resume`, `send`, `inbox`, `reply <msg-id>`, `review <task> --accept|--reject`.
- Child: `task me`, `ask [--park]`, `done "summary"`, `fail "reason"`, `prime`.
- Coordinator: `coord <project> [--profile P] [--attach]`, `run stop --park`, `needs`.
- System: `sync`, `profiles [--tag]`, `event` (hook), `daemon`, `tui`, `stats`, `doctor`, `context`.

## Adapters (experiment results: spike/results/*.md)
- Server-bound: OpenCode (otag's own `opencode serve` + TUI `--server`; SSE + HTTP prompt), Codex (one
  `codex app-server` per pane + TUI `--remote`; otag is a second client).
- Hook-based: Claude (`--settings` hook + `--plugin-dir` mod `$.prompt.submit`).
- Nothing is written to global config. A pane watchdog (kill -9) and a stall watchdog are required. State is keyed by
  run/pane (Claude `/clear` produces a new session_id).

## Isolation / security
One git worktree + branch per task (or the same directory if the project asks for it). Guard: writes outside the
worktree, push/`--force`, secret files, the project's deny list. A child commits and calls `otag done`; merging is
up to the coordinator or a human.

The threat model is **accidents**: it protects against a careless or confused agent, not a malicious agent trying to
escape (details: ADAPTERS.md "Threat model").
- The guard level is a project setting `guard = "light" | "strict"` (default **light**; a run records the level it
  started with). Light: only push, git touching other refs/worktrees/the main checkout, file-tool writes and `rm -r`
  outside the worktree, and reads/writes of deny-glob files (+ `.env*`) are forbidden; everything else is free, there
  is no native sandbox, and the environment is inherited with secrets removed. Strict: the full behavior below. In
  both, the token, FORBIDDEN, lifecycle and fail-closed hooks are the same.

- The write boundary is the harness's native sandbox: in Claude, the Claude Code sandbox (strict,
  `failIfUnavailable`); in Codex, `permissions.otag` (only its own branch ref is writable). OpenCode has no native
  sandbox; only the guard + config deny.
- The guard is best-effort fast feedback; guarded hooks (pre-tool, permission) fail closed.
- A child acts only for its own run: `OTAG_RUN_TOKEN` (hashed in the DB), `--run/--project` forbidden, DB/tmux/admin
  commands forbidden. There is no daemon broker; the token exists so a broker could verify runs later.
- A run's environment starts clean (allowlist + the profile's `env_passthrough`); raw hook payloads are not stored by default.

## Quota
Claude statusLine `rate_limits` (per account), Codex rollout `token_count.rate_limits` / app-server, LiteLLM
`/key/info`. Tokens are never refreshed. A 429/limit error starts a cooldown (account and profile level).

## Roadmap
0 experiments (done) · 1 core (schema, registry/sync, task, context/json, doctor) · 2 run + adapters + hook bridge
+ status · 3 daemon (lease, stall, fallback, quota) · 4 messaging + park/resume · 5 TUI, delivery, skill + prime.
Dropped/deferred (2026-10-14): third-party multiplexer integration (Herdr; the TUI replaces it).

Phase 3a (daemon core, done; details: [DAEMON.md](DAEMON.md)): `otag daemon` (launchd agent
`io.github.nesiler.otag`, single instance via `<data>/daemon.lock`), pane watchdog (dead pane -> `crashed`, harness
dying at startup -> `launch_failed`), stall watchdog (`stalled`), unanswered-question reminders, fallback chain (the
profile's own list, no transitive hops; after `max_attempts` the task becomes `blocked`), quota end on the hook side
(`quota`) + cooldown, `run.ProfileAvailable`, notifications (`otag notify`, osascript). Settings in
`~/.config/otag/otag.toml` `[daemon]`. Schema v4: `runs.fallback_origin_profile_id`. Phase 3b: quota collectors
(`daemon.QuotaCollector`), `otag quota`. Lease/fencing do not exist yet.

Phase 4 lifecycle (details: [DAEMON.md](DAEMON.md) "Park and resume", "Coordinator", "Review"; messaging:
[MESSAGES.md](MESSAGES.md)): `otag ask --park` / `otag run stop --park` / `[messages] park_after` -> the run ends
through the normal stop path with `ended_reason = parked`, and the task stays `waiting`. When a reply is queued the
daemon (or `otag run resume`) resumes the same harness session in a new run with the same profile revision, the same
workspace and an available account (`task.ClaimWaiting`, the reply becomes the first prompt as `launch_prompt`); if
that is not possible the task waits, with `otag needs` + a notification, and no new run is started silently. Every new
run receives the task's queued messages in its first prompt; a retry/fallback run receives a short summary of the
previous attempt. `otag coord <project>`: one live coordinator per project (a task-less run, Claude, in the project
checkout; no same-mode children while it runs). Review runs have `role=review`; `otag review --accept|--reject`
writes a `reviews` row; there is no review chain. `otag done`/`fail` close the task's open question. An idle child gets
at most 2 reminders (`idle_after`). Schema v9: `runs_one_live_coordinator`, `v_model_usage`; `otag stats`.

## Phase 5 plan (2026-10-14, approved)

### 5a `otag tui`
Stack: `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2` (click areas via Lip Gloss v2
layers; no bubblezone). No embedded terminal emulator (`x/vt` is too new). Surveyed: Herdr, claude-squad,
agent-deck, workmux.

Screen: a left list (projects -> tasks -> runs; status, profile, duration, quota), on the right a live preview of the
selected run (`capture-pane -p -e -J`, visible run only, ~4/s), a "waiting for you" counter at the top
(`otag needs`), a message box at the bottom. State is never read from the screen; it comes from the DB (the TUI only
reads and calls the existing CLI/`run.Manager` paths; the daemon is not required).

Three ways to interact with an agent:
1. **Message box**: `otag send` to the selected run/task (Phase 4 delivery, with proof of delivery); direct reply on a
   `needs` row (`reply`), review accept/reject.
2. **Typing mode in the preview**: clicking the preview or pressing `i` sends keys to the agent with
   `tmux send-keys` (including `/compact`, Esc, confirmations); Esc-Esc or clicking outside the preview exits. For
   special key combinations and in-agent mouse use, see way 3.
3. **Full-screen attach**: double click / Enter suspends the TUI and runs `tmux -L otag attach` (`display-popup` if the
   user is inside their own tmux). Return: a single key in otag's tmux config (one that does not collide with agents,
   e.g. M-q) + a clickable "<- otag" in the status bar; on return the mouse/alt-screen state is re-established.

Actions (menu + keys): add/edit/cancel task, start/stop/park/resume run, start with a chosen profile, `coord`, review
accept/reject, delivery, notifications, `stats`. Mouse: click selects, double click attaches, right click opens a
context menu, the wheel scrolls what is under the pointer, the pane divider is draggable; hover is optional. Every
mouse action has a key equivalent.
Tests: `Update` unit tests + end-to-end inside an isolated `tmux -L otag-test` (via `capture-pane`).
Technical design, decisions and steps (T1-T4): [TUI.md](TUI.md); T1-T4 are implemented (Phase 5a done: skeleton,
actions, typing mode + attach, context menus/filter/help/state saving).

### 5b Delivery
otag does not merge; how delivery happens is up to the project (the coordinator, the project's own delivery agent, or
the user).
- `done` = ready to deliver (after acceptance if there is a review). The branch and worktree are kept (they are not
  deleted today either).
- On `done`, an info message goes to `coord` (or to the human when there is no coordinator): task, branch, commit
  count, summary.
- `otag task deliver <id> [--note]` -> new terminal status `delivered` (+ `delivered_at`, event): the worktree is
  removed, the branch stays (`--delete-branch` deletes it only if it is merged into main). Warning for an unmerged branch.
- `otag task list --deliverable` and a "ready to deliver" group in the TUI. Analytics: done -> delivered time.

Implemented (schema v10, details: [DAEMON.md](DAEMON.md) "Delivery"): the notification is a single `info` message
with id `done:<task>` (none for review tasks); delivering a task also delivers its `done` review tasks and removes their
worktrees; it is refused while a run is live or the worktree has uncommitted changes (unless `--force`); if worktree
removal fails after the commit the task stays `delivered` and re-running the command completes the cleanup. Default
branch: `origin/HEAD`, else the branch of the project checkout. `otag stats` shows waiting time per project
(`v_task_delivery`).

### 5c Skill + `otag prime`
- `otag prime`: prints, for the agent in context, who it is, its task, the allowed otag commands and pending messages
  (separate output for child, review, coordinator and a human session outside a run). In Claude a post-compact
  SessionStart hook runs it automatically; in Codex/OpenCode the equivalent is used if there is one, otherwise the
  skill points to it.
- `otag skill install|uninstall|status`: a short SKILL.md (when to use otag + "see `otag prime` for details"), a single
  copy at `~/.agents/skills/otag/` (read by Codex and OpenCode) + a symlink `~/.claude/skills/otag` (Claude). Global;
  installed only by the user's command, otag never writes it on its own. Children in runs do not depend on the skill
  (they get their own run notes).
- Implemented: re-priming after compaction exists in all three harnesses: Claude SessionStart (compact/resume) output,
  and a system message to the run for OpenCode `session.compaction.ended` and Codex `item/completed contextCompaction`.
  Details: ADAPTERS.md "`otag prime` and the skill".

### Order
5c (small, independent) || 5b -> 5a (the TUI also shows delivery/prime). Live tests run only in isolated directories.
