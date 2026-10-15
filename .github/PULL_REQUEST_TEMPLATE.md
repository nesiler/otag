## Summary

<!-- What does this change and why? -->

## Checklist

- [ ] Conventional commit title (`feat:`, `fix:`, `docs:`, ...)
- [ ] `gofmt -l .` and `go vet ./...` are clean
- [ ] `go test -race ./...` passes
- [ ] Tests use temp dirs and private tmux sockets (no real user state touched)
- [ ] Docs and CHANGELOG updated when behavior changes
