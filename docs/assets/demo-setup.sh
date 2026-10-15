#!/bin/sh
# Builds an isolated otag environment with fake agents for the README demo (docs/assets/tui.tape).
#   docs/assets/demo-setup.sh up <otag-binary>   build the state under $OTAG_DEMO_ROOT (default /tmp/otag-demo)
#   docs/assets/demo-setup.sh down               stop the private tmux server and remove the temp dir
# Everything lives under one temp dir and a private tmux socket; nothing real is touched.
# The demo shell sources "$OTAG_DEMO_ROOT/env.sh" to get the isolated environment.
set -eu
ROOT=${OTAG_DEMO_ROOT:-/tmp/otag-demo}
SOCK=otag-demo-sock
BIN_DIR=$ROOT/bin

down() {
	tmux -L "$SOCK" kill-server 2>/dev/null || true
	rm -rf "$ROOT"
}

id_of() { sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' | head -1; }

up() {
	down
	mkdir -p "$ROOT/cfg/projects" "$ROOT/data" "$BIN_DIR" "$ROOT/home" "$ROOT/acme-api"
	cp "$1" "$BIN_DIR/otag"
	cat >"$ROOT/env.sh" <<EOF
export USER=demo HOME=$ROOT/home OTAG_CONFIG_DIR=$ROOT/cfg OTAG_DATA_DIR=$ROOT/data OTAG_DB=$ROOT/data/otag.db
export OTAG_TMUX_SOCKET=$SOCK OTAG_BIN=$BIN_DIR/otag PATH=$BIN_DIR:\$PATH
export GIT_AUTHOR_NAME=Demo GIT_AUTHOR_EMAIL=demo@example.com GIT_COMMITTER_NAME=Demo GIT_COMMITTER_EMAIL=demo@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
EOF
	. "$ROOT/env.sh"

	cd "$ROOT/acme-api"
	git init -q -b main
	mkdir -p cmd/server internal/auth internal/billing
	printf 'module acme/api\n\ngo 1.23\n' >go.mod
	printf 'package main\n\nfunc main() {}\n' >cmd/server/main.go
	printf 'package auth\n\n// Login checks credentials.\nfunc Login() {}\n' >internal/auth/login.go
	printf 'package billing\n\n// Invoice renders an invoice.\nfunc Invoice() {}\n' >internal/billing/invoice.go
	printf '# acme-api\n' >README.md
	git add -A
	git commit -q -m "initial commit"

	cat >"$ROOT/cfg/registry.toml" <<'TOML'
[accounts.fake]
harness = "fake"

[profiles.coord]
account = "fake"
description = "Coordinator"
args = ["start", "busy", "run:echo 'Plan: split the billing work into 4 tasks'; echo 'Waiting for children to report...'", "idle", "wait"]

[profiles.worker]
account = "fake"
description = "Implements a task"
args = ["start", "busy", "run:i=0; while :; do i=$((i+1)); case $((i%4)) in 0) echo \"> Edit internal/billing/invoice.go (+$((i*3)) -2)\";; 1) echo \"> Bash: go test ./internal/billing/... ok\";; 2) echo \"  Reading internal/auth/login.go\";; 3) echo \"  Thinking about the retry policy, step $i\";; esac; sleep 1; done"]

[profiles.asker]
account = "fake"
description = "Asks a question"
args = ["start", "busy", "run:echo 'Reading the token middleware...'; echo 'Two session stores are possible.'", "ask:Redis or in-memory session store for the rate limiter?", "wait"]

[profiles.finisher]
account = "fake"
description = "Finishes quickly"
args = ["start", "busy", "run:echo 'Implemented, tests pass.'", "commit", "done:Added pagination to /v1/invoices", "wait"]

[profiles.reviewer]
account = "fake"
description = "Reviews"
args = ["start", "busy", "run:echo 'Reviewing the branch...'; sleep 2; echo 'Looks reasonable, checking edge cases.'", "wait"]
TOML
	cat >"$ROOT/cfg/projects/acme-api.toml" <<TOML
path = "$ROOT/acme-api"
prefix = "api"
coordinator_profile = "coord"
default_profile = "worker"
review_profile = "reviewer"
workspace = "worktree"
guard = "light"
TOML
	otag sync >/dev/null
	# No coordinator: otag starts coordinators on claude profiles only, never on the fake harness.
	t1=$(otag --json task add --profile worker --priority 2 --body "Exponential backoff for billing webhooks" "Retry failed webhook deliveries" | id_of)
	t2=$(otag --json task add --profile asker "Add rate limiting to the public API" | id_of)
	t3=$(otag --json task add --profile finisher "Paginate /v1/invoices" | id_of)
	t4=$(otag --json task add --profile finisher "Fix login redirect loop" | id_of)
	otag task add --profile worker --draft "Document the auth flow" >/dev/null
	otag task add --profile worker "Rotate API keys on a schedule" >/dev/null
	for t in $t1 $t2 $t3 $t4; do otag run start --no-attach "$t" >/dev/null; done
	# Wait for the two finishers to reach review, accept one, start a reviewer on the other.
	n=0
	while [ $n -lt 30 ] && [ "$(otag task list | grep -c ' review ')" -lt 2 ]; do sleep 1; n=$((n + 1)); done
	otag review "$t4" --accept "LGTM, one nit fixed" >/dev/null
	rv=$(otag task list | sed -n 's/^\([a-z0-9-]*\) .*Review: Paginate.*/\1/p' | head -1)
	otag run start --no-attach "$rv" >/dev/null
	echo "$t1 $t2 $t3 $t4" >"$ROOT/ids"
}

case ${1:-} in
up) up "$2" ;;
down) down ;;
*) echo "usage: $0 up <otag-binary> | down" >&2; exit 2 ;;
esac
