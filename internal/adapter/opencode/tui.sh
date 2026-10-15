#!/bin/sh
# otag: runs the OpenCode TUI (a client of the run's server) and reports its exit to otag
# as session_end: OpenCode itself sends no event when the TUI quits.
# usage: tui.sh <command> [args...]
"$@"
code=$?
printf '{"code":%d}' "$code" | "${OTAG_BIN:-otag}" event opencode otag.tui.exit >/dev/null 2>&1
exit "$code"
