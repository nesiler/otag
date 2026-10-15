# otag

A command tent for your AI coding agents.

[![CI](https://github.com/nesiler/otag/actions/workflows/ci.yml/badge.svg)](https://github.com/nesiler/otag/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/nesiler/otag)](https://github.com/nesiler/otag/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/nesiler/otag)](go.mod)
[![License: MIT](https://img.shields.io/github/license/nesiler/otag)](LICENSE)
![Platform: macOS](https://img.shields.io/badge/platform-macOS-lightgrey)

<!-- TODO: docs/assets/tui.gif -->

## What is otag?

otag coordinates AI coding agents (Claude Code, Codex CLI and OpenCode) on your own Mac, across one or more
existing projects. A coordinator agent splits the work into tasks; child agents work on them in isolated git
worktrees inside a private tmux server; you watch and steer everything from one terminal UI.

- **Tasks and runs.** A task is a unit of work with a status machine (draft, ready, running, waiting, review,
  done, delivered). Every attempt is a run. When a run hits a quota limit or fails to launch, the daemon starts a
  new run with the profile's fallback; crashed or stalled runs fall back too if you add them to `fallback_on`.
- **Isolation.** One git worktree and branch (`otag/<task>`) per task. otag never writes into your project's
  working tree.
- **Review flow.** A finished task can spawn a linked review task, ideally on a different model; the reviewer
  accepts or rejects.
- **Messaging with proven delivery.** Agents and humans exchange questions, replies and commands (`otag send`,
  `otag ask`, `otag inbox`, `otag reply`). A child can park itself while waiting for an answer and is resumed when
  the reply arrives.
- **Guard.** Hooks block dangerous actions: `git push`, git operations on other refs or on the main checkout,
  writes outside the worktree, and deny-listed files such as `.env*`. Two levels: `light` (default) and `strict`
  (behind the harness's native sandbox where available). The guard protects against accidents, not against a
  malicious agent.
- **Quota tracking.** Per-account quota windows, cooldowns and profile fallback (`otag quota`).
- **Delivery.** otag never merges. A done task is "ready to deliver"; each project delivers its own way (the
  coordinator, a delivery agent, or you), then `otag task deliver` marks it delivered and removes the worktree.
- **Terminal UI.** `otag tui` is mouse-driven with a live pane preview, a typing mode that forwards keys to the
  agent, and full-screen attach.

State comes from harness hooks and a SQLite database, not from screen scraping.

## Why "otag"?

*Otag* (Turkish *otağ*) was the command tent of a Turkic or Ottoman ruler on campaign: the headquarters from
which the campaign was coordinated.

## Requirements

- macOS (Apple Silicon or Intel)
- `tmux` and `git`
- at least one agent CLI: `claude`, `codex` or `opencode`

## Install

Homebrew:

```sh
brew install nesiler/tap/otag
```

Install script (honors `OTAG_VERSION` and `OTAG_INSTALL_DIR`):

```sh
curl -fsSL https://raw.githubusercontent.com/nesiler/otag/main/install.sh | sh
```

From source (pure Go, no cgo):

```sh
go install github.com/nesiler/otag/cmd/otag@latest
```

Then install the background service (a launchd agent, `io.github.nesiler.otag`) and check your setup:

```sh
otag --version        # otag 0.1.0 (<commit>, <date>); also: otag version [--json]
otag daemon install
otag doctor
```

## Quick start

1. Describe your agent accounts and profiles in `~/.config/otag/registry.toml`. Copy
   [examples/registry.toml](examples/registry.toml) and edit it.

2. Register a project in `~/.config/otag/projects/<name>.toml`:

   ```toml
   path = "~/dev/projects/myapp"
   prefix = "app"                        # task ids: app-x3f

   coordinator_profile = "claude-opus-high"
   default_profile = "claude-sonnet-high"
   review_profile = "codex-gpt-high"     # optional; reviews should use a different model

   workspace = "worktree"                # worktree | same
   guard = "light"                       # light | strict
   deny = [".env*", "*.pem"]
   ```

   See the files in [examples/projects/](examples/projects/) for all keys.

3. Validate and load the configuration:

   ```sh
   otag sync
   ```

4. Start the project's coordinator (add `--attach` to jump into its window), then open the TUI:

   ```sh
   otag coord myapp
   otag tui
   ```

The coordinator creates tasks with `otag task add` and starts children with `otag run start <task>`; you can do
the same by hand. Useful commands: `otag status`, `otag needs` (what waits for you), `otag inbox`, `otag quota`,
`otag stats`, `otag notify list`.

Optional:

- `otag skill install` installs a short global skill so Claude Code, Codex and OpenCode know when to use otag.
- `otag prime` prints orientation (identity, task, allowed commands, unread messages) for the calling agent.

Add `--json` to any command for a versioned machine-readable envelope.

## Concepts

| Concept | Meaning |
|---|---|
| Project | A registered repo. Its own CLAUDE.md / AGENTS.md rules still apply; otag writes no files into it. |
| Task | A unit of work with a status and dependencies (`blocks`, `parent`, `review_of`, `related`). |
| Run | One attempt at a task by one profile (harness + account + model + effort). Retry or fallback means a new run. |
| Coordinator | The project's long-lived agent session that plans and creates tasks. |
| Child | An agent run working on one task in its own worktree; finishes with `otag done` or `otag fail`. |
| Review | A separate task linked with `review_of`; recorded with `otag review --accept` / `--reject`. |
| Guard | Hook-based blocking of dangerous actions, `light` or `strict`. |
| Delivery | Getting a done task's result into the project, done by the project; `otag task deliver` records it. |

## Documentation

- [Design](docs/DESIGN.md): goals, data model, decisions
- [Daemon](docs/DAEMON.md): watchdogs, fallback, park and resume, review, delivery
- [Adapters](docs/ADAPTERS.md): Claude Code, Codex and OpenCode integration, threat model
- [Messages](docs/MESSAGES.md): messaging and delivery proof
- [Quota](docs/QUOTA.md): quota collectors and cooldowns
- [TUI](docs/TUI.md): terminal UI design

## Status

Early alpha (v0.1), macOS only. Interfaces and the database schema may still change.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

See [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
