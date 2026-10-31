#!/usr/bin/env bash
# Which paths a pull request may change in relation to web/dist (#6447).
#
#   web-dist-pr-rules.sh <base> <sync|normal>
#
# <base> is the commit the PR's own change is measured from (HEAD^1 of a
# pull_request merge commit, or the merge base with master for a branch run);
# HEAD is the PR. A normal PR must not touch web/dist; the web-dist-sync PR may
# touch NOTHING ELSE.
#
# The decision is `git diff --quiet` with a pathspec, never a grep of
# `--name-only` text: git quotes non-ASCII names ("web/dist/\303\251.html"), and
# with rename detection a move prints only its destination, so both slipped past a
# text match. Planted files under web/dist are served from the daemon's origin
# (daemon/webserve.go), so this has to hold for every name. --no-renames keeps a
# move out of web/dist counted as a deletion in it. The listing below is
# display-only and never decides anything.
set -euo pipefail

base="${1:?usage: web-dist-pr-rules.sh <base> <sync|normal>}"
mode="${2:?usage: web-dist-pr-rules.sh <base> <sync|normal>}"

list() { git -c core.quotePath=false diff --no-renames --name-only "$base" HEAD -- "$@" | head -20; }

case "$mode" in
sync)
	if ! git diff --quiet --no-renames "$base" HEAD -- . ':(exclude)web/dist'; then
		echo "::error::the web-dist-sync PR may change only web/dist/, but it also changes:"
		list . ':(exclude)web/dist'
		exit 1
	fi
	;;
normal)
	if ! git diff --quiet --no-renames "$base" HEAD -- web/dist; then
		echo "::error::this PR changes web/dist, which is generated and committed only by web-dist-sync.yml (#6447)."
		list web/dist
		echo
		echo "Committed bundles are what made every open web PR conflict whenever another web PR merged."
		echo "Drop it from your branch (a web PR needs only its source change):"
		echo
		echo "    git checkout origin/master -- web/dist && git commit -m 'Drop generated web/dist'"
		exit 1
	fi
	;;
*)
	echo "unknown mode: $mode" >&2
	exit 2
	;;
esac
