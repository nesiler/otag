# Contributing to otag

Thanks for your interest. otag is a macOS-only Go tool; it needs `tmux` and `git` at runtime.

## Development setup

```sh
brew install go tmux git
git clone https://github.com/nesiler/otag && cd otag
go build ./cmd/otag
go test ./...
```

Before opening a PR, run:

```sh
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...
golangci-lint run   # optional locally, run in CI
```

## Test isolation rules

Tests must never touch real user state or a running otag setup.

- Use `t.TempDir()` for every file, DB, config and data directory. Never read or write `~/.config/otag`, `~/.local/share/otag`, `~/.claude*`, `~/.codex` or similar.
- Use a private tmux socket (for example `otag-test-<random>`) and kill that server in cleanup. Never use the default tmux socket or the `otag` socket.
- Do not install launchd jobs or global skills from tests.
- Tests must not need network access or real agent credentials; use fakes for harnesses.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`, `ci:`. The release changelog is grouped by these types.
- Fork, create a topic branch, keep PRs focused, and fill in the PR template.
- Update docs and `CHANGELOG.md` when behavior changes.
- CI must be green before review.

## Reporting bugs and security issues

Use the issue templates for bugs and features. Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).
