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

# --- #723: a modal must not erase the background left of itself --------------
#
# The A/B from the issue, driven through the real compositor — as a DIFFERENTIAL
# oracle, because the capture is not a trustworthy cell grid for clustered
# content (see the note at the top of this file) and any column arithmetic on it
# would be measuring the harness rather than af.
#
# Two sessions with identical row shapes are on screen: one titled with the ZWJ
# family, one plain. Both rail rows carry a ● status dot at the rail's right
# edge. The prefix left of an overlay was cut with a rune-wise truncator while
# its width was read with the grapheme-aware one, so on a row carrying a cluster
# the prefix came up short by the amount the two disagree — six cells for this
# family — and the padding blanked that many real background cells immediately
# left of the modal. On the plain row the two agree and nothing is blanked.
#
# So the plain row's dot is the CONTROL and the clustered row's dot is the
# assertion: same row shape, same modal, same column, and the only difference is
# the cluster.
#
# The geometry is SEARCHED FOR rather than hard-coded. The check needs the dot to
# be background (left of the modal) and inside the erased window, which is a band
# of terminal widths that moves whenever the help text or the rail clamp changes.
# Searching means the gate re-derives it instead of quietly going vacuous, and if
# no width in the sweep works the scenario fails loudly rather than passing on a
# geometry where the defect could not have shown.
drive_overlay_background() {
    _af_log "=== overlay: the modal must not erase background left of itself (#723) ==="

    local width zwj_row plain_row found=""
    for width in 200 190 180 210 170 160; do
        af_resize "$width" 40 || return 1
        af_ensure_nav
        zwj_row="$(_tree_row_of "$ANCHOR_HEAD")"
        plain_row="$(_tree_row_of "$PLAIN_TITLE")"
        if [ -z "$zwj_row" ] || [ -z "$plain_row" ]; then continue; fi
        # Both rows must carry the dot with NO modal up, or there is nothing for a
        # modal to erase.
        af_capture | sed -n "${zwj_row}p"   | grep -qF '●' || continue
        af_capture | sed -n "${plain_row}p" | grep -qF '●' || continue

        af_send '?'
        af_wait_for 'Managing:|Workspace:' "$AF_DRIVER_TIMEOUT" 'help overlay' || return 1
        # The control decides whether this width can discriminate: if the modal
        # covers the dot column, BOTH rows lose their dot and a pass below would
        # mean nothing.
        if af_capture | sed -n "${plain_row}p" | grep -qF '●'; then
            found="$width"
            break
        fi
        af_send Escape
        af_wait_gone 'Managing:' "$AF_DRIVER_TIMEOUT" 'help overlay closed' || return 1
    done

    if [ -z "$found" ]; then
        af_capture >&2
        _af_fail "no terminal width in the sweep leaves the ● dot as background beside the help \
overlay, so this check cannot discriminate. Re-derive the geometry (the dot must sit LEFT of the \
modal and within one cluster's width of it) rather than deleting the check — a vacuous pass here \
is how #723 stayed closed for three months."
        return 1
    fi

    if ! af_capture | sed -n "${zwj_row}p" | grep -qF '●'; then
        af_capture >&2
        _af_fail "the overlay erased background left of itself on the clustered row at ${found} \
columns: its ● dot is gone while the plain control row kept its own (#723)"
        return 1
    fi
    _af_log "=== overlay: PASS at ${found} columns (clustered row kept its background dot; plain row is the control) ==="

    af_send Escape
    af_wait_gone 'Managing:' "$AF_DRIVER_TIMEOUT" 'help overlay closed' || return 1
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
        if af_capture | grep -qE "▾[[:space:]]+${ANCHOR_HEAD}" && af_capture | grep -qF 'D kill'; then
            break
        fi
        af_send j
        sleep "$AF_DRIVER_POLL"
    done
    af_send D
    af_wait_for 'to cancel|esc cancel' "$AF_DRIVER_TIMEOUT" 'kill confirmation' || return 1

    local needle row line col
    needle='to cancel'
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
