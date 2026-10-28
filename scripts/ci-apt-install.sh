#!/usr/bin/env bash
# ci-apt-install.sh — install apt packages on a CI runner without letting a
# source we do not install from fail the step (#3774).
#
# WHAT THIS REPLACES. Six workflow steps used to read
#
#     sudo apt-get update && sudo apt-get install -y --no-install-recommends zsh
#
# `apt-get update` exits 100 when ANY configured source fails, whether or not
# the packages you asked for come from it. The ubuntu-latest runner image ships
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

# GitHub runners are a non-root user with passwordless sudo; containers are
# usually root with no sudo installed. Resolve it once here so no call site has
# to care which it is on.
apt=(apt-get)
if [ "$(id -u)" -ne 0 ]; then
	if ! command -v sudo >/dev/null 2>&1; then
		echo "::error::ci-apt-install.sh: not root and sudo is not installed" >&2
		exit 1
	fi
	apt=(sudo apt-get)
fi

update_log="$(mktemp)"
trap 'rm -f "$update_log"' EXIT

# tee, not capture-then-print: a slow update stays visible live in the job log,
# and the copy on disk is what the failure report below reads back. pipefail
# makes the pipeline carry apt-get's status rather than tee's.
echo "+ ${apt[*]} update"
update_status=0
"${apt[@]}" update 2>&1 | tee "$update_log" || update_status=$?

if [ "$update_status" -ne 0 ]; then
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

echo "+ ${apt[*]} install -y --no-install-recommends $*"
install_status=0
"${apt[@]}" install -y --no-install-recommends "$@" || install_status=$?
if [ "$install_status" -ne 0 ]; then
	echo "::error::ci-apt-install.sh: apt-get install failed (exit ${install_status}) for: $*" >&2
	if [ "$update_status" -ne 0 ]; then
		echo "::error::apt-get update had also failed (exit ${update_status}); its errors are above and may be the cause." >&2
	fi
	exit "$install_status"
fi
