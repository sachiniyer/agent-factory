#!/bin/sh
# release-bump.sh — land the stable-release version bump on master. Called
# from stable-release.yml's "Bump and publish" job, after all four platform
# builds have succeeded (#1041).
#
# The tag is deliberately NOT created here: `gh release create --target` makes
# it atomically with the published release (#5159), so a run that dies after
# this script at worst leaves a bump commit on master — a clean, re-runnable
# state — and never a tag with no release.
#
# Usage: release-bump.sh <version>    # bare semver, no leading v
#
# Runs on the ubuntu-24.04 release runner. Kept POSIX-portable (no `sed -i`,
# no `grep -oP`) so scripts/release_scripts_test.go can exercise it hermetically
# on both the Linux and macOS CI runners.
set -eu

NEW_VERSION="${1:?usage: release-bump.sh <version>}"

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
# failure that landed the bump commit but not the release).
if ! git diff --cached --quiet; then
	git commit -m "chore: release v${NEW_VERSION}"
fi

# Push the bump to master, tolerating a master that moved during the ~4-minute
# build (#1927). The auto-gate squash-merges CI-green PRs continuously, so a
# plain push races that traffic and is rejected — '(fetch first)' when the new
# tip is a commit this checkout has not fetched, '(non-fast-forward)' when it
# has the tip and it is not an ancestor. Both mean master advanced; rebase the
# one-line bump onto the new tip and retry, bounded. Any other failed push
# (auth, network, a protected-branch refusal) prints git's own error and fails
# on its own terms: naming it "master advanced" would point a reader at the
# #1927 race instead of the real cause (#6747, #6744), and rebasing cannot fix
# it. A rebase drops the bump as already-applied when a prior run already
# landed it (patch-id), so the re-run converges to a clean no-op push. A
# genuine conflict — only a rival version bump would touch this line — fails
# the rebase loudly here instead of force-pushing over another release.
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
		echo "::error::Version bump push failed and not because master moved — nothing a rebase can fix (git output above)"
		exit 1
		;;
	esac
	if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
		echo "::error::Version bump push rejected after ${MAX_ATTEMPTS} attempts (master kept moving)"
		exit 1
	fi
	echo "Push rejected — master advanced during the build; rebasing onto origin/master and retrying (attempt ${attempt}/${MAX_ATTEMPTS})…"
	git fetch origin master
	git rebase FETCH_HEAD
	attempt=$((attempt + 1))
done

# The workflow tags `git rev-parse HEAD` next: after the rebase-retry loop,
# HEAD is exactly the commit that landed on master — the sha the release's
# --target must point at.
