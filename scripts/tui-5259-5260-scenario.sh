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
# /src is a linked worktree: its .git is a FILE pointing at the host's gitdir,
# unreachable inside the container, so git cannot answer rev-parse/archive
# here. The driver supplies the two SHAs and the master source as a tar:
#   AF_BRANCH_SHA  — `git rev-parse HEAD` of the worktree mounted at /src
#   AF_MASTER_SHA  — `git rev-parse origin/master` at run time
#   AF_MASTER_TAR  — container path of `git archive <master-sha>` output
# (falling back to in-container git only if a non-worktree /src ever has one).
git config --global --add safe.directory /src 2>/dev/null || true
BRANCH_SHA="${AF_BRANCH_SHA:-$(git -C /src rev-parse HEAD 2>/dev/null || true)}"
MASTER_SHA="${AF_MASTER_SHA:-$(git -C /src rev-parse origin/master 2>/dev/null || true)}"
[ -n "$BRANCH_SHA" ] || { _af_fail "branch SHA unknown — pass AF_BRANCH_SHA"; exit 1; }
[ -n "$MASTER_SHA" ] || { _af_fail "master SHA unknown — pass AF_MASTER_SHA"; exit 1; }
MASTER_SRC="$(mktemp -d /tmp/af-master.XXXXXX)"
if [ -n "${AF_MASTER_TAR:-}" ]; then
    tar -xf "$AF_MASTER_TAR" -C "$MASTER_SRC"
elif ! git -C /src archive "$MASTER_SHA" | tar -x -C "$MASTER_SRC"; then
    _af_fail "no master source: pass AF_MASTER_TAR (or give /src a reachable gitdir)"
    exit 1
fi
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

# tui_provenance <label> <bin> — prove WHICH binary the running TUI is (per
# approval: a leg that silently ran the wrong binary voids the control). The
# TUI is launched as `cd <repo> && <abs bin>` — a direct child of the drive
# pane's shell — so resolve that child's /proc/<pid>/exe and sha256 it against
# the binary this leg intended. Fails loud on any mismatch.
tui_provenance() {
    local label="$1" bin="$2" pane_pid tui_pid exe sha want comm
    pane_pid="$(tmux display-message -p -t "$AF_DRIVER_SESSION" '#{pane_pid}')"
    comm="$(basename "$bin")"
    tui_pid="$(pgrep -P "$pane_pid" -x "$comm" 2>/dev/null | head -1 || true)"
    if [ -z "$tui_pid" ]; then
        tui_pid="$(ps -eo pid=,ppid=,args= 2>/dev/null | awk -v pp="$pane_pid" -v b="$bin" '$2 == pp && $3 == b { print $1; exit }' || true)"
    fi
    [ -n "$tui_pid" ] || { _af_fail "$label: no TUI process found (child of pane shell $pane_pid named '$comm')"; return 1; }
    exe="$(readlink "/proc/$tui_pid/exe")"
    sha="$(sha256sum "$exe" | cut -d' ' -f1)"
    want="$(sha256sum "$bin" | cut -d' ' -f1)"
    {
        echo "$label tui pid:    $tui_pid"
        echo "$label tui exe:    $exe"
        echo "$label tui sha256: $sha"
        echo "$label bin sha256:  $want ($bin)"
    } | tee "$EVID/$label-tui-provenance.txt"
    [ "$exe" = "$bin" ] || { _af_fail "$label: running TUI exe '$exe' != intended '$bin'"; return 1; }
    [ "$sha" = "$want" ] || { _af_fail "$label: running TUI sha256 != $bin"; return 1; }
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

    # lostone -> Lost, DURABLY. lifecycle.sh documents that a bare kill-session
    # is NOT enough: the daemon's #1108 restore loop re-spawns a killed pane in
    # ~4s (it heals even a missing worktree), so the row flashes Lost and comes
    # back. To keep it resting the restore must fail instead: remove the
    # worktree AND delete its branch so Recover cannot rebuild the runtime.
    # Verified live: no af_*_lostone session ever reappears and the row keeps
    # its ▾ binding — exactly the resting row the fold test needs.
    local ts wt br
    ts="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E '_lostone$' | head -1 || true)"
    [ -n "$ts" ] || { _af_fail "could not find lostone's tmux session"; return 1; }
    wt="$(git -C "$AF_DRIVER_REPO" worktree list | awk '/lostone/ {print $1; exit}')"
    br="$(git -C "$AF_DRIVER_REPO" worktree list | awk '/lostone/ {print $NF; exit}' | tr -d '[]')"
    [ -n "$wt" ] && git -C "$AF_DRIVER_REPO" worktree remove --force "$wt" >/dev/null 2>&1
    [ -n "$br" ] && git -C "$AF_DRIVER_REPO" branch -D "$br" >/dev/null 2>&1
    tmux kill-session -t "=$ts"
    af_wait_for '\[lost\] +lostone' 30 'lostone marked lost'
    # Durability gate: the session must still be absent 8s later — well past
    # the ~4s self-heal lifecycle.sh measured — before any footer evidence is
    # collected from the row.
    sleep 8
    if tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -qE '_lostone$'; then
        _af_fail "lostone was auto-restored — row is not durably Lost"
        return 1
    fi
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
# Sets RESIZED_NS to the nanosecond timestamp the resize landed, so the first
# painted frame's capture time can be reported relative to it (the #5259
# window is ~100ms — the offsets are part of the evidence).
raw_resize() {
    tmux set-option -t "$AF_DRIVER_SESSION" window-size manual >/dev/null 2>&1 || true
    tmux resize-window -t "$AF_DRIVER_SESSION" -x "$1" -y "$2"
    RESIZED_NS="$(date +%s%N)"
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
    local want="$1" re="$2" i c hit_ns
    for i in $(seq 1 120); do
        c="$(af_capture)"
        if { [ "$want" = present ] && printf '%s' "$c" | grep -qE -- "$re"; } ||
           { [ "$want" = absent ] && ! printf '%s' "$c" | grep -qE -- "$re"; }; then
            hit_ns="$(date +%s%N)"
            FRAME_MS=$(( (hit_ns - RESIZED_NS) / 1000000 ))
            printf '%s' "$c" >"$EVID/$3.txt"
            printf 'first-frame capture: +%sms after resize-window\n' "$FRAME_MS" >"$EVID/$3-meta.txt"
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
# stale_shrink / stale_grow and appends a CSV row per direction to $FRAMES_CSV
# (first-frame capture offset in ms relative to the resize landing, per the
# approved plan's timing record):
#   stale_shrink=1 — first small frame still advertised 'r restore' although
#                    the bound row just scrolled off screen
#   stale_grow=1   — first restored frame still lacked 'r restore' although
#                    the bound row just scrolled back into view
fold_burst() {
    local label="$1" m ms
    wait_menu "$R_REST" 10 "$label: adoption footer before shrink" || return 1
    af_capture >"$EVID/$label-pre.txt"

    raw_resize 100 12
    first_painted_menu present "$FOLD_MARK" "$label-shrink" >"$EVID/$label-shrink-menu.txt" || return 1
    m="$(cat "$EVID/$label-shrink-menu.txt")"
    ms="$(sed -nE 's/.*\+([0-9]+)ms.*/\1/p' "$EVID/$label-shrink-meta.txt")"
    stale_shrink=0
    if printf '%s' "$m" | grep -qE -- "$R_REST"; then stale_shrink=1; fi
    printf '%s,shrink,+%sms,stale=%s\n' "$label" "${ms:-?}" "$stale_shrink" >>"$FRAMES_CSV"

    # Let the ~100ms tick correct the control leg before the next flip, so the
    # grow burst starts from the settled (correct) small-size menu.
    wait_menu_gone "$R_REST" 10 "$label: footer corrected after shrink" || return 1

    raw_resize 100 34
    first_painted_menu absent "$FOLD_MARK" "$label-grow" >"$EVID/$label-grow-menu.txt" || return 1
    m="$(cat "$EVID/$label-grow-menu.txt")"
    ms="$(sed -nE 's/.*\+([0-9]+)ms.*/\1/p' "$EVID/$label-grow-meta.txt")"
    stale_grow=0
    if ! printf '%s' "$m" | grep -qE -- "$R_REST"; then stale_grow=1; fi
    printf '%s,grow,+%sms,stale=%s\n' "$label" "${ms:-?}" "$stale_grow" >>"$FRAMES_CSV"
    wait_menu "$R_REST" 10 "$label: adoption footer restored after grow" || return 1
}

# --- legs --------------------------------------------------------------------

# purge_archived — af_reset_sandbox wipes instances/ + sibling worktrees and
# deletes every non-master branch, but archived worktrees live under
# $AGENT_FACTORY_HOME/archived/ — NOT in its wipe list — and git refuses
# `branch -D` on a branch still checked out there. A re-run then collides:
# "dev/<name> is already checked out at .../archived/...". Drop the archived
# dir, prune the registrations, and delete the now-freed branches. Same
# fail-closed sandbox-path guard af_reset_sandbox uses.
purge_archived() {
    case "$AGENT_FACTORY_HOME" in
        */sandbox/* | */sandbox | /tmp/* | */af-driver*) ;;
        *)
            _af_fail "purge_archived: refusing — '$AGENT_FACTORY_HOME' is not a sandbox path"
            return 1
            ;;
    esac
    rm -rf "$AGENT_FACTORY_HOME/archived"
    if [ -d "$AF_DRIVER_REPO/.git" ]; then
        git -C "$AF_DRIVER_REPO" worktree prune 2>/dev/null || true
        local b
        for b in $(git -C "$AF_DRIVER_REPO" for-each-ref \
            --format='%(refname:short)' refs/heads/ 2>/dev/null \
            | grep -vE '^(master|main)$' || true); do
            git -C "$AF_DRIVER_REPO" branch -D "$b" 2>/dev/null || true
        done
    fi
}

run_leg() { # <master|branch> <binary>
    local label="$1"
    export AF_DRIVER_BIN="$2"

    af_reset_sandbox
    purge_archived
    af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'
    af_boot
    # Prove the running TUI is THIS leg's binary before trusting any capture
    # (the control is void if it silently ran the branch build).
    tui_provenance "$label" "$2" || return 1
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
    FRAMES_CSV="$EVID/$label-frames.csv"
    printf 'burst,direction,first_frame_offset_ms,stale\n' >"$FRAMES_CSV"
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
