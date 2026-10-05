#!/bin/sh
# release-forget-version.sh — remove every UNPUBLISHED trace of a version:
# a leftover draft release, and an orphaned tag ref with no release object at
# all (the #5159 stranding). A published release is never touched.
#
# Called before `gh release create` so a re-run after a failed or cancelled
# release converges instead of colliding with its own residue, and again on
# job failure so a dead run leaves nothing behind. Cancellation can still
# strand a draft (cleanup steps do not run on cancel), which is exactly what
# the pre-create call is for.
#
# Usage: release-forget-version.sh <version>     # bare semver, no leading v
# Requires GH_TOKEN (contents:write) and GH_REPO (owner/name) in the env.
set -eu

version="${1:?usage: release-forget-version.sh <version>}"
tag="v${version}"
repo="${GH_REPO:?set GH_REPO to owner/name}"

# Release state first: drafts store tag_name but create no git ref (they sit
# as untagged-* until publish), yet still collide with a fresh
# `gh release create`, so they are deleted by id — find-by-tag can miss an
# untagged draft. A published release means the tag legitimately exists and
# the whole version is off-limits.
release=$(gh api "repos/${repo}/releases?per_page=100" --paginate \
	--jq '.[] | select(.tag_name == "'"${tag}"'") | "\(.id) \(.draft)"')
if [ -n "$release" ]; then
	id=${release%% *}
	draft=${release##* }
	if [ "$draft" = "false" ]; then
		echo "${tag} has a published release; nothing to forget"
		exit 0
	fi
	echo "Deleting leftover draft release ${tag} (id ${id})"
	gh api -X DELETE "repos/${repo}/releases/${id}" --silent
fi

# Reaching here means no published release carries ${tag}, so any surviving
# ref is an orphan — pushed by a pre-#5159 run or left by a deleted draft.
# It must go: a tag may exist on origin only with its published release, and a
# stale ref would pin `gh release create --target` to the wrong commit —
# existing tags are reused, never re-pointed.
if ref=$(gh api "repos/${repo}/git/ref/tags/${tag}" 2>&1); then
	echo "Deleting orphaned tag ref ${tag} (no published release carries it)"
	gh api -X DELETE "repos/${repo}/git/refs/tags/${tag}" --silent
else
	case "$ref" in
		*404* | *"Not Found"*) : ;; # no ref — the common case
		*)
			echo "::error::cannot inspect ref tags/${tag}: ${ref}" >&2
			exit 1
			;;
	esac
fi
