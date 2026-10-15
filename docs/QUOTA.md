# Quota

otag keeps the latest known quota of every account in `quota_snapshots` and puts accounts
and profiles in cooldown when a limit is hit. The code is `internal/quota`; the research
behind the choice of sources is in the reference notes (`kota-okuma.md`).

Rules every source follows:

- **Read-only.** Nothing is written into a harness's config directory (`~/.claude*`,
  `~/.codex`, `~/.opencode-work`, `~/.gemini`), and no token is ever refreshed (refreshing
  would rotate the harness's refresh token behind its back).
- **Passive first.** Local payloads and files before network calls; network calls are
  spaced out and their failure only means "no new snapshot".
- **No secrets** in logs, snapshots or test output. Snapshot `raw` holds a small extract
  (slot, plan, budget duration, the run of a hit), never a token or a key.

## Sources

| account harness | source (`quota_snapshots.source`) | how | windows |
|---|---|---|---|
| claude | `claude_statusline` | `otag statusline` wrapper reads Claude Code's statusLine stdin `rate_limits` | `5h`, `7d`, `spend[-period]` (gateway spend limit) |
| claude | `claude_oauth` | opt-in: `GET https://api.anthropic.com/api/oauth/usage` with the Keychain token | `5h`, `7d`, `7d-opus`, `7d-<model>` |
| codex | `codex_rollout` | tail of the newest `$CODEX_HOME/sessions/**/rollout-*.jsonl`, `token_count.rate_limits` | by `window_minutes`: `5h`, `7d` |
| opencode | `litellm` | `GET <baseURL>/key/info` with the account's LiteLLM virtual key | `budget[-<duration>]` (spend / max_budget) |
| any | `rate_limit` | a run's normalized `rate_limit` event | `limit` (100 %, reached) |

### Claude: statusLine wrapper

Claude Code (≥ 2.1.80, Pro/Max) gives its statusLine command a JSON payload on stdin with
`rate_limits.five_hour|seven_day.{used_percentage (0–100), resets_at (epoch s)}`; behind a
gateway also `spend_limit`. A window is absent until the session's first API response
and dropped once its `resets_at` passes.

`otag statusline --account <name> [--harness claude] [--config-dir DIR] [--command CMD]`
(hidden command):

1. reads stdin (≤ 16 MiB, 2 s), parses the windows (a `used_percentage` above 101 on
   `five_hour`/`seven_day` is dropped: Claude Code has put the `resets_at` epoch there);
2. writes `<data>/quota/statusline-<account>.json` (atomic) and records the snapshot in the
   database with a 300 ms busy timeout. Claude Code runs the statusLine after every
   assistant message, so unchanged values within 30 s write nothing at all, and within a
   minute only the file (the collector later moves the row's `observed_at` forward from
   it). The database is opened **without migrations** (`db.Options.NoMigrate`): a
   missing database or an older schema is never created or migrated from this hot path
   (no write lock); then only the file is written, silently, and the daemon's collector
   ingests it after the daemon migrated. Any other database failure only logs;
3. runs the user's own statusLine through `/bin/sh -c` with the same stdin and prints its
   stdout unchanged: `--command` if given, else the first `statusLine.command` found in
   the payload project's `.claude/settings.local.json`, `.claude/settings.json`, then
   `<config dir>/settings.json` (`--config-dir`, else `$CLAUDE_CONFIG_DIR`, else
   `~/.claude`). No statusLine configured → prints nothing. The command runs in a
   process group of its own; when the wrapper's 10 s limit ends, the whole group is
   killed, so a child that inherited stdout (`sleep 3600 & wait`) cannot keep the
   harness waiting for the end of the output;
4. always exits 0. Problems go to `<data>/hook-errors.log` only.

The status file is published under a per-account `flock` (`statusline-<account>.json.lock`,
at most 1 s) and never replaced by an older observation, so concurrent statusLine calls
that finish out of order keep the newest one.

Recursion guards: the wrapped command runs with `OTAG_STATUSLINE_DEPTH=1` (a nested
`otag statusline` prints nothing), and otag never runs itself: a looked-up command that is
itself the wrapper (you opted your own sessions in, below) is unwrapped. The wrapper is
`otag statusline …` in command position, after any `VAR=value`, `exec` or `env`; "otag
statusline" as an argument, as in `printf '%s %s' otag statusline`, is your own command. Its
`--command` value (shell-quoted, `--command X` or `--command=X`, unwrapped again if it
is a wrapper too, up to 5 levels) is what runs; a wrapper without `--command` wraps
nothing, so nothing is printed and nothing is logged. This is what makes the opt-in
below work together with otag runs, whose wrapper looks up the same settings file.

**otag runs**: the Claude adapter puts the wrapper into the run's `--settings` file
(`statusLine` from `--settings` replaces the user's, hence the wrapping):
`<otag> statusline --harness claude --account <account> --config-dir <dir>` (no
`--config-dir` for the default `~/.claude`, which runs with `CLAUDE_CONFIG_DIR` unset). A profile
`--settings` with its own `statusLine` keeps its other keys (`padding`, ...) and its
command becomes `--command`.

**Your own interactive sessions (opt-in, manual).** otag never edits your settings. To let
otag see the quota of sessions you start yourself, change `statusLine.command` in each
account's `settings.json`, keeping your original command after `--command`:

```jsonc
// ~/.claude/settings.json (account claude)
"statusLine": {"type": "command",
  "command": "/abs/path/to/otag statusline --account claude --command 'sh ~/.claude/statusline-command.sh'"}
// ~/.claude-work/settings.json (account claude-work)
"statusLine": {"type": "command",
  "command": "/abs/path/to/otag statusline --account claude-work --command 'sh ~/.claude/statusline-command.sh'"}
```

Your status line looks exactly as before (verified byte for byte with the shared
`statusline-command.sh`; ~60 ms per call including the database write). To undo, put the
original command back.

### Claude: OAuth usage (optional, off)

```toml
[accounts.claude-work.quota]
oauth_usage = true
```

Every collector round, if the statusLine has not observed the account for 10 minutes and
the last call is ≥ 5 minutes ago, the collector reads the access token with
`/usr/bin/security find-generic-password -s <service> -w` (read-only) and calls
`GET https://api.anthropic.com/api/oauth/usage` with `anthropic-beta: oauth-2025-04-20`
and a Claude Code user agent (`claude-cli/<claude --version> (external, cli)`; the
endpoint answers 429 without one). Service names: `Claude Code-credentials-<first 8 hex
of sha256(config dir)>` (Claude Code uses the suffix whenever `CLAUDE_CONFIG_DIR` is set;
e.g. `/Users/u/.claude-work` → `-87169a1d`); for `~/.claude` first the plain
`Claude Code-credentials` (what Claude Code and otag runs use with the variable unset),
then the suffixed one. An expired token is skipped (Claude Code renews it on its next
use). 429 waits for `Retry-After` (default 5 min), 401/403 for 30 min.

Caveats: the endpoint is undocumented and may change; the first Keychain read from a new
binary may show a macOS access dialog, and Claude Code's own refresh may reset the
item's ACL. Keep it off unless the statusLine is not enough.

### Codex: rollout files

`$CODEX_HOME` = the account's `config_dir`, else `$CODEX_HOME`, else `~/.codex`. Each
round looks at the 5 most recently modified `rollout-*.jsonl` in the 3 newest
`sessions/YYYY/MM/DD` directories (a session keeps writing to the file of the day it
started). A file is read once: the first time its last 2 MiB, then only what was appended
(offset cached per file; a partial last line waits for its newline; more than 8 MiB
unread or a truncated file → read the tail again). Lines with `"type":"token_count"` and
`rate_limits.limit_id` `"codex"` (or empty) with at least one window count; other buckets
(e.g. `"premium"` with null windows, which recent Codex versions write after the main
one) are skipped. The newest line wins; its own `timestamp` is the snapshot's
`observed_at`, so an old rollout is shown as old. `rate_limit_reached_type` marks the
fullest window as reached.

The Codex adapter's app-server listener already forwards `account/rateLimits/updated`
notifications to the bridge; the adapter turns them into a `rate_limit` event (with the
reset time) only when a window is full or `rateLimitReachedType` is set, which then cools
the account down (see below). Non-limit updates are not recorded from there: the rollout
carries the same numbers.

### LiteLLM (OpenCode accounts)

The account's OpenCode config is `<config_dir>/config/opencode/opencode.json[c]` (the
wrapper layout, `XDG_CONFIG_HOME=<dir>/config`), else `~/.config/opencode/`. The provider is
`quota.litellm_provider`, else `litellm`, else the only provider whose id or name
mentions LiteLLM. `options.baseURL` minus a trailing `/v1`, `options.apiKey` literal,
`{env:VAR}` or `{file:path}` (relative to the config file, `~` expanded; in the daemon,
`{env:VAR}` only resolves if `[daemon] env_passthrough` lists VAR at `otag daemon
install`, and `otag doctor` warns otherwise; docs/DAEMON.md). Every 2
minutes: `GET <base>/key/info` (5 s timeout) → `info.spend`, `info.max_budget`,
`info.budget_reset_at`, `info.budget_duration`; `info.blocked` counts as reached. Without
a `max_budget` only the spend is stored (`used_percent` NULL). Unreachable, 4xx/5xx or a
config without LiteLLM → no snapshot, reported as a problem (`otag quota --refresh`
prints it as a warning), not an error.

### Rate-limit events

A normalized `rate_limit` event (Claude `StopFailure` with `rate_limit`, Codex
`usageLimitExceeded` / full `account/rateLimits/updated`, OpenCode
`provider.rate-limit|quota`) is passed by `run.Ingest` to `quota.RecordHit`, in the same
transaction, also when the run has already ended. Only a hit that **names a future reset
time** (`ResetsAt`: Codex rate-limit updates, OpenCode `retry.scheduled`) records
anything:

- Claude and Codex accounts cool down as a whole (subscription windows span every
  model); for other harnesses the run's **profile** cools down (a LiteLLM route is
  per model). Until = `ResetsAt`. `RecordHit` writes only the
  snapshot and returns the target; the state machine sets the one cooldown through
  `run.SetCooldown`.
- A snapshot `source=rate_limit, window=limit, used_percent=100, resets_at=ResetsAt,
  raw.reached=true` is written (with `profile_id` for profile-level hits), so readers of
  the latest snapshots see the hit too.

A hit without a reset records nothing here: harnesses retry transient 429s themselves,
and a turn that really fails on a limit is the run state machine's business: the run
ends `quota` and the same target as above (`quota.HitTarget`: account for Claude and
Codex, profile otherwise) cools down for 30 min (`run.DefaultCooldown`, docs/DAEMON.md)
unless a known reset already cooled that target. An exhausted window that a statusLine
or rollout showed has already set its own cooldown until its reset.

## Storage

`quota_snapshots` (schema v1, unchanged): per (account, profile, source, window) the
latest row is **touched** (`observed_at` moves forward) while used/limit/resets/raw stay
the same; a change inserts a new row; an observation older than the latest row of its
window is dropped. The collectors prune rows older than 30 days once a day, keeping the
latest row of each window.

Cooldowns go to `account_state` / `profile_state` through `quota.SetCooldown` (which is
`run.SetCooldown`): an exhausted window (`used_percent ≥ 100` or reached) with a future
reset extends the cooldown to the reset; it is never shortened. `cooldown_reason` and
`last_error` say which window and source.

The daemon's availability check (`run.ProfileAvailable`, docs/DAEMON.md) also blocks a
profile while the latest snapshot of a window is at or above `quota_threshold` (90 %).

## Reading quota (for the daemon and fallback)

- `quota.List(ctx, q, Filter{Account, MaxAge}, now)` → latest row per window with
  derived `Stale` (older than 1 h), `ResetPassed` (the window has rolled over since; the
  used percent no longer applies, `Effective()` returns 0) and `ResetsInSeconds`.
- `quota.Cooldowns(ctx, q, now)` → active account and profile cooldowns.
- A snapshot without `resets_at` (LiteLLM without a budget period) can only
  age; treat a stale one with care.

## Collectors

```go
type Collector interface { Collect(ctx context.Context) error }
c := quota.New(db, quota.Options{Getenv: os.Getenv}) // keep one value for the daemon's lifetime
err := c.Collect(ctx)                                // one round over all active accounts
c.Problems()                                         // last soft failure per account
```

`otag daemon` registers one such value and runs it in a goroutine of its own (never
delaying the pane and stall checks), first at start and then every `quota_interval`
(`[daemon]` in `otag.toml`, default 60 s). `Collect` returns only
database errors; missing files, unreachable endpoints and expired
tokens are soft (no snapshot, `Problems`). Per round: claude → ingest the statusLine file,
OAuth if enabled; codex → rollouts; opencode → LiteLLM (2 min spacing). `accounts.<name>.quota.disabled = true` skips an account.

## CLI

- `otag quota [--refresh] [--account A] [--all] [--json]`: one row per account, window and
  source: `ACCOUNT WINDOW USED RESETS IN SOURCE AGE` (`(stale)` after 1 h; a rolled-over
  window shows `0% (was N%)` and `passed`), then active cooldowns. `--refresh` runs one
  collector round first. Snapshots older than 7 days are hidden unless `--all`.
- `otag status` ends with a `QUOTA` section (one line per account, windows not yet
  rolled over) and the cooldowns; `--json` has `quota` and `cooldowns`.

## Verified on this Mac (2026-10-10, read-only, temporary database)

- Codex: `~/.codex/sessions` → `5h 100 %` (resets 22:13 UTC), `7d 46 %`, observed 19:03:55
  UTC from the last `codex` bucket line, although the newest rollout holds only
  `"premium"` lines; the account went into cooldown until the 5h reset. One round over all
  accounts took ~0.25 s.
- LiteLLM (opencode): `{file:~/.opencode-work/litellm-key}` resolved, `/key/info` →
  `budget-30d 16 % ($15.63/$100)`, resets 2026-11-01.
- Claude: both accounts' `settings.json` use `sh ~/.claude/statusline-command.sh`; the
  wrapper with a sample payload printed byte-identical output, recorded `5h`, dropped a
  bogus `seven_day` value, and refused to run itself from a settings file. The real
  statusLine payload of a live session and the OAuth endpoint were not exercised (OAuth is
  off by default; a Keychain read may prompt).
- Keychain service names exist as computed (`Claude Code-credentials`,
  `Claude Code-credentials-87169a1d`); only names were listed, no secret was read.
