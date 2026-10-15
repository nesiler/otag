# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Merge/pull request flow primitives (mechanisms only; the workflow stays the project's): see
  [docs/DAEMON.md](docs/DAEMON.md) "MR/PR flow".
- Project settings `base`, `branch_prefix`, `remote` and `push` (all optional). A remote-tracking `base`
  (`origin/dev`) is fetched before a task branch is created; a failed fetch fails the run's start.
- `otag task push <task> [--force]`: otag pushes the task branch to the project's remote (the same branch name,
  no other refs, no tags). Who may push follows `push = "none" | "coordinator" | "child"` (default
  `coordinator`: the project's coordinator and you). `--force` is `--force-with-lease` against the commit otag
  pushed last. The push is recorded on the task (`task show`, TUI details, done notice) with a `task.pushed`
  event.
- `otag task reopen <task> "reason"`: a done task goes back to ready on the same worktree and branch; the next
  run gets the reason, an accepted review goes stale (the task is reviewed again), and the done notice is
  withdrawn. Delivered tasks cannot be reopened.
- `otag task add/edit --base <ref>` (until the task's worktree exists) and `--review <profile|none>`.
- `otag task deliver --delete-branch --force` deletes a branch that is not merged (squash merges).
- Setup wizard and `otag project add` ask for the branch prefix, the base (with the repository's remote
  default branch as the suggestion) and the push setting; `project add` takes `--branch-prefix`, `--base`,
  `--push` and `--remote`.
- TUI: Push… (`P`) and Reopen… (`O`) on tasks.

### Changed

- The default task branch is `feature/<task>` (was `otag/<task>`); set `branch_prefix` to choose another.
  A task keeps the branch it got when its worktree was first created: existing tasks keep `otag/<task>`.
- A review run works in a detached worktree at the reviewed commit instead of a branch of its own from the
  project's HEAD; its prompt names the base, the head and the diff command.
- The guard's `git push` refusal points to `otag task push`. Raw `git push` stays refused for every agent.
- Database schema v11 (columns only; existing databases migrate in place).

## [0.1.1] - 2026-10-15

### Added

- First-run setup: `otag init` (and plain `otag` on a machine that is not set up yet) runs a wizard that
  detects Claude Code, Codex, OpenCode, tmux and git, proposes accounts and profiles with fallbacks,
  registers the current git repository, offers the daemon and the agent skill, shows the files it will
  write and writes commented TOML atomically, then syncs. `--yes` (non-interactive), `--no-daemon`,
  `--force` (backs up the old registry). On a configured machine it only adds what is missing.
- `otag project add [path]`, `otag project list` and `otag project remove <name>` (retires the project;
  its tasks, history and worktrees are kept).
- `otag doctor` checks tmux and git and gives every warning and failure a `fix:` (in `--json`, `fix`).
- TUI: an empty tree says how to set otag up or add a project; `a` adds the repository the TUI was
  started in.
- [docs/SETUP.md](docs/SETUP.md).

### Changed

- Plain `otag` in a terminal opens the setup wizard (not configured) or the TUI; without a terminal it
  prints the usage and a hint. `otag help`, `-h` and `--help` print the usage on stdout.
- The Homebrew caveats and the install script point to `otag` as the first step.

### Fixed

- The Homebrew formula no longer passes `-s -w` twice to the linker.

## [0.1.0] - 2026-10-15

### Added

- Tasks with a coordinator agent and child agents, each child running in its own git worktree and tmux pane.
- Adapters for Claude Code, Codex and OpenCode.
- Guard levels `light` and `strict` to restrict what child agents may do.
- Agent messaging (ask, answer, park and resume) with proven delivery.
- Quota tracking per account and profile, with fallback to another profile.
- Review flow with an independent reviewer run before a task is accepted.
- Delivery with `otag task deliver`, which marks a task delivered and cleans up its worktree.
- `otag prime` and a global skill that teach agents how to use otag.
- Mouse-driven terminal UI (`otag tui`).
- launchd daemon (`otag daemon`), `otag doctor`, SQLite-backed state.
- `otag version` and `otag --version`.
- Install script, GoReleaser release builds (macOS arm64 and amd64) and a Homebrew formula that builds from source.

[Unreleased]: https://github.com/nesiler/otag/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/nesiler/otag/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/nesiler/otag/releases/tag/v0.1.0
