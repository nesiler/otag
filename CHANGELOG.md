# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/nesiler/otag/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/nesiler/otag/releases/tag/v0.1.0
