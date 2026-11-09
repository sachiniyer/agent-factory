#!/bin/sh
# release-bump.sh — make the stable-release version-bump commit and land it on
# master. Called from stable-release.yml's "Bump and publish" job, after all
# four platform builds have succeeded (#1041), on a checkout of github.sha: the
# commit every artifact was built from.
#
# The tag is deliberately NOT created here: `gh release create --target` makes
# it atomically with the published release (#5159), so a run that dies after
# this script at worst leaves the bump on master — a clean, re-runnable
# state — and never a tag with no release.
#
# What gets tagged is the bump commit parented on the BUILT commit, whatever
# master became during the build (#7018). Its tree is the built tree plus the
# one-line main.go edit, so the tag's source, the release's generated notes
# (previous release..tag) and the binaries all hold the same code. A PR that
# merged during the build is not in the tag's history: it ships in the next
# release, and that release's notes list it. On exit HEAD is that commit; the
# workflow passes `git rev-parse HEAD` to `gh release create --target`.
#
# Usage: release-bump.sh <version>    # bare semver, no leading v
#
# Runs on the ubuntu-24.04 release runner. Kept POSIX-portable (no `sed -i`,
# no `grep -oP`) so scripts/release_scripts_test.go can exercise it hermetically
# on both the Linux and macOS CI runners.
set -eu

NEW_VERSION="${1:?usage: release-bump.sh <version>}"
BUILT=$(git rev-parse HEAD)

# Landing the bump brings the tagged commit's history into master, so that
# history must already be master's: a dispatch from another branch would
# otherwise push that branch's unreviewed commits to master. The checkout's own
# view of master is enough — master only moves forward, so a commit on master
# when the run was dispatched is still on it.
if ! git merge-base --is-ancestor "$BUILT" refs/remotes/origin/master 2>/dev/null; then
	echo "::error::The release was built from ${BUILT}, which is not on master (or the checkout has no origin/master to check against) — refusing to land it"
	exit 1
fi

# Extract the current value of main.go's `version` fallback string. POSIX BRE,
# tolerant of the gofmt alignment whitespace around the `=`.
extract_version() {
	sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' main.go | head -n1
}

CURRENT=$(extract_version)
echo "Current main.go version: ${CURRENT:-<none>}, releasing: $NEW_VERSION"

# Rewrite the version string in place (portable: no GNU `sed -i`).
tmp=$(mktemp)
sed 's/^\([[:space:]]*version[[:space:]]*=[[:space:]]*"\)[^"]*\(".*\)$/\1'"$NEW_VERSION"'\2/' main.go > "$tmp"
mv "$tmp" main.go

# Fail loudly if the substitution missed (e.g. formatting drift in main.go)
# instead of tagging a release with a stale embedded version.
UPDATED=$(extract_version)
if [ "$UPDATED" != "$NEW_VERSION" ]; then
	echo "::error::Failed to update version in main.go (found '${UPDATED:-<none>}')"
	exit 1
fi

git config user.name "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"
git add main.go
# No commit when main.go already carries the version: a fresh dispatch after a
# run that landed the bump but died before publishing builds a commit that holds
# the bump already, and that built commit is itself what gets tagged.
if ! git diff --cached --quiet; then
	# Dated from the built commit, not the clock. "Re-run failed jobs" keeps
	# github.sha, so with the identity, message, parent and tree already fixed
	# the date is the only input that would differ between runs. Pinning it
	# makes a re-run reproduce the very commit an earlier run landed, rather
	# than a twin master would have to take in a second time.
	date=$(git show -s --format=%cd --date=raw HEAD)
	GIT_AUTHOR_DATE="$date" GIT_COMMITTER_DATE="$date" \
		git commit -m "chore: release v${NEW_VERSION}"
fi
TAG=$(git rev-parse HEAD)

# Push the bump to master. When master has not moved since the build, the
# release commit is a fast-forward of it and lands as-is: history stays linear.
#
# master moving during the ~4-minute build (#1927) is routine — the merger
# squash-merges CI-green PRs continuously — and the push is then rejected
# '(fetch first)' when the new tip is a commit this checkout has not fetched,
# '(non-fast-forward)' when it has the tip and it is not an ancestor. Both mean
# master advanced: merge the release commit into the new tip and retry,
# bounded. A merge, not a rebase: a rebased copy would carry the mid-build PRs
# into the tagged tree (#7018), and leaving the release commit off master
# instead would hide the new stable from everything that looks for it on
# master — latest-released-tag.sh, which previews use as their baseline, and
# --generate-notes, which starts the next release's notes at the nearest
# earlier release in its history. Because the built commit is already on
# master, the merge adds only the main.go edit. master's ruleset requires a
# linear history; the release deploy key bypasses that ruleset (as it does its
# required status checks), and only this path makes a merge commit.
#
# Any other failed push (auth, network, a protected-branch refusal) prints
# git's own error and fails on its own terms: naming it "master advanced" would
# point a reader at the #1927 race instead of the real cause (#6747, #6744),
# and merging cannot fix it. A genuine conflict — only a rival version bump
# touches that line — fails the merge loudly instead of force-pushing over
# another release.
MAX_ATTEMPTS="${RELEASE_PUSH_MAX_ATTEMPTS:-5}"
attempt=1
while :; do
	if push_out=$(git push origin HEAD:master 2>&1); then
		printf '%s\n' "$push_out"
		break
	fi
	printf '%s\n' "$push_out"
	case "$push_out" in
	*non-fast-forward*|*"fetch first"*) ;;
	*)
		echo "::error::Version bump push failed and not because master moved — nothing a merge can fix (git output above)"
		exit 1
		;;
	esac
	if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
		echo "::error::Version bump push rejected after ${MAX_ATTEMPTS} attempts (master kept moving)"
		exit 1
	fi
	echo "Push rejected — master advanced during the build; merging the release commit into origin/master and retrying (attempt ${attempt}/${MAX_ATTEMPTS})…"
	git fetch origin master
	# A re-run of a build whose bump an earlier run already landed: the release
	# commit is reproduced exactly (see the commit date above), so it is
	# already in master's history and there is nothing left to push.
	if git merge-base --is-ancestor "$TAG" FETCH_HEAD; then
		echo "master already has the release commit ${TAG}; nothing to push"
		break
	fi
	git checkout -q --detach FETCH_HEAD
	if ! git merge --no-edit -m "chore: merge release v${NEW_VERSION} into master" "$TAG"; then
		git merge --abort || :
		echo "::error::Merging the release commit into master conflicted — a rival commit changed main.go's version line; not forcing over it"
		exit 1
	fi
	attempt=$((attempt + 1))
done

# Leave HEAD on the commit the release tags, not on the merge that carried it.
git checkout -q --detach "$TAG"
