#!/usr/bin/env bash
# ci-apt-install.sh — install apt packages on a CI runner without letting a
# source we do not install from fail the step (#3774).
#
# WHAT THIS REPLACES. Six workflow steps used to read
#
#     sudo apt-get update && sudo apt-get install -y --no-install-recommends zsh
#
# `apt-get update` exits 100 when ANY configured source fails, whether or not
# the packages you asked for come from it. The ubuntu-24.04 runner image ships
# packages.microsoft.com in /etc/apt/sources.list.d and nothing here installs
# from it; on 2026-09-03 it answered 403, the `&&` short-circuited, and master's
# Build went red on a step whose only job was to add zsh from the Ubuntu
# archive. The same one-liner gated the required Test check on every PR and both
# release preflights, so a third-party outage could have blocked a release.
#
# THE PROPERTY: a source we do not install from cannot fail this step.
#
# HOW: the update is advisory — its failures are printed, annotated, and
# survived — and the INSTALL is the assertion. If a package is genuinely
# unavailable (a real archive outage, a dropped package, a typo'd name),
# apt-get install exits non-zero and so does this script.
#
# This is deliberately NOT `|| true` on the compound command. That would swallow
# the real "zsh is unavailable" too, and a check that reports clean when it did
# not run is a failure mode this repo has already paid for.
#
# NOT for the Dockerfiles under scripts/container/. Those build from a pinned
# base image whose only apt sources are the distro's own, so there is no
# unrelated source to tolerate, and a failed update there should fail the build.
#
# WEDGES (#6457). apt can also stop answering altogether — a dead mirror
# connection, a stuck dpkg lock, a wedged runner — and then neither command
# returns. On 2026-11-01 one arm64 Build sat in `Install zsh` for ~2h, held
# master's head "unverified", and stalled the merger for every open PR, because
# the only bound was the 6h job default. So each apt-get call runs under
# `timeout` (APT_ATTEMPT_TIMEOUT seconds, default 180; a healthy call takes
# seconds) and a call that times out is retried (APT_ATTEMPTS, default 3). A
# wedge therefore fails in minutes, with a message that says "wedged" rather
# than a package error. Only a TIMEOUT is retried: an ordinary non-zero exit is
# still the answer it always was (a failed update tolerated, a failed install
# fatal), so the contract above is unchanged. Every workflow step that calls
# this script also sets timeout-minutes above the worst case here (2 calls ×
# 3 attempts × 180s ≈ 19 min; the steps use 25), as
# a backstop for a hang this script cannot see (e.g. sudo itself).
#
# Usage: scripts/ci-apt-install.sh <package>...
#
# Package names are ordinary argv entries and are never re-parsed by a shell.
# Call sites must pass them as literal words in the workflow's `run:` — never
# interpolate `${{ }}` into the command line, which is script injection rather
# than a variable reference.

set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "::error::ci-apt-install.sh: no packages given" >&2
	echo "usage: scripts/ci-apt-install.sh <package>..." >&2
	exit 2
fi

attempt_timeout="${APT_ATTEMPT_TIMEOUT:-180}"
attempts="${APT_ATTEMPTS:-3}"
case "$attempt_timeout$attempts" in
'' | *[!0-9]*)
	echo "::error::ci-apt-install.sh: APT_ATTEMPT_TIMEOUT and APT_ATTEMPTS must be whole numbers" >&2
	exit 2
	;;
esac
if [ "$attempts" -lt 1 ] || [ "$attempt_timeout" -lt 1 ]; then
	echo "::error::ci-apt-install.sh: APT_ATTEMPT_TIMEOUT and APT_ATTEMPTS must be at least 1" >&2
	exit 2
fi

# GitHub runners are a non-root user with passwordless sudo; containers are
# usually root with no sudo installed. Resolve it once here so no call site has
# to care which it is on.
#
# `timeout` runs INSIDE sudo so it is root and can signal apt-get; killing a
# sudo wrapper from outside would leave the apt-get child running. -k escalates
# to SIGKILL when apt ignores TERM, which a wedged apt often does.
apt=(timeout -k 10 "$attempt_timeout" apt-get)
if [ "$(id -u)" -ne 0 ]; then
	if ! command -v sudo >/dev/null 2>&1; then
		echo "::error::ci-apt-install.sh: not root and sudo is not installed" >&2
		exit 1
	fi
	apt=(sudo "${apt[@]}")
fi

# timeout exits 124 when it timed out, 137 when it had to SIGKILL.
timed_out() { [ "$1" -eq 124 ] || [ "$1" -eq 137 ]; }

# run_apt <log> <apt-get args...>: run one apt-get subcommand, retrying ONLY
# when an attempt timed out. Sets run_status to the final attempt's status and
# run_wedged to 1 if every attempt timed out. Output is teed to <log> so the
# caller can read it back; pipefail makes the pipeline carry apt-get's status.
run_apt() {
	local log="$1" n=1
	shift
	run_wedged=0
	: >"$log"
	while :; do
		echo "+ ${apt[*]} $* (attempt ${n}/${attempts})"
		run_status=0
		"${apt[@]}" "$@" 2>&1 | tee -a "$log" || run_status=$?
		if ! timed_out "$run_status"; then
			return 0
		fi
		echo "::warning::ci-apt-install.sh: 'apt-get $1' made no progress to completion within ${attempt_timeout}s (attempt ${n}/${attempts}) — apt is wedged, not failing (#6457)"
		if [ "$n" -ge "$attempts" ]; then
			run_wedged=1
			return 0
		fi
		n=$((n + 1))
		sleep "${APT_RETRY_DELAY:-5}"
	done
}

update_log="$(mktemp)"
install_log="$(mktemp)"
trap 'rm -f "$update_log" "$install_log"' EXIT

# tee, not capture-then-print: a slow update stays visible live in the job log,
# and the copy on disk is what the failure report below reads back.
run_apt "$update_log" update
update_status="$run_status"

if [ "$update_status" -ne 0 ]; then
	if [ "$run_wedged" -eq 1 ]; then
		echo "::warning::apt-get update wedged on all ${attempts} attempts — continuing to the install, which will fail fast if apt really is hung (#6457)"
	else
		# Print the failing sources rather than only the exit code. A single 403
		# from a source we never use looks identical to a total archive outage in
		# the exit status; the difference is in these lines, and the install below
		# is what decides which one it was.
		echo "::warning::apt-get update exited ${update_status} — continuing, because the install below is the real assertion (#3774)"
		failures="$(grep -E '^[[:space:]]*[EW]:' "$update_log" || true)"
		if [ -n "$failures" ]; then
			echo "apt-get update reported:"
			printf '%s\n' "$failures"
		else
			echo "apt-get update reported no E:/W: lines; see its full output above."
		fi
		echo "Continuing to the install. If the packages below install, the failing"
		echo "source was one we do not need. If they do not, this step fails — and"
		echo "that is a real archive problem, not this one."
	fi
fi

run_apt "$install_log" install -y --no-install-recommends "$@"
install_status="$run_status"
if [ "$run_wedged" -eq 1 ]; then
	echo "::error::ci-apt-install.sh: apt-get install WEDGED — no completion within ${attempt_timeout}s on each of ${attempts} attempts for: $*. This is a hung apt/dpkg/mirror on the runner, not a missing package; re-run the job (#6457)." >&2
	exit "$install_status"
fi
if [ "$install_status" -ne 0 ]; then
	echo "::error::ci-apt-install.sh: apt-get install failed (exit ${install_status}) for: $*" >&2
	if [ "$update_status" -ne 0 ]; then
		echo "::error::apt-get update had also failed (exit ${update_status}); its errors are above and may be the cause." >&2
	fi
	exit "$install_status"
fi
