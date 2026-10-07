#!/bin/sh
# latest-released-tag.sh — the "latest" v* tag for release purposes is the one
# nearest HEAD that has a PUBLISHED GitHub release. A tag stranded on origin
# without its release (#5159) must not become the baseline: it never shipped
# binaries, and counting commits since it would either suppress the retry that
# should re-release ("no new commits") or skip a preview number.
#
# Usage: latest-released-tag.sh <released-tags-file>
#   <released-tags-file>: the repo's published release tag names, one per line
#   (drafts excluded by the caller). Prints the tag name, or nothing when no
#   released tag reaches HEAD.
set -eu

released="${1:?usage: latest-released-tag.sh <released-tags-file>}"

# `git describe` has no "only these tags" mode, but it does take --exclude:
# feed it every reachable v* tag WITHOUT a published release and let it pick
# the nearest survivor, preserving its distance semantics instead of
# re-implementing them.
orphans=$(mktemp)
trap 'rm -f "$orphans"' EXIT
# grep -Fxv keeps merged v* tags not in the released file. Exit 1 (all
# candidates released) is fine; anything >=2 is a real grep failure.
git tag -l 'v*' --merged HEAD | grep -Fxv -f "$released" >"$orphans" || {
	status=$?
	[ "$status" -eq 1 ] || exit "$status"
}

set --
while IFS= read -r orphan; do
	[ -n "$orphan" ] || continue
	set -- "$@" --exclude "$orphan"
done <"$orphans"

git describe --tags --abbrev=0 --match 'v*' "$@" 2>/dev/null || true
