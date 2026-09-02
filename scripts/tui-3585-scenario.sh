#!/usr/bin/env bash
# Real-TUI regression for #3585, via:
#
#   scripts/testbox.sh scenario scripts/tui-3585-scenario.sh
#
# The invariant: a modal's mouse zones are registered where the modal is DRAWN.
#
# app's overlayOrigin decides both — placeOverlay renders at it, and the
# confirmation, search and selection overlays all call RegisterZones with it — but
# it measured with lipgloss.Width while the compositor clamped with
# ansi.PrintableRuneWidth and the zone columns were counted with
# runewidth.StringWidth. For clustered text those disagree (2, 8 and 2 cells for
# one ZWJ family; tmux advances 4), so the registered buttons ended up offset from
# the rendered ones. Everything now measures with layout.Cells.
#
# A real terminal is the only honest oracle here: the defect is that a CLICK
# lands somewhere other than the thing it was aimed at, which no unit test on a
# composited string can observe. So this drives real SGR mouse events at the words
# on screen and checks what happened.
#
# A session title carrying a ZWJ family is on screen throughout, so the clustered
# text is in the tree, in the search rows, and in the confirmation message.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

FAMILY='👨‍👩‍👧‍👦'
ZWJ_TITLE="fam${FAMILY}zzz"
# Anchors, not the title. The rendered row is NOT byte-identical to the requested
# title — measured: a title of "fam<family>zzz" comes back from capture-pane as
# "fam<family minus its last emoji>zzz", the cluster cut short somewhere between
# af, tmux and the capture. Matching on the full title therefore times out even
# though the session exists, so every wait and row lookup below anchors on the
# ASCII either side of the cluster, which is stable.
ANCHOR_HEAD='fam'
ANCHOR_TAIL='zzz'

# _new_zwj_instance <title> — af_new_instance waits for the literal title to
# appear, which a clustered title never does (see ANCHOR_HEAD). Same steps, but
# synced on the ASCII anchor plus the ready dot.
_new_zwj_instance() {
    local name="$1"
    af_ensure_nav
    af_focus_tree || return 1
    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'name prompt' || return 1
    af_wait_gone '[Ss]ession [Nn]ame:' 1 'old name prompt label' || return 1
    af_send_literal "$name"
    af_send Enter
    af_wait_for "${ANCHOR_HEAD}.*●" "$AF_DRIVER_TIMEOUT" "instance ready" || return 1
}

_seed_config() {
    af_set_config "default_program = \"claude\"

[program_overrides]
claude = \"bash\""
}

# _cell_col <line> <needle> — 0-based CELL column where <needle> starts in <line>.
#
# Not a byte offset: the sandbox runs in the C locale, so every byte-oriented tool
# reports a row of box-drawing and emoji as several times its real width, and a
# click computed that way lands off-screen. (#3433's scenario learned this the
# hard way with `awk length()`.) python3 counts characters and widens the East
# Asian Wide/Fullwidth ones, which is what the prefix of these rows contains.
_cell_col() {
    python3 - "$1" "$2" <<'PY'
import sys, unicodedata
line, needle = sys.argv[1], sys.argv[2]
i = line.find(needle)
if i < 0:
    print(-1); raise SystemExit
def w(ch):
    if ch == '‍' or unicodedata.combining(ch):
        return 0
    return 2 if unicodedata.east_asian_width(ch) in ('W', 'F') else 1
print(sum(w(c) for c in line[:i]))
PY
}

# _row_of <needle> — 1-based screen row containing <needle>.
_row_of() { af_capture | grep -nF -- "$1" | head -1 | cut -d: -f1; }

# af_select_zwj — put the tree cursor on the clustered session. af_select matches
# the literal title, which the rendered row does not carry (see ANCHOR_HEAD), so
# this walks to the row the anchor is on.
af_select_zwj() {
    af_ensure_nav
    af_focus_tree || return 1
    local i
    for i in $(seq 1 12); do
        if af_capture | grep -qE "▾[[:space:]]+${ANCHOR_HEAD}"; then
            af_capture | grep -qF 'D kill' && return 0
        fi
        af_send j
        sleep "$AF_DRIVER_POLL"
    done
    _af_fail "could not put the cursor on the clustered session"
    return 1
}

# --- confirmation: narrow zones, so the COLUMN has to be right ---------------
#
# The y/n zones are a few cells wide, which makes this the discriminating case:
# an offset origin moves the zone off the words entirely. Two clicks, because one
# proves nothing on its own — if the overlay dismissed on any click, a hit test
# would look like a pass.
drive_confirmation_zone() {
    _af_log "=== confirmation: the cancel zone must be ON the cancel words ==="
    af_select_zwj || return 1
    af_send D
    af_wait_for 'to cancel|esc cancel' "$AF_DRIVER_TIMEOUT" 'kill confirmation' || return 1

    local needle row line col
    needle='to cancel'
    row="$(_row_of "$needle")"
    [ -n "$row" ] || { _af_fail "confirmation instruction row not found"; return 1; }
    line="$(af_capture | sed -n "${row}p")"
    col="$(_cell_col "$line" "$needle")"
    [ "$col" -ge 0 ] || { _af_fail "could not locate '$needle' in row $row"; return 1; }
    _af_log "DEBUG confirm row=$row col=$col line=[$line]"

    # A click far from the zone must NOT dismiss it. Without this, a global
    # dismiss-on-click would make the real assertion below vacuous.
    af_click 2 "$row"
    if ! af_capture | grep -qE 'to cancel|esc cancel'; then
        _af_fail "the confirmation closed on a click OUTSIDE its zones, so hitting the zone proves nothing"
        return 1
    fi

    # And a click ON the words must dismiss it. Pre-fix the zone sits left of
    # them, so this click lands on nothing and the overlay stays up.
    af_click "$((col + 2))" "$row"
    af_wait_gone 'to cancel|esc cancel' "$AF_DRIVER_TIMEOUT" 'confirmation dismissed by clicking its cancel words' || {
        _af_fail "clicking the rendered cancel words did not hit their zone (#3585)"
        return 1
    }
    _af_log "=== confirmation: PASS ==="
}

# --- search: full-width row zones, and the rows carry the clustered title ----
drive_search_zone() {
    _af_log "=== search: clicking a result row must select that session ==="
    af_ensure_nav
    af_send '/'
    af_wait_for "$ANCHOR_HEAD" "$AF_DRIVER_TIMEOUT" 'search overlay listing the session' || return 1

    local row line col
    row="$(_row_of "$ANCHOR_TAIL")"
    [ -n "$row" ] || { _af_fail "the clustered-title row is not on screen"; return 1; }
    line="$(af_capture | sed -n "${row}p")"
    col="$(_cell_col "$line" "$ANCHOR_HEAD")"
    [ "$col" -ge 0 ] || { _af_fail "could not locate the row text"; return 1; }

    # The row zone spans [origin.X, origin.X+W) — it is full-width within the
    # MODAL, not within the frame, and a centered modal does not start at column
    # zero. So click where the text actually is.
    af_click "$((col + 2))" "$row"
    af_wait_gone 'esc close|esc cancel' 3 'search overlay closed by the row click' || true
    if af_capture | grep -qF 'Search'; then
        _af_fail "clicking the search row did not hit its zone: the overlay is still open (#3585)"
        return 1
    fi
    af_capture | grep -qE "${ANCHOR_HEAD}.*●" || {
        _af_fail "after clicking the search row the session is not the selected one"
        return 1
    }
    _af_log "=== search: PASS ==="
}

# --- selection: full-width row zones -----------------------------------------
drive_selection_zone() {
    _af_log "=== selection: clicking a picker row must take that row ==="
    af_ensure_nav
    af_focus_tree || return 1
    af_send t
    af_wait_for 'New tab' "$AF_DRIVER_TIMEOUT" 'new-tab picker' || return 1

    local row line col
    row="$(_row_of 'Terminal')"
    [ -n "$row" ] || { _af_fail "no 'Terminal' row in the picker"; return 1; }
    line="$(af_capture | sed -n "${row}p")"
    col="$(_cell_col "$line" 'Terminal')"
    [ "$col" -ge 0 ] || { _af_fail "could not locate the 'Terminal' row text"; return 1; }
    af_click "$((col + 2))" "$row"
    af_wait_gone 'New tab' "$AF_DRIVER_TIMEOUT" 'picker dismissed by the row click' || {
        _af_fail "clicking the picker row did not hit its zone (#3585)"
        return 1
    }
    _af_log "=== selection: PASS ==="
}

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
af_reset_sandbox
_seed_config
af_boot
_new_zwj_instance "$ZWJ_TITLE"

# The clustered title is genuinely on screen — the precondition for every zone
# below, since it is what makes the measures disagree.
af_capture | grep -qF '👨' || _af_fail "precondition: the clustered title must be rendered on screen"

drive_confirmation_zone

_af_log "#3585 scenario: PASS"
