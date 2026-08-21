#!/usr/bin/env bash
# Real-TUI regression for #3430, via:
#
#   scripts/testbox.sh scenario scripts/tui-3430-scenario.sh
#
# The invariant: the config pane never renders a line wider than itself. A wider
# line is wrapped by the overlay frame, and the pane's height window budgets by
# counting the lines renderRowLines produces — so a wrapped row makes that count
# a lie and the pane overflows its box.
#
# Two ways it was broken, and a real terminal is the oracle for both: an
# over-wide row wraps, which SEPARATES fragments that belong on one line.
#
#   A/B (narrow pane): the hint row composed five hints, shed the one
#       hintDropOrder could shed, and the remainder was still 43 cells. Below
#       ~44 the row wrapped, which is reachable — the config overlay takes the
#       #1821 full-screen fallback on a narrow terminal, and af renders normally
#       down to layout.HardMinWidth (40 columns).
#
#   C (any pane): the value field's width was a flat pane-24, correct only for a
#       20-cell key, so editing a long-keyed row overflowed at EVERY geometry.
#       120x40 is an ordinary terminal, not a narrow one.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# A long value on a 20-cell key: the field must show its TAIL on the key's row.
# The path is free-form (it only names a VS Code binary), so nothing validates it.
EDIT_VALUE='/opt/very/long/path/to/code-server/bin/TAILMARK'
EDIT_KEY='vscode_server_binary'
EDIT_TAIL='TAILMARK'

_seed_config() {
    af_set_config "default_program = \"claude\"
vscode_server_binary = \"$EDIT_VALUE\"

[program_overrides]
claude = \"bash\""
}

# _open_config — open the config overlay, waiting on the HEADER rather than the
# driver's af_open_config, which waits on the literal phrase "esc close".
# Pre-fix that phrase is exactly what wraps across two rows, so the shared helper
# times out before the assertions below can name the actual defect (measured: the
# 48x30 run died at "TIMEOUT 25s waiting for: config editor" with
# "│  ↑/↓ move · ↵ edit · C assistant · esc │ / │  close │" on screen).
_open_config() {
    af_ensure_nav
    af_send ','
    af_wait_for 'Config ' "$AF_DRIVER_TIMEOUT" 'config editor (header)' || return 1
}

# _row_with <literal> — the first captured row containing a literal string.
# -F, not -E: '›' and '↵' must never be read as a regex, and the sandbox's
# C/POSIX locale matches bracket expressions byte-wise anyway.
_row_with() { af_capture | grep -F -- "$1" | head -1 || true; }

# --- A/B: the hint row fits a narrow pane, and still advertises the exit ----
drive_narrow_hints() {
    local cols="$1" rows="$2" row
    export AF_DRIVER_COLS="$cols" AF_DRIVER_ROWS="$rows"
    af_reset_sandbox
    _seed_config
    af_boot
    _open_config

    # The exit survives every degradation, ON ONE ROW. Pre-fix this is already
    # where it dies: the over-wide row wraps mid-phrase, so "esc close" is not
    # contiguous anywhere on screen (measured at 48x30:
    # "│  ↑/↓ move · ↵ edit · C assistant · esc │" / "│  close │").
    af_assert_screen 'esc close' "the exit hint renders contiguously at ${cols}x${rows}" || return 1

    row="$(_row_with 'esc close')"
    _af_log "#3430 hint row (${cols}x${rows}): $row"
    if ! printf '%s' "$row" | grep -qF -- '↵ edit'; then
        _af_fail "#3430: at ${cols}x${rows} the hint row wrapped — 'esc close' is on a different physical row from '↵ edit', so the pane rendered a line wider than itself:"
        af_capture >&2
        return 1
    fi
    # Shedding is supposed to be the mechanism, so prove something WAS shed at
    # this width rather than the row happening to fit whole.
    if printf '%s' "$row" | grep -qF -- 'advanced'; then
        _af_fail "#3430: at ${cols}x${rows} the advanced toggle survived on a row this narrow, so nothing was shed: [$row]"
        return 1
    fi
    echo "PASS: #3430 hint row fits a ${cols}-column terminal and keeps the exit"
}

# --- C: the value field is sized from the row it renders into ---------------
drive_edit_field() {
    local cols="$1" rows="$2" row i
    export AF_DRIVER_COLS="$cols" AF_DRIVER_ROWS="$rows"
    af_reset_sandbox
    _seed_config
    af_boot
    _open_config

    # Put the CURSOR on the row (the '›' marker), not merely on screen: only the
    # selected row can be opened for editing.
    for i in $(seq 1 40); do
        [ -n "$(_row_with "› $EDIT_KEY")" ] && break
        af_send j
        sleep "$AF_DRIVER_POLL"
    done
    if [ -z "$(_row_with "› $EDIT_KEY")" ]; then
        _af_fail "#3430: never landed the cursor on '$EDIT_KEY'"
        af_capture >&2
        return 1
    fi

    af_send Enter
    af_wait_for 'esc cancel' "$AF_DRIVER_TIMEOUT" 'the value field opened' || return 1

    row="$(_row_with "› $EDIT_KEY")"
    _af_log "#3430 edit row (${cols}x${rows}): $row"
    if ! printf '%s' "$row" | grep -qF -- "$EDIT_TAIL"; then
        _af_fail "#3430: the open value field is not on its key's row at ${cols}x${rows} — the row was wider than the pane, so the frame wrapped the field off it:"
        af_capture >&2
        return 1
    fi
    af_close_config
    echo "PASS: #3430 the value field fits its row at ${cols}x${rows}"
}

# 48 columns puts the pane at ~42 cells (one under the un-sheddable 43) and 44
# at ~38; both are above af's 40-column hard minimum.
drive_narrow_hints 48 30
drive_narrow_hints 44 30
drive_edit_field 120 40

echo "PASS: #3430 the config pane fits its box at 48x30, 44x30 and 120x40"
