#!/bin/sh
# release-bump-and-tag.sh — land the stable-release version bump on master and
# tag the released commit. Called from stable-release.yml's "Tag and publish"
# job, after all four platform builds have succeeded (#1041).
#
# Usage: release-bump-and-tag.sh <version>    # bare semver, no leading v
#
# Runs on the ubuntu-latest release runner. Kept POSIX-portable (no `sed -i`,
# no `grep -oP`) so scripts/release_scripts_test.go can exercise it hermetically
# on both the Linux and macOS CI runners.
set -eu

NEW_VERSION="${1:?usage: release-bump-and-tag.sh <version>}"

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
# No-op when main.go already carries the version (e.g. re-running after a
# failure that landed the bump commit but not the tag).
if ! git diff --cached --quiet; then
	git commit -m "chore: release v${NEW_VERSION}"
fi

# Push the bump to master, tolerating a master that moved during the ~4-minute
# build (#1927). The auto-gate squash-merges CI-green PRs continuously, so a
# plain push races that traffic and dies non-fast-forward, throwing away four
# good builds. On a rejection, rebase the one-line bump onto the new tip and
# retry, bounded. A rebase drops the bump as already-applied when a prior run
# already landed it (patch-id), so the re-run converges to a clean no-op push.
# A genuine conflict — only a rival version bump would touch this line — fails
# the rebase loudly here instead of force-pushing over another release.
MAX_ATTEMPTS="${RELEASE_PUSH_MAX_ATTEMPTS:-5}"
attempt=1
until git push origin HEAD:master; do
	if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
		echo "::error::Version bump push rejected after ${MAX_ATTEMPTS} attempts (master kept moving)"
		exit 1
	fi
	echo "Push rejected — master advanced during the build; rebasing onto origin/master and retrying (attempt ${attempt}/${MAX_ATTEMPTS})…"
	git fetch origin master
	git rebase FETCH_HEAD
	attempt=$((attempt + 1))
done

# Tag the commit that actually landed on master. A rebase above may have
# rewritten HEAD, so the tag is created only after the push succeeds — and
# pushed after master, never before, so a failure at worst leaves a bump commit
# with no tag (a clean, re-runnable state) and never a tag with no release.
git tag "v${NEW_VERSION}"
git push origin "v${NEW_VERSION}"
