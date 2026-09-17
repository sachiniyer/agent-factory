#!/usr/bin/env bash
# Run only via scripts/testbox.sh scenario or in its isolated playtest container.
# Real tmux driver for #4480: the pane's tmux window size is owned by the
# surface the user is driving — embedding/opening sibling panes/resizing the
# outer terminal while merely WATCHING must never resize the session's window
# (each used to write a RESIZE frame, and every different-size resize-window
# reflows scrollback — the mangling this issue reports).
#
# Observable contract asserted here, against the session's REAL tmux window
# size on the sandbox's private tmux server:
#   * fresh embedded pane is a viewer: pane stays at the 80x24 spawn size
#   * outer resizes + sibling-pane churn while viewing: pane stays 80x24
#   * entering interactive promotes the pane to owner: pane resizes to the box
#   * exiting interactive demotes WITHOUT resizing back (no compensating reflow)
#   * viewer box narrower than the pane renders a crop with a `…` edge marker
#   * full-screen attach owns: pane follows the attached client's size
#   * detach re-embeds as a viewer: pane keeps the attach size
#   * re-entering interactive re-asserts the current box (shrinks the pane)
set -euo pipefail
source /src/scripts/tui-driver.sh
export AF_DRIVER_COLS=140 AF_DRIVER_ROWS=40

SESS=""
sess_wxh() { tmux display-message -p -t "$SESS" '#{window_width}x#{window_height}' 2>/dev/null; }

# wait_size_change <old> [timeout] — poll until the session's window size is no
# longer <old>; prints the new size. For owner-driven resizes (a marker exists —
# the size itself), never a blind sleep.
wait_size_change() {
    local old="$1" deadline=$(( $(_af_now) + ${2:-15} )) cur
    while :; do
        cur="$(sess_wxh)"
        if [ -n "$cur" ] && [ "$cur" != "$old" ]; then printf '%s' "$cur"; return 0; fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "pane never left $old (still ${cur:-unknown}) — owner resize never landed"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# wait_size_is <want> [timeout] — poll until the window size equals <want>.
wait_size_is() {
    local want="$1" deadline=$(( $(_af_now) + ${2:-15} )) cur
    while :; do
        cur="$(sess_wxh)"
        [ "$cur" = "$want" ] && return 0
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "pane never reached $want (is ${cur:-unknown})"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# assert_size_is <want> <why> — negative-direction checks cannot poll a marker
# (nothing happens when the bug is absent), so this settles briefly, then pins.
assert_size_is() {
    local want="$1" why="$2" cur
    sleep 2
    cur="$(sess_wxh)"
    if [ "$cur" != "$want" ]; then
        af_capture >&2
        _af_fail "$why: expected pane $want, found ${cur:-unknown} — a passive surface resized the session"
        return 1
    fi
    printf '[tui-driver] pane size %s — %s\n' "$cur" "$why" >&2
}

af_boot
af_new_instance alpha
af_new_instance beta

# af's repo-scoped tmux names are af_<repo-hash>_<title> (SanitizedNameForRepo).
SESS="$(tmux list-sessions -F '#{session_name}' | grep -E '_alpha$' | head -1)"
[ -n "$SESS" ] || { _af_fail "no af_alpha tmux session"; exit 1; }
printf '[tui-driver] session under test: %s\n' "$SESS" >&2

# --- viewer: embed + sibling-pane churn + outer resizes never move the pane ---
af_select alpha
af_open_pane
assert_size_is "80x24" "embedded viewer must not resize the pane away from spawn size"

af_select beta
af_open_pane          # second pane opens beside alpha's → alpha's box shrinks
assert_size_is "80x24" "sibling pane opening must not resize the watched session"
af_hide_pane          # hides beta's focused pane; alpha's takes the slot + focus
assert_size_is "80x24" "sibling pane closing must not resize the watched session"

af_resize 100 30
assert_size_is "80x24" "outer shrink while watching must not resize the pane"
af_resize 160 44
assert_size_is "80x24" "outer grow while watching must not resize the pane"

# --- promotion: the focused interactive pane owns the size ---
af_enter_interactive
BOX="$(wait_size_change "80x24")"
printf '[tui-driver] interactive promotion asserted box %s\n' "$BOX" >&2

af_send_to_pane 'printf "Q%.0s" $(seq 1 200); echo WIDE_TAIL_4480'
af_wait_for 'WIDE_TAIL_4480'

# --- demotion: leaving interactive must NOT resize back to a viewer box ---
af_exit_interactive
assert_size_is "$BOX" "leaving interactive must not reflow the pane back"

# --- viewer narrow: box shrinks below pane width → crop, pane untouched ---
af_resize 90 24
assert_size_is "$BOX" "outer shrink after demotion must not resize the pane"
af_assert_screen 'Q{10,}…'  # clipped content carries the edge marker

# --- attach owns: pane follows the attached client's geometry ---
# af_attach's `o` binds the tree-SELECTED instance — re-select alpha first (the
# last af_select was beta; alpha's pane holding keyboard focus does not move
# the tree selection).
af_select alpha
af_attach
# The attach client's terminal is the drive PANE, which loses a row to tmux's
# status line — expect width 90, whatever the height lands at.
ATTACH_SZ="$(wait_size_change "$BOX")"
[ "${ATTACH_SZ%%x*}" = "90" ] || { _af_fail "attach should drive the pane to the client width 90, got $ATTACH_SZ"; exit 1; }
printf '[tui-driver] attach asserted %s\n' "$ATTACH_SZ" >&2
af_send_line 'printf "A%.0s" $(seq 1 150); echo ATTACH_TAIL_4480'

# --- detach: re-embed is a viewer again; the pane keeps the attach size ---
af_detach
assert_size_is "$ATTACH_SZ" "detach → embed must not resize the pane back"
af_assert_screen 'A{10,}…|Q{10,}…'   # crop marker still marks clipped content

# --- re-promotion asserts the CURRENT (now smaller) box ---
af_select alpha
af_open_pane
af_enter_interactive
BOX2="$(wait_size_change "$ATTACH_SZ")"
printf '[tui-driver] re-promotion asserted box %s\n' "$BOX2" >&2
af_send_to_pane 'echo FINAL_4480'
af_wait_for 'FINAL_4480'
af_exit_interactive

# --- custom spawn size: a never-driven viewer must render the pane's REAL ---
# --- size, learned on the capture path — not an assumed 80x24            ---
# tmux's default-size is user-configurable; with `default-size 200x60` the next
# session's window spawns at 200x60 and NO af surface ever writes a RESIZE for
# it. The viewer must learn that geometry from the capture path — a guessed
# emulator size would reflow the screen wrong for as long as it is watched
# (#4480 review).
tmux set-option -g default-size 200x60
af_new_instance gamma
SESS="$(tmux list-sessions -F '#{session_name}' | grep -E '_gamma$' | head -1)"
[ -n "$SESS" ] || { _af_fail "no af_gamma tmux session"; exit 1; }
af_select gamma
af_open_pane
assert_size_is "200x60" "embedded viewer must not resize a 200x60 spawn"

# Input at the tmux layer (send-keys is not a size writer): the pane's own
# program produces the content the viewer must render at 200x60. The marker
# strings are built by %s%s so the echoed command line itself never contains
# them — only a real cursor-positioned write can put them on screen.
tmux send-keys -t "$SESS" 'printf "\033[45;1H%s%s" RO W45_4480; printf "\033[50;1H%s%s" RO W50_4480; printf "G%.0s" $(seq 1 150); echo GAMMA_WIDE_4480' Enter
af_wait_for 'GAMMA_WIDE_4480'
# A 24-row emulator would clamp both CSI moves onto its last row, so the second
# write would overwrite the first — BOTH markers surviving inside the ~19-row
# crop (which ends at the cursor, ~row 52) proves the emulator is 60 rows tall.
# The 150-char run clipped with `…` at the ~62-col box edge proves it is 200
# cols wide — an 80-wide emulator would wrap it into rows that fit unmarked.
af_wait_for 'ROW45_4480'
af_wait_for 'ROW50_4480'
af_wait_for 'G{10,}…'
assert_size_is "200x60" "viewer-only session must keep its 200x60 spawn size"

af_assert_no_orphan_clients
printf 'PASS #4480 single-owner pane size: viewers never resize, owners assert, crops mark clips, spawn size measured\n'
