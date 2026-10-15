#!/bin/sh
# otag: starts the run's OpenCode server. The agent's shell tool runs inside this
# process, so otag's own binary goes first on PATH: `otag done` then reaches the same
# otag (and database) that started the run.
# usage: serve.sh <opencode> serve [args...]
if [ -n "$OTAG_BIN" ]; then
	PATH=$(dirname "$OTAG_BIN"):$PATH
	export PATH
fi
exec "$@"
