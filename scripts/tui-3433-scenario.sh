#!/usr/bin/env bash
# Real-TUI regression for #3433, via:
#
#   scripts/testbox.sh scenario scripts/tui-3433-scenario.sh
#
# The invariant: opening a modal never costs the user the frame behind it.
#
# PlaceOverlay measured the foreground with ansi.PrintableRuneWidth and, when that
# read wider than the terminal, returned the modal ALONE — the whole background
# frame gone. That is reachable without any modal being oversized, because the
# measures disagree: for one joined-emoji family lipgloss.Width says 2 cells,
# PrintableRuneWidth says 8, and tmux 3.4 actually advances 4. A modal built from
# user text can therefore be certain it fits while the compositor is certain it
# does not.
#
# A real terminal is the oracle because the failure is a WHOLE-SCREEN one: the unit
# tests assert on the composited string, but what the user experiences is every
# pane vanishing behind a modal. Here we assert both halves on a live screen — the
# modal is up, AND the frame is still underneath it.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# Twelve joined-emoji families: 96 cells under the compositor's per-codepoint
# measure, comfortably past an 80-column frame, while every cell-accurate measure
# puts the string at 24 and tmux at 48. The config value is free-form (it names a
# shell command), so nothing validates or rewrites it on the way in.
FAMILY='👨‍👩‍👧‍👦'
WIDE_VALUE="${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}${FAMILY}"

_seed_config() {
    af_set_config "default_program = \"claude\"
on_archive_command = \"$WIDE_VALUE\"

[program_overrides]
claude = \"bash\""
}

# _frame_is_alive — the background frame is still composited under the modal.
# The app title belongs to the frame chrome and never to a modal, so seeing it
# proves the compositor did not hand back the modal alone.
_frame_is_alive() {
    af_capture | grep -qF 'Agent Factory'
}

# _dump <why> — put the live screen in the log before failing, so a broken
# assertion is diagnosable from the run instead of by guessing at markers.
_dump() {
    printf '[tui-driver] --- screen (%s) ---\n' "$1" >&2
    af_capture >&2
    printf '[tui-driver] --- end screen ---\n' >&2
}

# drive_overlay_keeps_the_frame <cols> <rows> <composited|fullscreen>
#
# The third argument is not a convenience — it is the difference between the bug
# and a designed behaviour. Below roughly 100 columns the config overlay takes the
# #1821 FULL-SCREEN fallback, which covers the frame on purpose. Asserting
# "frame visible behind the modal" there would fail on correct code, and a
# scenario that cries wolf on a design decision is worse than no scenario. So the
# narrow geometry asserts the other half instead: the frame comes BACK when the
# modal closes, which a destroyed frame cannot do.
drive_overlay_keeps_the_frame() {
    local cols="$1" rows="$2" mode="$3"
    export AF_DRIVER_COLS="$cols" AF_DRIVER_ROWS="$rows"
    _af_log "=== ${cols}x${rows}: a modal carrying clustered text must not blank the frame ==="
    af_reset_sandbox
    _seed_config
    af_boot

    # Precondition: the frame is up before any modal exists.
    _frame_is_alive || { _dump "boot"; _af_fail "precondition: the frame must be on screen before the modal opens"; }

    af_ensure_nav
    af_send ','
    af_wait_for 'Config ' "$AF_DRIVER_TIMEOUT" 'config overlay' || return 1

    # Both halves, on the live screen.
    af_capture | grep -qF 'Config ' || _af_fail "the modal itself must still render"
    if [ "$mode" = composited ]; then
        _frame_is_alive || { _dump "modal open"; _af_fail \
            "the frame was DROPPED: the config modal is on screen but the frame behind it is gone (#3433)"; }
    elif _frame_is_alive; then
        # Not a failure, but worth knowing: if the fallback ever stops being
        # full-screen this geometry silently stops testing what it thinks it does.
        _af_log "note: ${cols}x${rows} composited rather than taking the full-screen fallback"
    fi

    # No width assertion here, deliberately. The sandbox runs in the C locale, so
    # awk/wc measure the capture BYTE-wise, and one joined-emoji family is 25 bytes
    # against 2-8 cells depending on whose measure you ask — a "300 columns in a
    # 120-column terminal" reading that says nothing about the screen. (Measured:
    # that is exactly what the first version of this scenario reported.) Cell width
    # is what the unit tests assert, on the composited string, where the measure is
    # unambiguous. What a real terminal adds is the whole-screen oracle above: the
    # frame is still there.

    af_send Escape
    af_wait_gone 'Config ' "$AF_DRIVER_TIMEOUT" 'config overlay closed' || return 1
    _frame_is_alive || _af_fail "the frame must survive closing the modal too"
    _af_log "=== ${cols}x${rows}: PASS ==="
}

# Two geometries: an ordinary terminal, and one narrow enough that the overlay
# takes the #1821 full-screen fallback, where the clip is far more likely to fire.
# 120x40 is the #3433 oracle: a real overlay composited over a real frame, with
# content the compositor measures at 96 cells in a 120-column terminal. Pre-fix
# this is the geometry where the frame disappears.
drive_overlay_keeps_the_frame 120 40 composited
# 80x30 takes the full-screen fallback; it proves the frame is restored, not
# destroyed, and that the clip does not corrupt the modal at a tighter width.
drive_overlay_keeps_the_frame 80 30 fullscreen

_af_log "#3433 scenario: PASS"
