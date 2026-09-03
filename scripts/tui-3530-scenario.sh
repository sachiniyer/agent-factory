#!/usr/bin/env bash
# Real-TUI play-test for #3530, via:
#
#   scripts/testbox.sh scenario scripts/tui-3530-scenario.sh
#
# The invariant this PR's app/ half exists for: a registered project whose
# recorded path no longer resolves must appear in the switch-project overlay
# ONCE, under the identity it RECORDED — not twice, with a second row keyed by
# that path's own hash.
#
# The fixture has to be a LINKED WORKTREE, and that is the whole difficulty.
# A plain repository's identity root IS its path, so its recorded RepoID equals
# the hash the root_agents fallback derives, and the two rows collapse to one
# whatever the code does — the first version of this scenario passed with the
# fix disabled for exactly that reason. A linked worktree's identity is its main
# repository's, so recorded id != path hash, and the fork is observable.
#
# Pre-fix the overlay unions the unresolvable root_agents key under the path
# hash, which is nobody's identity once the registry row is addressed by its
# recorded RepoID. The extra row has zero sessions, nothing to open, and a
# delete that delete-project refuses because it normalizes the same path back to
# the recorded identity — three dead ends the user cannot tell apart from the
# real row.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
BARE="$HOME/sandbox/repo2.git"
WT="$HOME/sandbox/mock-repo-2"

# A BARE repository's linked worktree. This is the one shape where the recorded
# root and the identity root differ: registration records the WORKSPACE (the
# worktree the user named) while identity comes from the bare common directory
# (#3361). A non-bare repo will not do — `af projects add` normalizes a linked
# worktree of one back to its main repo root, so the record names a path the
# root_agents key never mentions and no fork is possible. Measured, on the
# second version of this scenario.
git init -q -b master "$HOME/sandbox/repo2-src"
git -C "$HOME/sandbox/repo2-src" config user.email t@t
git -C "$HOME/sandbox/repo2-src" config user.name t
git -C "$HOME/sandbox/repo2-src" commit -q --allow-empty -m initial
git clone -q --bare "$HOME/sandbox/repo2-src" "$BARE"
git -C "$BARE" worktree add -q "$WT" master

af="$(_af_resolve_bin)"
deadline=$(( $(_af_now) + 180 ))
while [ ! -x "$af" ]; do
    [ "$(_af_now)" -ge "$deadline" ] && { echo "af binary never appeared" >&2; exit 1; }
    sleep 1
done

echo "=== registering the worktree as a project ==="
"$af" projects add "$WT"
"$af" projects list

# Prove the fixture before asserting on it: the recorded identity must differ
# from the recorded path's hash, or this scenario cannot see the defect.
# PROVE the fixture before asserting on it. The recorded identity must differ
# from the recorded path's own hash, or the two rows collapse to one whatever
# the code does and this scenario passes for every possible implementation --
# which is exactly what its first version did.
recorded="$("$af" projects list | grep -o '"repo_id": "[^"]*"' | tail -1 | cut -d'"' -f4)"
pathhash="$(printf '%s' "$WT" | sha256sum | cut -c1-12)"
echo "=== recorded identity: $recorded  ·  hash of the recorded path: $pathhash ==="
if [ -z "$recorded" ] || [ "$recorded" = "$pathhash" ]; then
    echo "FIXTURE INVALID: recorded identity must not be the recorded path's hash" >&2
    exit 1
fi
recroot="$("$af" projects list | grep -o '"root": "[^"]*"' | tail -1 | cut -d'"' -f4)"
if [ "$recroot" != "$WT" ]; then
    echo "FIXTURE INVALID: the record must name the worktree ($WT), got $recroot" >&2
    exit 1
fi

# The legacy path-keyed opt-in the overlay unions in. Written with the driver's
# helper so it replaces config.toml rather than appending a second table.
af_set_config "$(printf '[program_overrides]\nclaude = "bash"\n\n[root_agents]\n"%s" = {}\n' "$WT")"

# Now make the recorded path stop resolving, with a DETERMINATE answer: the
# directory is still there and git says it is not a repository. An unanswered
# probe deliberately does NOT take this path (that is the other half of the
# fix), so the fixture has to produce the answered one.
rm -f "$WT/.git"
echo "=== git verdict at the recorded root (must be a determinate no) ==="
git -C "$WT" rev-parse --show-toplevel 2>&1 | head -1 || true

af_boot || exit 1
af_ensure_nav
af_send C-p
af_wait_for 'Switch project' "$AF_DRIVER_TIMEOUT" 'project picker overlay' || exit 1
sleep 1

screen="$(af_capture)"
printf '%s\n' "$screen"

# Count INSIDE the overlay box only. The sidebar behind it lists the same
# project legitimately (that is the project list, not the picker), and counting
# the whole screen made the first run of this scenario report 2 for a correct
# render — the assertion has to name the region it is about.
overlay="$(printf '%s\n' "$screen" | sed -n '/Switch project/,/navigate/p')"
rows="$(printf '%s\n' "$overlay" | grep -c 'mock-repo-2' || true)"
echo "=== overlay rows naming mock-repo-2: $rows ==="
if [ "$rows" -ne 1 ]; then
    echo "FAIL: the unresolvable recorded root must collapse to ONE picker row, got $rows" >&2
    exit 1
fi
echo "PASS: one row for the unresolvable registered project"
