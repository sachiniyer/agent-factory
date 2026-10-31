#!/usr/bin/env bash
# `go test -exec` wrapper: run one test binary and report (never fail) when it has
# made no progress for STALL_SECONDS. Usage (CI only):
#
#   AF_STALL_DIR=$RUNNER_TEMP/stall go test -exec scripts/ci-test-exec.sh ./...
#
# Progress is per test binary, not per `go test` invocation (#5766). `go test
# ./...` holds back the output of every parallel package but one, so silence on
# its stdout says nothing about any one binary (#6040 fired on that and was
# reverted in #6053). Here each binary's own output is watched directly: every
# `=== RUN`, `--- PASS` or log line is progress, so the longest legitimate gap is
# the longest single test. That was 124s across the six macOS runs measured on
# 2026-10-29, against STALL_SECONDS=300 and a hang that went >9 minutes silent.
#
# On a stall it writes a report file and, once, asks the binary for a goroutine
# dump with SIGUSR2, written to AF_STALL_DIR/goroutines-<pid>.dump as well as
# stderr: go test buffers a package's output, so stderr is lost when the step is
# killed from outside, which is the #5766 case (only if the binary carries the handler, see
# internal/testguard/stalldump_unix.go). Nothing is killed and the exit status is
# the binary's own.
set -u -o pipefail
stall=${STALL_SECONDS:-300}
dir=${AF_STALL_DIR:-}
bin=$1

last=$(mktemp)
touch "$last"
export AF_STALL_DUMP=1

stamp_age() {
	local now mtime
	now=$(date +%s)
	mtime=$(stat -c %Y "$last" 2>/dev/null || stat -f %m "$last" 2>/dev/null || echo "$now")
	echo $((now - mtime))
}

# Each line the binary prints (stdout or stderr) refreshes the timestamp.
"$@" 2>&1 | while IFS= read -r line || [ -n "$line" ]; do
	printf '%s\n' "$line"
	# A write, not a touch: a builtin, so no fork per line, and a real write
	# moves the mtime on every platform (a truncate of an empty file may not).
	echo . >"$last"
done &
runner=$!

(
	fired=0
	while kill -0 "$runner" 2>/dev/null; do
		# The sleep must not inherit this wrapper's stdout/stderr: go test waits
		# for EOF on that pipe, so a sleep outliving the binary would hold it
		# open for up to 10s per binary.
		sleep 10 >/dev/null 2>&1 </dev/null
		# The binary finished and the wrapper removed the stamp while we slept.
		[ -e "$last" ] || break
		[ "$fired" = 1 ] && continue
		age=$(stamp_age)
		[ "$age" -lt "$stall" ] && continue
		fired=1
		# The binary itself, by exact argv[0]: matching on the path anywhere in
		# the command line would also find this wrapper, which carries it as an
		# argument, and SIGUSR2 kills a process with no handler.
		pid=$(ps -axo pid=,command= | awk -v b="$bin" '$2 == b { print $1; exit }')
		report="no output from $(basename "$bin") (pid ${pid:-?}) for ${age}s; report-only, nothing was killed"
		echo "$report" >&2
		if [ -n "$dir" ]; then
			mkdir -p "$dir"
			{
				echo "$report"
				ps -axo pid,ppid,etime,time,command | head -80
			} >"$dir/$(basename "$bin")-$$.txt" 2>&1
		fi
		if [ -n "$pid" ] && grep -qa 'af-stall-dump-v1' "$bin"; then
			kill -USR2 "$pid" 2>/dev/null || true
		fi
	done
) &
dog=$!

wait "$runner"
status=$?
# Reap the watchdog silently: bash reports a killed job as "Terminated" followed
# by the subshell's whole source text, and a grep for stall reports in a hang's
# log would then match that text.
# Not pkill -P: killing the watchdog's sleep makes the watchdog print
# "Terminated" itself. The orphaned sleep holds no output fd (redirected above)
# and ends within 10s.
{ kill "$dog"; wait "$dog"; } 2>/dev/null
rm -f "$last"
exit "$status"
