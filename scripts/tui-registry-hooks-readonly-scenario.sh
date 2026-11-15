#!/usr/bin/env bash
# Real-TUI drive for the registry-mode hooks read-only fix, via:
#
#   scripts/testbox.sh scenario scripts/tui-registry-hooks-readonly-scenario.sh
#
# The behavior under test: in registry mode (#2477, launched outside a git
# repo), the hooks editor (`e`) must open READ-ONLY with an explanatory reason
# instead of inviting edits that can never be saved. Before the fix, the overlay
# opened editable, the empty-state said "No hooks configured. Press Enter to
# focus, then n to add.", and every edit entered the permanent failed-save
# cycle ("repo root is required to save in-repo config").
#
# Driven at 100x30 because the read-only reason and footer must be legible.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

drive_registry_hooks_read_only() {
    export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30

    # A directory that is deliberately NOT a git repository — exactly the
    # #2477 registry-mode case. af_boot cd's into AF_DRIVER_REPO and launches af.
    local nonrepo
    nonrepo="$(mktemp -d "${TMPDIR:-/tmp}/af-reg-hooks-nonrepo.XXXXXX")"
    export AF_DRIVER_REPO="$nonrepo"

    af_reset_sandbox
    af_boot

    # In registry mode newHome lands focus on the Projects section when it has
    # rows; the empty non-repo fresh sandbox has the mock-repo registered, so
    # move focus to the tree first (Esc leaves the captive Projects section),
    # then `e` opens the hooks overlay from the tree.
    af_send Escape
    af_send e

    # The overlay must open: its title renders. Wait for that frame rather than
    # capturing straight after the keys — on a loaded testbox the capture can
    # still hold the frame from before they were processed (#7324).
    local screen
    if ! af_wait_for 'Post-worktree hooks'; then
        _af_fail "registry mode: hooks overlay did not open (no 'Post-worktree hooks' title)"
        rm -rf "$nonrepo"
        return 1
    fi
    screen="$(af_capture)"

    # The read-only reason must render and the editable-pane invitation must NOT.
    if ! printf '%s\n' "$screen" | grep -q 'Hooks belong to a project'; then
        _af_fail "registry mode: the read-only reason 'Hooks belong to a project' is missing:"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    if printf '%s\n' "$screen" | grep -q 'No hooks configured. Press Enter to focus, then n to add.'; then
        _af_fail "registry mode: the editable-pane invitation that lured the doomed edit still renders:"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    # The read-only footer, not the editable one.
    if ! printf '%s\n' "$screen" | grep -q 'esc back'; then
        _af_fail "registry mode: the read-only footer 'esc back' is missing:"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    if printf '%s\n' "$screen" | grep -q 'n add'; then
        _af_fail "registry mode: the editable footer 'n add' renders on a read-only pane:"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    _af_log "assert OK: registry-mode hooks overlay is read-only with the reason and no edit invitation"

    # Pressing `n` must NOT open an add field (the pane is read-only). Capture
    # after the key and assert no `enter save` footer appears (the add form's
    # footer) and the reason is still shown. This one asserts an ABSENCE on a
    # frame the key must not change, so there is no frame to wait for: a short
    # settle is all that can be done, and a late capture can only miss a
    # regression, never report a false one.
    af_send n
    sleep 0.5
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -q 'enter save'; then
        _af_fail "registry mode: pressing 'n' opened an add form on a read-only hooks pane:"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    if ! printf '%s\n' "$screen" | grep -q 'Hooks belong to a project'; then
        _af_fail "registry mode: the read-only reason disappeared after pressing 'n' (an edit was started):"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    _af_log "assert OK: 'n' is inert on the read-only registry-mode hooks pane (no add form)"

    # Esc closes the overlay; NO 'failed to save hooks' / 'repo root is required'
    # error must appear (the whole failed-save cycle is what this fix prevents).
    # Wait for the frame with the overlay gone before reading it (#7324).
    af_send Escape
    if ! af_wait_gone 'Post-worktree hooks'; then
        _af_fail "registry mode: Esc did not close the hooks overlay"
        rm -rf "$nonrepo"
        return 1
    fi
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qiE 'failed to save hooks|repo root is required to save in-repo config'; then
        _af_fail "registry mode: closing the read-only hooks overlay surfaced a save error:"
        printf '%s\n' "$screen" >&2
        rm -rf "$nonrepo"
        return 1
    fi
    _af_log "assert OK: closing the read-only hooks overlay raised no save error"

    rm -rf "$nonrepo"
    echo "PASS: registry-mode hooks overlay is read-only (no unsaveable edits, no failed-save cycle)"
}

drive_registry_hooks_read_only
echo "PASS: registry-mode hooks read-only real-TUI scenario"
