#!/usr/bin/env bash
# Real-TUI symptom contrast for the resting-row footer pair, #5259 + #5260.
#
#   scripts/testbox.sh playtest -d        # start the sandbox, then:
#   docker exec <name> bash /src/scripts/tui-5259-5260-scenario.sh
#   # or in one step:
#   scripts/testbox.sh scenario scripts/tui-5259-5260-scenario.sh
#
# #5260: the compact resting footer omits 's open pane' on Lost/Dead rows even
# though the key works — a Lost row keeps a pane surface (ui/tab_pane.go
# renders its fallback content); only an Archived row loses its panes
# (pruneDeadPanes). Deterministic: on master the hint is absent at every width.
#
# #5259: relayout invalidates the sidebar's fitted window (SetSize drops
# hasRendered) but nothing re-resolves the footer's RowVerbTarget until the
# next selectionChanged — up to one preview tick (~100ms) away. A resize that
# moves the ▾-bound resting row across the sidebar fold therefore leaves the
# footer advertising that row's verbs (r/D) against a row that is no longer on
# screen — or keeps hiding them after it scrolls back in.
#
# Both legs run in THIS sandbox against binaries built inside it:
#   af.master — git archive of origin/master, the control that MUST show the
#               bugs (proves the scenario can detect them)
#   af        — the playtest entry's branch build, which must NOT
# Provenance (git SHAs + sha256sums) is recorded first so every capture can be
# tied to exact code.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=34

EVID="$HOME/sandbox/footer-5259-5260"
mkdir -p "$EVID"

# --- provenance --------------------------------------------------------------
git config --global --add safe.directory /src 2>/dev/null || true
BRANCH_SHA="$(git -C /src rev-parse HEAD)"
MASTER_SHA="$(git -C /src rev-parse origin/master)"
MASTER_SRC="$(mktemp -d /tmp/af-master.XXXXXX)"
git -C /src archive "$MASTER_SHA" | tar -x -C "$MASTER_SRC"
(cd "$MASTER_SRC" && go build -buildvcs=false -o "$HOME/bin/af.master" .)
{
    echo "branch HEAD (bin/af):      $BRANCH_SHA"
    echo "origin/master (af.master): $MASTER_SHA"
    sha256sum "$HOME/bin/af" "$HOME/bin/af.master"
} | tee "$EVID/provenance.txt"

# --- helpers -----------------------------------------------------------------

menu_row() { af_capture | _af_menu_row; }

# wait_menu <regex> [timeout] — poll until the status-bar MENU ROW matches.
wait_menu() {
    local re="$1" timeout="${2:-10}" row
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while :; do
        row="$(menu_row)"
        if printf '%s' "$row" | grep -qE -- "$re"; then return 0; fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "TIMEOUT ${timeout}s waiting for menu /$re/; last menu row:"
            _af_log "${row:-<none>}"
            af_capture >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# wait_menu_gone <regex> [timeout] — poll until a REAL menu row exists and does
# not match (an absent/notice bar does NOT count as gone — an empty row would
# pass a bare !grep instantly and prove nothing).
wait_menu_gone() {
    local re="$1" timeout="${2:-10}" row
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while :; do
        row="$(menu_row)"
        if [ -n "$row" ] && ! printf '%s' "$row" | grep -qE -- "$re"; then return 0; fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "menu still matches /$re/ (or menu row missing) after ${timeout}s: ${row:-<none>}"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

menu_has() { # <regex> <label> — assert the CURRENT menu row matches
    local row; row="$(menu_row)"
    if ! printf '%s' "$row" | grep -qE -- "$1"; then
        af_capture >"$EVID/$2-fail.txt"
        _af_fail "$2: expected menu to match /$1/, got: ${row:-<no menu row>}"
        return 1
    fi
}

menu_lacks() { # <regex> <label> — assert the CURRENT menu row does not match
    local row; row="$(menu_row)"
    if printf '%s' "$row" | grep -qE -- "$1"; then
        af_capture >"$EVID/$2-fail.txt"
        _af_fail "$2: menu unexpectedly matches /$1/: $row"
        return 1
    fi
}

S_OPEN='(^|[[:space:]])s open pane([[:space:]]|$)'
R_REST='(^|[[:space:]])r restore([[:space:]]|$)'
# The rail's hidden-rows-below indicator. Anchored at the sidebar's left edge
# (only whitespace may precede it — same rule _AF_RAIL_MARKER documents for ▲):
# pane text never satisfies it (it sits inside its border), and a section
# header's ▼/▶ arrow is followed by a WORD, not '<digit> more'.
FOLD_MARK='^[[:space:]]*▼ [0-9]+ more'

# select_row <name> — nav-scan the cursor onto <name>'s row, tolerant of the
# '[lost] '/▧ decorations af_select's regex can't see. The k-saturation can
# leave the cursor ON a section header (headers are not nav stops); a parked
# header with a bound resting row shows the SAME resting footer by adoption,
# which would be a false pass — so the scan starts with one mandatory j that
# steps off any header, and a row is only accepted once the cursor is truly on
# it: its line carries the ▾ sticky mark AND the menu is the resting footer.
# Two attempts: a transient notice can blank the menu row mid-scan and walk the
# cursor past the target — re-anchoring is cheap.
select_row() {
    local name="$1" i a screen
    af_ensure_nav
    af_focus_tree || return 1
    for _ in 1 2; do
        for ((a = 0; a < 30; a++)); do af_send k; done
        af_send j
        sleep "$AF_DRIVER_POLL"
        for i in $(seq 1 45); do
            screen="$(af_capture)"
            if printf '%s' "$screen" | grep -qE -- "▾.*${name}" && \
               printf '%s' "$screen" | _af_menu_row | grep -qE -- "$R_REST"; then
                return 0
            fi
            af_send j
            sleep "$AF_DRIVER_POLL"
        done
    done
    _af_log "select_row: never landed on '$name' (need its ▾ row + resting menu)"
    af_capture >&2
    return 1
}

# seed_rows — 8 cheap live workers, one archived row, one Lost resting row.
# Live rows sort oldest-first by CreatedAt, so lostone is created LAST and sits
# at the bottom of the Sessions list — the row a shrink pushes below the fold.
# archone is created FIRST so once archived it moves under the collapsed
# Archived folder without disturbing the Sessions list's indices.
seed_rows() { # <label>
    af_new_instance archone
    local i
    for i in 1 2 3 4 5 6 7 8; do af_new_instance "w$i"; done
    af_new_instance lostone

    # archone -> Archived folder via the out-of-band CLI (the TUI picks it up
    # on the next snapshot, same as a second surface archiving it).
    local bin; bin="$(_af_resolve_bin)"
    (cd "$AF_DRIVER_REPO" && "$bin" sessions archive archone >"$EVID/$1-archive.log" 2>&1)
    af_wait_for 'Archived' "$AF_DRIVER_TIMEOUT" 'Archived folder header appears'

    # lostone -> Lost: kill the backing tmux session with no kill intent on
    # record — the canonical #1108 outage class. The daemon observes the death
    # and marks the row lost: '[lost] lostone' title prefix + the right-edge ◌
    # glyph. A settled Lost row is resting (IsResting) with no in-flight op.
    local ts
    ts="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E '_lostone$' | head -1 || true)"
    [ -n "$ts" ] || { _af_fail "could not find lostone's tmux session"; return 1; }
    tmux kill-session -t "=$ts"
    af_wait_for '\[lost\] +lostone' 30 'lostone marked lost'
    af_capture >"$EVID/$1-seeded.txt"
}

# park_on_sessions_header — cursor onto the Sessions section header via the real
# click path (zones.TreeHeader -> ClickHeader). Each click toggles the section,
# so two clicks leave it expanded with the cursor parked on the header — the
# posture where RowVerbTarget adopts the ▾-bound resting row. The sticky store
# binding survives header selection, so 'lostone' keeps its ▾.
park_on_sessions_header() {
    local line
    line="$(af_capture | grep -nE 'Sessions \(' | head -1 | cut -d: -f1)"
    [ -n "$line" ] || { _af_fail "Sessions header not on screen"; return 1; }
    af_click 10 "$line"
    af_click 10 "$line"
    af_wait_for 'lostone' "$AF_DRIVER_TIMEOUT" 'section re-expanded'
}

# raw_resize <cols> <rows> — resize WITHOUT the af_resize settle: the burst
# capture that follows is the whole point, so no polling delay may interpose.
raw_resize() {
    tmux set-option -t "$AF_DRIVER_SESSION" window-size manual >/dev/null 2>&1 || true
    tmux resize-window -t "$AF_DRIVER_SESSION" -x "$1" -y "$2"
}

# first_painted_menu <want:present|absent> <regex> <evidence-prefix>
# Immediately after a raw resize, return (stdout) the MENU ROW of the FIRST
# capture provably painted at the new geometry:
#   present — first frame MATCHING <regex>: the shrink sentinel. Reflow (tmux's
#             own reflow of the old frame into the new grid) cannot invent a
#             '▼ N more' the all-visible frame never had.
#   absent  — first frame NOT matching <regex>: the grow sentinel. The small
#             frame always carries '▼ N more', so its absence marks the grown
#             repaint. (The folded row's title can't be the sentinel: the
#             workspace preview may render the bound row's name every frame.)
first_painted_menu() {
    local want="$1" re="$2" i c
    for i in $(seq 1 120); do
        c="$(af_capture)"
        if { [ "$want" = present ] && printf '%s' "$c" | grep -qE -- "$re"; } ||
           { [ "$want" = absent ] && ! printf '%s' "$c" | grep -qE -- "$re"; }; then
            printf '%s' "$c" >"$EVID/$3.txt"
            printf '%s' "$c" | _af_menu_row
            return 0
        fi
    done
    af_capture >"$EVID/$3-timeout.txt"
    _af_fail "first_painted_menu: sentinel $want /$re/ never settled (see $3-timeout.txt)"
    return 1
}

# fold_burst <label> — one shrink+grow flip while the cursor is parked on the
# Sessions header with 'lostone' as the ▾-bound resting row. Sets globals
# stale_shrink / stale_grow:
#   stale_shrink=1 — first small frame still advertised 'r restore' although
#                    the bound row just scrolled off screen
#   stale_grow=1   — first restored frame still lacked 'r restore' although the
#                    bound row just scrolled back into view
fold_burst() {
    local label="$1" m
    wait_menu "$R_REST" 10 "$label: adoption footer before shrink" || return 1
    af_capture >"$EVID/$label-pre.txt"

    raw_resize 100 12
    m="$(first_painted_menu present "$FOLD_MARK" "$label-shrink")" || return 1
    stale_shrink=0
    if printf '%s' "$m" | grep -qE -- "$R_REST"; then stale_shrink=1; fi

    # Let the ~100ms tick correct the control leg before the next flip, so the
    # grow burst starts from the settled (correct) small-size menu.
    wait_menu_gone "$R_REST" 10 "$label: footer corrected after shrink" || return 1

    raw_resize 100 34
    m="$(first_painted_menu absent "$FOLD_MARK" "$label-grow")" || return 1
    stale_grow=0
    if ! printf '%s' "$m" | grep -qE -- "$R_REST"; then stale_grow=1; fi
    wait_menu "$R_REST" 10 "$label: adoption footer restored after grow" || return 1
}

# --- legs --------------------------------------------------------------------

run_leg() { # <master|branch> <binary>
    local label="$1"
    export AF_DRIVER_BIN="$2"

    af_reset_sandbox
    af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'
    af_boot
    seed_rows "$label"
    sleep 4 # let any create/notice line expire out of the status bar

    # ---- #5260: the Lost row's own footer ----------------------------------
    select_row lostone
    af_capture >"$EVID/$label-lost-footer.txt"
    if [ "$label" = master ]; then
        # The bug: 's open pane' is never advertised...
        menu_lacks "$S_OPEN" "$label-5260-hint"
        # ...yet the key still works — press it and the pane opens anyway.
        af_send s
        af_wait_for 'hide pane' "$AF_DRIVER_TIMEOUT" 's opens the lost row pane despite no hint'
        af_capture >"$EVID/$label-5260-key-works.txt"
        af_hide_pane
        wait_menu "$R_REST" 10 "$label: resting footer after hiding pane" || return 1
    else
        menu_has "$S_OPEN" "$label-5260-hint"
        # 'r restore' stays the primary affordance (displayed before s).
        menu_has 'r restore.*s open pane' "$label-5260-order"
    fi

    # ---- archived negative control (BOTH legs): an archived row owns no pane
    select_row archone
    af_capture >"$EVID/$label-archived-footer.txt"
    menu_lacks "$S_OPEN" "$label-5260-archived"
    menu_has "$R_REST" "$label-5260-archived"

    # ---- #5259: fold-crossing footer staleness -----------------------------
    select_row lostone           # rebind the sticky selection to lostone
    park_on_sessions_header
    wait_menu "$R_REST" 10 "$label: header adopts bound lost row" || return 1

    local flip; SHRINK_STALES=0; GROW_STALES=0
    for flip in 1 2 3 4 5 6; do
        fold_burst "$label-fold$flip" || return 1
        SHRINK_STALES=$((SHRINK_STALES + stale_shrink))
        GROW_STALES=$((GROW_STALES + stale_grow))
    done
    {
        echo "$label: stale 'r restore' on first shrunken frame: $SHRINK_STALES/6"
        echo "$label: missing 'r restore' on first regrown frame: $GROW_STALES/6"
    } | tee "$EVID/$label-fold-verdict.txt"

    if [ "$label" = master ]; then
        if [ "$((SHRINK_STALES + GROW_STALES))" -eq 0 ]; then
            _af_fail "control leg: NO stale frame caught in 12 first-frames — the scenario failed to observe the #5259 window on master, so it proves nothing about the fix"
            return 1
        fi
    elif [ "$((SHRINK_STALES + GROW_STALES))" -ne 0 ]; then
        _af_fail "branch leg: $((SHRINK_STALES + GROW_STALES)) stale first-frames — footer still lags the fold (#5259 not fixed)"
        return 1
    fi
}

run_leg master "$HOME/bin/af.master"
run_leg branch "$HOME/bin/af"

printf 'PASS #5259 stale fold footer reproduced on master, absent on branch; #5260 s-hint absent on master, advertised on the Lost row on branch, still absent on Archived\n'
