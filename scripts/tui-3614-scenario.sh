#!/usr/bin/env bash
# Real-TUI regression for #3614 and #723, via:
#
#   scripts/testbox.sh scenario scripts/tui-3614-scenario.sh
#
# Two invariants, both about a row whose true width no library function in this
# tree can report:
#
#   #3614  A fixed rectangle is NEVER overflowed, and the row keeps its ● status
#          dot while not overflowing. Under-filling is the accepted cost.
#   #723   A modal's background prefix is budgeted in the measure its position is
#          read in, so background cells left of the overlay are not erased.
#
# A real terminal is the only honest oracle for either. tmux is what actually
# advances the cursor over a chained ZWJ family — 4 cells, where x/ansi and
# lipgloss say 2 and PrintableRuneWidth says 8 — so "did this row leave the
# rectangle" is a question only tmux can answer. A unit test on the composed
# string can assert the BOUND (and does, in ui/layout and ui); it cannot see the
# wrap.
#
# The oracles here are structural, never width measurements: a row that draws
# past the terminal WRAPS, pushing the next rendered row down, and a status glyph
# either is on a row or is not. Nothing below has to agree with anyone about how
# wide the family is — which matters, because nothing agrees.
#
# What this scenario CAN and CANNOT prove, measured rather than assumed
#
# The testbox runs tmux 3.3a, and 3.3a does NOT reproduce the width disagreement
# the issue measured on 3.4: it stores this family in the cells x/ansi reports,
# and the capture comes back with the cluster's last emoji missing (#3585's
# scenario recorded the same thing). So capture-pane is not a faithful cell grid
# for clustered content here, which rules out any column arithmetic on it — and
# the rail row does not actually overflow in this container even on master.
#
# What that means for each check below, stated plainly so nobody reads a green
# run as more than it is:
#
#   rail       PASSES on master here, because 3.3a does not disagree. It is a
#              REGRESSION guard, not a reproduction: it goes red the moment the
#              status glyph stops being laid out from the rectangle's edge — that
#              mutation was run, and the scenario cannot even create the session,
#              because the row no longer matches "fam.*●".
#   overlay    FAILS on master here. Verified by mutation: with the prefix cut by
#              truncate.String again, the clustered row loses its ● dot at 200
#              columns while the plain control row keeps its own.
#   zones      The #3585 contract under #3614's measure. Clicks land on the words.
#
# The overflow itself is asserted by BOUND in the unit tests (ui/layout and ui),
# where the assertion is "this row cannot exceed the rectangle whatever tmux does
# with the cluster" — which is the only form of that claim a machine can check
# without a tmux 3.4 to hand.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

FAMILY='👨‍👩‍👧‍👦'
ZWJ_TITLE="fam${FAMILY}zzz"
# Anchors, not the title. The rendered row is NOT byte-identical to the requested
# title — measured in #3585's scenario: the cluster comes back from capture-pane
# cut short somewhere between af, tmux and the capture. So every wait and row
# lookup anchors on the ASCII either side of it, which is stable.
ANCHOR_HEAD='fam'
# A control session with the SAME row shape and no clustered content. The #723
# check is a differential oracle against it: what happens to this row is what
# should happen to the clustered one.
PLAIN_TITLE='plainzzz'

# _new_instance <title> <ready-anchor> — af_new_instance waits for the literal
# title to appear, which a clustered title never does (see ANCHOR_HEAD). Same
# steps, synced on an anchor the caller names plus the ready dot.
_new_instance() {
    local name="$1" anchor="$2"
    af_ensure_nav
    af_focus_tree || return 1
    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'name prompt' || return 1
    af_wait_gone '[Ss]ession [Nn]ame:' 1 'old name prompt label' || return 1
    af_send_literal "$name"
    af_send Enter
    af_wait_for "${anchor}.*●" "$AF_DRIVER_TIMEOUT" "instance ready" || return 1
}

_seed_config() {
    af_set_config "default_program = \"claude\"

[program_overrides]
claude = \"bash\""
}

# _row_of <needle> — 1-based screen row containing <needle>.
_row_of() { af_capture | grep -nF -- "$1" | head -1 | cut -d: -f1; }

# _tree_row_of <anchor> — 1-based screen row of the TREE row for <anchor>.
#
# Not _row_of: the workspace pane's header carries the selected session's title
# too, and it sits ABOVE the tree row, so a plain first-match lookup finds the
# header — which legitimately has no status dot — and the check fails on the
# harness rather than on the product. Anchored on the tree's own ▾/▸ arrow.
_tree_row_of() {
    af_capture | grep -nE "[▾▸][[:space:]]+$1" | head -1 | cut -d: -f1
}

# _cell_col <line> <needle> — 0-based CELL column where <needle> starts.
# Character-oriented, not byte-oriented: the sandbox runs in the C locale, so
# every byte-oriented tool reports a row of box-drawing and emoji as several
# times its real width and a click computed that way lands off-screen (#3433
# learned this with `awk length()`).
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

# --- #3614: the rail row must not overflow, and must keep its dot -----------
#
# 88 columns puts the rail at exactly its 22-column minimum: the grid clamps it
# to clamp(22, 25%·W, 36) (layout.TreeMinWidth, #1090), so 25%·88 = 22. That is
# the tightest SUPPORTED width, which is where a 2-cell overflow is most likely
# to matter and least likely to be absorbed by slack.
drive_rail_row() {
    _af_log "=== rail: the clustered row must keep its ● and stay inside 22 columns ==="
    af_resize 88 40 || return 1
    af_ensure_nav

    local title_row branch_row
    title_row="$(_tree_row_of "$ANCHOR_HEAD")"
    if [ -z "$title_row" ]; then
        af_capture >&2
        _af_fail "the clustered session's tree row is not on screen"
        return 1
    fi

    # Half one: the status dot survives the width accounting. Bounding the row
    # without laying the glyph out from the rectangle's edge just trades the
    # overflow for a missing status marker — the measured harm that made #3610
    # withdraw the overestimate as the general measure.
    if ! af_capture | sed -n "${title_row}p" | grep -qF '●'; then
        af_capture >&2
        _af_fail "the Ready session lost its ● status dot at the 22-column rail (#3614)"
        return 1
    fi

    # Half two: the row did not wrap. The branch line is rendered as the row
    # directly under the title; if the title overflowed the terminal, tmux wrapped
    # it and pushed the branch line one row further down.
    # Scoped to THIS session's block, not the first ⎇ on screen: other sessions
    # have branch lines too, and matching the wrong one turns a harness mistake
    # into a product failure.
    branch_row=$((title_row + 1))
    if ! af_capture | sed -n "${branch_row}p" | grep -qF '⎇'; then
        af_capture >&2
        _af_fail "the clustered rail row wrapped: row $branch_row should be this session's ⎇ \
branch line and is not — the title drew past the terminal, tmux wrapped it, and every height \
budget below it is now off by one (#3614/#3430)"
        return 1
    fi

    # And the frame still ends where it should: a wrap anywhere above would push
    # the status bar off the bottom.
    if ! af_capture | tail -3 | grep -qE 'quit|help'; then
        af_capture >&2
        _af_fail "the status bar is not on the last rows — something above it wrapped"
        return 1
    fi
    _af_log "=== rail: PASS ==="
}

# --- #723: a modal must not leave or erase background beside itself ----------
#
# History: #723 was a rune-wise prefix cut read with a grapheme-aware width, so a
# clustered row's prefix came up short and the padding blanked real background
# cells left of the modal. #5297 then changed the compositor's policy: on every
# row the modal OCCUPIES the cells either side of it are blank backdrop, never
# the background's own cells; only rows outside the modal's vertical span keep
# their backdrop. That makes the old oracle ("the clustered row keeps its dot
# beside the modal") vacuous (#5697): a surviving dot can only sit on a row the
# modal does not touch, where no compositing happens at all.
#
# The oracle that can still fail is the policy itself, checked ON rows inside
# the modal's vertical band:
#   * the clustered row and the plain row both sit inside the band, and each
#     carried a ● dot within a cluster's width of the modal's left edge before it
#     opened, so the dot WOULD be written there by a compositor that copies or
#     cuts the background (the #723 cut, or a regression of #5297);
#   * after the modal opens neither row keeps any ●, i.e. the margin is blank;
#   * the modal's own left edge is at the same column on both rows, so a
#     miscounted prefix on the clustered row would shift it;
# Mutation check (see the PR body): restoring the background prefix on modal
# rows fails the "margin is blank" assertion at the width this finds.
#
# The geometry is SEARCHED FOR (widths and heights) rather than hard-coded, and
# if nothing in the sweep puts both rows in the band the scenario fails loudly
# instead of passing where the defect could not have shown.
drive_overlay_background() {
    _af_log "=== overlay: modal rows have blank margins, rows outside keep the backdrop (#723/#5297) ==="

    local width height zwj_row plain_row found="" screen
    local top bottom modal_line plain_line dot_col modal_col zwj_modal_col
    for height in 40 30 24 20 16 14 12; do
    for width in $(seq 90 2 200); do
        af_resize "$width" "$height" || return 1
        af_ensure_nav
        zwj_row="$(_tree_row_of "$ANCHOR_HEAD")"
        plain_row="$(_tree_row_of "$PLAIN_TITLE")"
        if [ -z "$zwj_row" ] || [ -z "$plain_row" ]; then continue; fi
        # Both rows must carry the dot with NO modal up, or there is nothing for a
        # modal to erase.
        af_capture | sed -n "${zwj_row}p"   | grep -qF '●' || continue
        af_capture | sed -n "${plain_row}p" | grep -qF '●' || continue
        plain_line="$(af_capture | sed -n "${plain_row}p")"
        dot_col="$(_cell_col "$plain_line" '●')"

        af_send '/'
        af_wait_for 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search overlay' || return 1
        screen="$(af_capture)"
        top="$(printf '%s\n' "$screen" | grep -n -m1 '╭' | cut -d: -f1)"
        bottom="$(printf '%s\n' "$screen" | grep -n '╰' | tail -1 | cut -d: -f1)"
        if [ -n "$top" ] && [ -n "$bottom" ] \
            && [ "$zwj_row" -ge "$top" ] && [ "$zwj_row" -le "$bottom" ] \
            && [ "$plain_row" -ge "$top" ] && [ "$plain_row" -le "$bottom" ]; then
            modal_line="$(printf '%s\n' "$screen" | sed -n "${plain_row}p")"
            modal_col="$(_cell_col "$modal_line" '│')"
            zwj_modal_col="$(_cell_col "$(printf '%s\n' "$screen" | sed -n "${zwj_row}p")" '│')"
            # The dot has to be where the modal now is, or a copying compositor
            # would not have shown it either.
            if [ "$modal_col" -gt "$dot_col" ] && [ "$((modal_col - dot_col))" -le 7 ]; then
                found="${width}x${height}"
                break 2
            fi
        fi
        af_send Escape
        af_wait_gone 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search overlay closed' || return 1
    done
    done

    if [ -z "$found" ]; then
        af_capture >&2
        _af_fail "no terminal size in the sweep puts BOTH dotted rows inside the search overlay's \
vertical span with the ● dot within a cluster's width of its left edge, so this check cannot \
discriminate. Re-derive the geometry rather than deleting the check — a vacuous pass here is how \
#723 stayed closed for three months, and how this check sat vacuous after #5297 (#5697)."
        return 1
    fi

    local row
    for row in "$plain_row" "$zwj_row"; do
        if printf '%s\n' "$screen" | sed -n "${row}p" | grep -qF '●'; then
            af_capture >&2
            _af_fail "row $row is inside the overlay's vertical span at ${found} but still shows a ● \
beside the modal: the margin on a modal-occupied row must be blank backdrop, never the \
background's own cells (#5297/#723)"
            return 1
        fi
    done
    if [ "$modal_col" != "$zwj_modal_col" ]; then
        af_capture >&2
        _af_fail "the modal's left edge is at column $modal_col on the plain row but \
$zwj_modal_col on the clustered row at ${found}: the clustered row's prefix was mis-measured (#723)"
        return 1
    fi
    _af_log "=== overlay: PASS at ${found} (both rows in the band, margins blank, edge aligned) ==="

    af_send Escape
    af_wait_gone 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search overlay closed' || return 1
}

# --- zones: the #3585 contract must still hold ------------------------------
#
# #3614 changes what a row's width MEANS, and #3585's whole subject is that the
# modal is drawn where its mouse zones were registered. So both click-verified
# overlays from scripts/tui-3585-scenario.sh are re-driven here against the new
# measure, with a clustered title on screen throughout.
drive_confirmation_zone() {
    _af_log "=== confirmation: the cancel zone must be ON the cancel words ==="
    af_resize 120 40 || return 1
    af_ensure_nav
    af_focus_tree || return 1

    for _ in $(seq 1 12); do
        if af_capture | grep -qE "▾[[:space:]]+${ANCHOR_HEAD}" && af_capture | grep -qF 'D delete'; then
            break
        fi
        af_send j
        sleep "$AF_DRIVER_POLL"
    done
    af_send D
    af_wait_for 'to cancel|esc cancel' "$AF_DRIVER_TIMEOUT" 'kill confirmation' || return 1

    local needle row line col
    needle='esc cancel'
    row="$(_row_of "$needle")"
    [ -n "$row" ] || { _af_fail "confirmation instruction row not found"; return 1; }
    line="$(af_capture | sed -n "${row}p")"
    col="$(_cell_col "$line" "$needle")"
    [ "$col" -ge 0 ] || { _af_fail "could not locate '$needle' in row $row"; return 1; }

    # A click far from the zone must NOT dismiss it, or hitting the zone proves
    # nothing — a global dismiss-on-click would pass the real assertion below.
    af_click 2 "$row"
    if ! af_capture | grep -qE 'to cancel|esc cancel'; then
        _af_fail "the confirmation closed on a click OUTSIDE its zones, so hitting the zone proves nothing"
        return 1
    fi
    af_click "$((col + 2))" "$row"
    af_wait_gone 'to cancel|esc cancel' "$AF_DRIVER_TIMEOUT" 'confirmation dismissed by clicking its cancel words' || {
        _af_fail "clicking the rendered cancel words did not hit their zone (#3585 under #3614's measure)"
        return 1
    }
    _af_log "=== confirmation: PASS ==="
}

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
        _af_fail "clicking the picker row did not hit its zone (#3585 under #3614's measure)"
        return 1
    }
    _af_log "=== selection: PASS ==="
}

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
af_reset_sandbox
_seed_config
af_boot
_new_instance "$PLAIN_TITLE" "$PLAIN_TITLE"
_new_instance "$ZWJ_TITLE" "$ANCHOR_HEAD"

# The clustered title is genuinely on screen — the precondition for every check
# below, since it is what makes the measures disagree.
af_capture | grep -qF '👨' || _af_fail "precondition: the clustered title must be rendered on screen"

drive_rail_row
drive_overlay_background
drive_selection_zone
drive_confirmation_zone

_af_log "#3614/#723 scenario: PASS"
