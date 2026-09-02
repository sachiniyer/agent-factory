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
# The overflow oracle here is the wrap itself. If a rail row draws wider than the
# terminal, tmux wraps it onto the next screen row and everything below shifts
# down by one — so the session's ⎇ branch line stops being the row directly under
# its title. That is a structural consequence, not a width measurement, which is
# the point: the harness never has to agree with anyone about how wide the family
# is.
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

# _row_of <needle> — 1-based screen row containing <needle>.
_row_of() { af_capture | grep -nF -- "$1" | head -1 | cut -d: -f1; }

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
    title_row="$(_row_of "$ANCHOR_HEAD")"
    [ -n "$title_row" ] || { _af_fail "the clustered session's title row is not on screen"; return 1; }

    # Half one: the status dot survives the width accounting. Bounding the row
    # without laying the glyph out from the rectangle's edge just trades the
    # overflow for a missing status marker — the measured harm that made #3610
    # withdraw the overestimate as the general measure.
    af_capture | sed -n "${title_row}p" | grep -qF '●' || {
        _af_fail "the Ready session lost its ● status dot at the 22-column rail (#3614)"
        af_capture | sed -n "${title_row}p" >&2
        return 1
    }

    # Half two: the row did not wrap. The branch line is rendered as the row
    # directly under the title; if the title overflowed the terminal, tmux wrapped
    # it and pushed the branch line one row further down.
    branch_row="$(_row_of '⎇')"
    [ -n "$branch_row" ] || { _af_fail "the session's ⎇ branch line is not on screen"; return 1; }
    if [ "$branch_row" -ne "$((title_row + 1))" ]; then
        _af_fail "the clustered rail row wrapped: ⎇ is at row $branch_row, expected $((title_row + 1)) — \
the row drew past the terminal and every height budget below it is now off by one (#3614/#3430)"
        af_capture >&2
        return 1
    fi

    # And the frame still ends where it should: a wrap anywhere above would push
    # the status bar off the bottom.
    af_capture | tail -3 | grep -qE 'quit|help' || {
        _af_fail "the status bar is not on the last rows — something above it wrapped"
        af_capture >&2
        return 1
    }
    _af_log "=== rail: PASS ==="
}

# --- #723: a modal must not erase the background left of itself --------------
#
# The A/B from the issue, driven through the real compositor. The prefix left of
# an overlay was cut with a rune-wise truncator while its width was read with the
# grapheme-aware one, so on a row carrying a cluster the prefix came up short and
# the padding blanked real background cells — six of them, immediately left of the
# modal.
#
# The check needs no knowledge of the modal's geometry beyond its LEFT EDGE, and
# it derives that from the screen: on rows the modal covers, the first column at
# which the frame changed is the modal's origin, and the minimum of that over
# several rows is the origin itself. Everything left of it must be untouched.
drive_overlay_background() {
    _af_log "=== overlay: the modal must not erase background left of itself (#723) ==="
    af_resize 120 40 || return 1
    af_ensure_nav

    local before after
    before="$(af_capture)"
    af_send '?'
    af_wait_for 'Managing:|Workspace:' "$AF_DRIVER_TIMEOUT" 'help overlay' || return 1
    after="$(af_capture)"

    printf '%s' "$before" > /tmp/af-3614-before.txt
    printf '%s' "$after"  > /tmp/af-3614-after.txt
    if ! python3 - "$ANCHOR_HEAD" <<'PY'
import sys, unicodedata

anchor = sys.argv[1]
before = open('/tmp/af-3614-before.txt').read().split('\n')
after  = open('/tmp/af-3614-after.txt').read().split('\n')

def cells(s):
    out = 0
    for ch in s:
        if ch == '‍' or unicodedata.combining(ch):
            continue
        out += 2 if unicodedata.east_asian_width(ch) in ('W', 'F') else 1
    return out

def prefix_to(s, width):
    """The leading characters of s occupying at most `width` cells."""
    out, used = [], 0
    for ch in s:
        w = 0 if (ch == '‍' or unicodedata.combining(ch)) else (
            2 if unicodedata.east_asian_width(ch) in ('W', 'F') else 1)
        if used + w > width:
            break
        out.append(ch); used += w
    return ''.join(out)

def first_diff_col(a, b):
    """Cell column at which two rows first differ, or None if identical."""
    used = 0
    for i in range(max(len(a), len(b))):
        ca = a[i] if i < len(a) else ''
        cb = b[i] if i < len(b) else ''
        if ca != cb:
            return used
        ch = ca
        if ch and not (ch == '‍' or unicodedata.combining(ch)):
            used += 2 if unicodedata.east_asian_width(ch) in ('W', 'F') else 1
    return None

rows = min(len(before), len(after))
target = next((i for i in range(rows) if anchor in before[i]), None)
if target is None:
    print("FAIL: the clustered row is not on the pre-overlay screen"); raise SystemExit(1)
if before[target] == after[target]:
    print("FAIL: the modal does not cover the clustered row, so this proves nothing. "
          "Pick a taller overlay or a shorter terminal."); raise SystemExit(1)

# The modal's left edge: the smallest first-difference column over the rows it
# covers, ignoring the clustered row itself (which is the one under test).
edges = [c for i in range(rows) if i != target
         and (c := first_diff_col(before[i], after[i])) is not None]
if not edges:
    print("FAIL: no other row changed, so the modal's left edge cannot be located")
    raise SystemExit(1)
place_x = min(edges)
if place_x < 4:
    print(f"FAIL: derived a modal origin of {place_x}, which leaves nothing to check")
    raise SystemExit(1)

want = prefix_to(before[target], place_x)
got  = prefix_to(after[target],  place_x)
if want != got:
    print(f"FAIL: the overlay erased background left of column {place_x} on the clustered row (#723)")
    print(f"  before: {want!r}")
    print(f"  after:  {got!r}")
    raise SystemExit(1)
print(f"clustered row {target}: {place_x} cells left of the modal are intact "
      f"({cells(want)} measured)")
PY
    then
        _af_fail "overlay background check failed (#723)"
        return 1
    fi

    af_send Escape
    af_wait_gone 'Managing:' "$AF_DRIVER_TIMEOUT" 'help overlay closed' || return 1
    _af_log "=== overlay: PASS ==="
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

    local i
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
_new_zwj_instance "$ZWJ_TITLE"

# The clustered title is genuinely on screen — the precondition for every check
# below, since it is what makes the measures disagree.
af_capture | grep -qF '👨' || _af_fail "precondition: the clustered title must be rendered on screen"

drive_rail_row
drive_overlay_background
drive_selection_zone
drive_confirmation_zone

_af_log "#3614/#723 scenario: PASS"
