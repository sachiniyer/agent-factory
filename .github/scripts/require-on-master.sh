#!/bin/sh
# require-on-master.sh — fail unless a commit is on master, as this checkout's
# refs/remotes/origin/master knows it. stable-release.yml runs it twice on
# github.sha: in the validate job, so a dispatch from a branch master does not
# contain fails before the 30-minute preflight and four builds, and again from
# release-bump.sh in the publish job, right before anything is pushed.
#
# Landing a release merges the tagged commit's history into master (#7018), so
# that history must already be master's, or a dispatch from another branch
# would push that branch's unreviewed commits to master. master only moves
# forward, so a commit that was on master when the run was dispatched is still
# on it at checkout.
#
# Usage: require-on-master.sh [<commit>]     # default HEAD
#
# Needs a full-history checkout (actions/checkout fetch-depth: 0). Each way the
# checkout can fail to answer the question fails closed with its own message,
# never as "not on master": no origin/master at all, or shallow history, whose
# ancestry walk stops at a grafted-away parent and so answers "not an ancestor"
# for commits that are.
set -eu

commit=$(git rev-parse --verify "${1:-HEAD}^{commit}")

if ! git rev-parse -q --verify refs/remotes/origin/master >/dev/null; then
	echo "::error::This checkout has no refs/remotes/origin/master to check ${commit} against — it needs a full-history checkout (fetch-depth: 0); refusing to release"
	exit 1
fi
if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
	echo "::error::This checkout is shallow, so its history cannot show whether ${commit} is on master — it needs a full-history checkout (fetch-depth: 0); refusing to release"
	exit 1
fi
if ! git merge-base --is-ancestor "$commit" refs/remotes/origin/master; then
	echo "::error::The release is built from ${commit}, which is not on master — dispatch Stable Release from master; refusing to release"
	exit 1
fi
echo "${commit} is on master"
