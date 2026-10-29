#!/usr/bin/env bash
# Run a command; if it prints nothing for STALL_SECONDS, SIGQUIT every Go test
# binary under it so each one dumps all of its goroutines, then let the command finish.
#
#   scripts/ci-stall-watchdog.sh <stall-seconds> -- go test -v ... ./...
#
# Why (#5766): Test (macOS) hung at one test with ~10 minutes of silence and the
# step was then ended from outside. Go's own -timeout (30m) never got to fire, so
# the log named the test that was running and nothing about what it was waiting
# on. A SIGQUIT'd Go binary prints every goroutine's stack and exits, which turns
# the next sighting into evidence. Only descendants of the command are signalled.
set -u
stall=${1:?usage: ci-stall-watchdog.sh <stall-seconds> -- command...}
shift
[ "${1:-}" = "--" ] && shift

descendants() {
	local child
	for child in $(pgrep -P "$1" 2>/dev/null); do
		echo "$child"
		descendants "$child"
	done
}

log=$(mktemp)
"$@" > >(tee "$log") 2>&1 &
cmd=$!

(
	last=0
	idle=0
	while kill -0 "$cmd" 2>/dev/null; do
		sleep 10
		size=$(wc -c <"$log" | tr -d ' ')
		if [ "$size" != "$last" ]; then
			last=$size
			idle=0
			continue
		fi
		idle=$((idle + 10))
		if [ "$idle" -ge "$stall" ]; then
			echo "::error::no output for ${idle}s; sending SIGQUIT to every Go test binary so it dumps its goroutines (#5766)"
			for pid in $(descendants "$cmd"); do
				if ps -o command= -p "$pid" | grep -q -- '-test\.timeout'; then
					echo "SIGQUIT $pid: $(ps -o command= -p "$pid" | cut -c1-200)"
					kill -QUIT "$pid" 2>/dev/null || true
				fi
			done
			break
		fi
	done
) &
dog=$!

wait "$cmd"
status=$?
kill "$dog" 2>/dev/null || true
rm -f "$log"
exit "$status"
