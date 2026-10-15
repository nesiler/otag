#!/bin/sh
# otag fake harness: runs the steps given as arguments in order and reports them with
# `otag event fake <kind>`, like a real harness's hooks would. Used by tests and demos.
#
# steps: trust start busy tool toolrun tooldone ask perm idle usage error ratelimit quota end commit
#        done:<summary> fail:<reason> ask:<question> askpark:<question> inbox:<sec>
#        sleep:<sec> exit:<code> run:<command> wait server
otag=${OTAG_BIN:-otag}
sid=${FAKE_SESSION_ID:-fake-$$}
n=0

emit() { # kind [json]
	n=$((n + 1))
	p=${2:-}
	[ -n "$p" ] || p='{}'
	printf '%s' "$p" | "$otag" event fake "$1" --event-id "$sid-$n" >/dev/null
}

for step in "$@"; do
	echo "fake: $step"
	case $step in
	trust)
		printf 'Do you trust the files in this folder? (y/n) '
		read -r ans
		[ "$ans" = y ] || exit 9
		;;
	start) emit session_start "{\"session_id\":\"$sid\",\"source\":\"startup\"}" ;;
	busy) emit turn_start ;;
	tool) emit activity '{"tool":"Bash"}' ;;
	toolrun) emit activity '{"tool":"Bash","phase":"start"}' ;;  # a tool call begins...
	tooldone) emit activity '{"tool":"Bash","phase":"end"}' ;;   # ...and ends
	ask) emit question '{"text":"Which color?"}' ;;
	perm) emit permission '{"text":"Allow curl?","tool":"Bash"}' ;;
	idle) emit turn_end '{"text":"turn finished","reason":"answer"}' ;;
	usage) emit usage "{\"model\":\"fake-1\",\"input_tokens\":100,\"output_tokens\":20,\"id\":\"$sid-u$n\"}" ;;
	error) emit error '{"text":"provider exploded"}' ;;
	ratelimit) emit rate_limit '{"text":"429 too many requests","retry":true}' ;; # transient: the harness retries
	quota) # the turn fails on a usage limit
		emit rate_limit '{"text":"usage limit reached"}'
		emit turn_end '{"text":"usage limit reached","reason":"error"}'
		;;
	end) emit session_end '{"reason":"exit"}' ;;
	commit)
		echo "fake change $n" >>fake.txt
		git add fake.txt && git commit -q -m "fake: change $n"
		;;
	done:*) "$otag" done "${step#done:}" ;;
	ask:*) "$otag" ask "${step#ask:}" ;;                  # an open question (otag ask)...
	askpark:*) "$otag" ask --park "${step#askpark:}" ;; # ...that parks the run
	fail:*) "$otag" fail "${step#fail:}" ;;
	inbox:*) # wait up to N s for a delivery (DeliverReceipt), then submit it as a prompt
		f="$(dirname "$0")/fake-inbox.txt"
		i=0
		while [ ! -s "$f" ] && [ $i -lt "${step#inbox:}" ]; do sleep 1; i=$((i + 1)); done
		if [ -s "$f" ]; then
			m=$(grep -o "\[otag#d[0-9]*\]" "$f" | tr "\n" " ")
			mv "$f" "$f.taken"
			emit turn_start "{\"prompt\":\"$m\"}"
			emit turn_end '{"reason":"answer"}'
		fi
		;;
	sleep:*) sleep "${step#sleep:}" ;;
	run:*) sh -c "${step#run:}" ;; # a command in the pane (tests: an input box)
	exit:*) exit "${step#exit:}" ;;
	wait) while :; do sleep 3600; done ;;
	server)
		: >"$FAKE_READY_FILE"
		while :; do sleep 3600; done
		;;
	*) echo "fake: unknown step $step" >&2 ;;
	esac
done
