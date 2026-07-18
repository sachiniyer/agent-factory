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
git tag "v${NEW_VERSION}"
git push origin HEAD:master
git push origin "v${NEW_VERSION}"
