#!/usr/bin/env bash
# Real-TUI drive for #2477 (run af anywhere), via:
#
#   scripts/testbox.sh scenario scripts/tui-2477-scenario.sh
#
# The behavior under test: `af` launched from a directory that is NOT inside a
# git repository must open the TUI in registry mode — the same rail, all known
# sessions/projects, a project selectable from the Projects section — instead of
# erroring "failed to determine project context". Pre-#2477 af_boot below times
# out here because af printed that error and drew no frame.
#
# Driven at 100x30 and 80x24 because this is visible TUI. git itself is still
# required; only the "must be inside a repo" restriction is lifted.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

drive_launch_outside_repo() {
    export AF_DRIVER_COLS="$1" AF_DRIVER_ROWS="$2"

    # A directory that is deliberately NOT a git repository. af_boot cd's into
    # AF_DRIVER_REPO and launches af there; a non-repo cwd is exactly the #2477
    # case. af_reset_sandbox skips worktree/branch cleanup for it (no .git).
    local nonrepo
    nonrepo="$(mktemp -d "${TMPDIR:-/tmp}/af-2477-nonrepo.XXXXXX")"
    export AF_DRIVER_REPO="$nonrepo"

    af_reset_sandbox
    # af_boot waits for the 'Agent Factory' first frame AND the rail marker; both
    # only appear if af actually launched. Its success IS the core #2477 proof.
    af_boot

    # Belt and suspenders: the old not-a-repo launch error must not be on screen.
    local screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qiE 'failed to determine project context|must be run from within a git repository'; then
        _af_fail "#2477: af launched outside a repo but the screen still shows the not-a-repo error (${1}x${2}):"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    _af_log "assert OK: af launched outside a git repository with no project-context error (${1}x${2})"

    rm -rf "$nonrepo"
    echo "PASS: #2477 launch outside a git repo at ${1}x${2}"
}

drive_launch_outside_repo 100 30
drive_launch_outside_repo 80 24
echo "PASS: #2477 real-TUI scenario (launch outside a git repository at both geometries)"
