#!/usr/bin/env bash
# Real-TUI regression for #5298 — deleting a tab (w, then y) must keep the
# tree selection inside the SAME session, landing on the nearest remaining
# sibling tab: the tab that took the deleted tab's index, else the previous
# one. On master the pane reconcile's relayout re-reads the sidebar before the
# tree re-pin, the stale flat index lands on the FOLLOWING session's row, and
# pushSelection commits it — the selection jumps sessions.
#
#   scripts/testbox.sh scenario scripts/tui-5298-scenario.sh
#
# One container, two legs, master leg first. The control MUST be able to fail:
# the master leg's last-tab delete is expected to move the ▾ selection marker
# off the acting session entirely — if it does not, the run reports
# CONTROL-INCONCLUSIVE rather than attesting a fix that cannot be told apart.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# The control must stay a KNOWN-BUGGY revision: once this fix merges,
# archiving the moving origin/master would build a fixed control that can
# never reproduce the session jump, and the master leg would fail forever
# after. `testbox.sh scenario` archives this pinned SHA in place of
# origin/master; a manual run passes the same SHA as AF_MASTER_SHA with a tar
# of it. 7a14fadf is the master the branch is based on (v1.0.312+).
AF_CONTROL_SHA=7a14fadf0fc1d55c8d47ad023bfa67b619c1fc0a

EVID="$HOME/sandbox/tabdel-5298"
rm -rf "$EVID"
mkdir -p "$EVID"

# --- provenance --------------------------------------------------------------
# /src is a linked worktree: its .git is a FILE pointing at the host's gitdir,
# unreachable inside the container, so git cannot answer rev-parse/archive
# here. `testbox.sh scenario` sees this script consume them and supplies the
# two SHAs and the master source as a tar:
#   AF_BRANCH_SHA  — `git rev-parse HEAD` of the worktree mounted at /src
#   AF_MASTER_SHA  — the control revision: the script's pinned AF_CONTROL_SHA
#                    when declared, else `git rev-parse origin/master`
#   AF_MASTER_TAR  — container path of `git archive <control-sha>` output
git config --global --add safe.directory /src 2>/dev/null || true
BRANCH_SHA="${AF_BRANCH_SHA:-$(git -C /src rev-parse HEAD 2>/dev/null || true)}"
MASTER_SHA="${AF_MASTER_SHA:-$(git -C /src rev-parse origin/master 2>/dev/null || true)}"
[ -n "$BRANCH_SHA" ] || { _af_fail "branch SHA unknown — pass AF_BRANCH_SHA"; exit 1; }
[ -n "$MASTER_SHA" ] || { _af_fail "master SHA unknown — pass AF_MASTER_SHA"; exit 1; }
if [ "$MASTER_SHA" != "$AF_CONTROL_SHA" ]; then
    _af_log "note: control SHA is $MASTER_SHA, not the pinned $AF_CONTROL_SHA — running anyway"
fi
MASTER_SRC="$(mktemp -d /tmp/af-master.XXXXXX)"
trap 'rm -rf "$MASTER_SRC"' EXIT
if [ -n "${AF_MASTER_TAR:-}" ]; then
    tar -xf "$AF_MASTER_TAR" -C "$MASTER_SRC"
elif ! git -C /src archive "$MASTER_SHA" | tar -x -C "$MASTER_SRC"; then
    _af_fail "no master source: pass AF_MASTER_TAR (or give /src a reachable gitdir)"
    exit 1
fi
(cd "$MASTER_SRC" && go build -buildvcs=false -o "$HOME/bin/af.master" .)
# Rebuild the branch binary from /src unconditionally: under AF_SELFTEST_NAME
# reuse the running container predates this checkout, so its boot-time
# ~/bin/af can be stale while AF_BRANCH_SHA already names the new HEAD.
(cd /src && go build -buildvcs=false -o "$HOME/bin/af" .)
{
    echo "branch HEAD (bin/af):      $BRANCH_SHA"
    echo "control (af.master):       $MASTER_SHA (pinned: $AF_CONTROL_SHA)"
    sha256sum "$HOME/bin/af" "$HOME/bin/af.master"
} | tee "$EVID/provenance.txt"

# --- helpers -----------------------------------------------------------------

# tui_provenance <label> <bin> — prove WHICH binary the running TUI is (a leg
# that silently ran the wrong binary voids the control). The TUI is launched
# as `cd <repo> && <abs bin>` — a direct child of the drive pane's shell — so
# resolve that child's /proc/<pid>/exe and sha256 it against the intended bin.
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

# af_capture_styled — af_capture with SGR escapes kept. The tree's cursor row
# is marked by STYLE only (the selection background paints it), which plain
# af_capture drops, so cursor position can only be proven from the styled frame.
af_capture_styled() { tmux capture-pane -p -e -t "$AF_DRIVER_SESSION" 2>/dev/null; }

# selected_name — the title on the row carrying the ▾ display-selection
# marker. Exactly one live instance auto-expands, so the first ▾ row names the
# store's selected session.
selected_name() {
    af_capture | grep -oE '▾[[:space:]]+[^[:space:]●○◌]+' | head -1 | sed 's/▾[[:space:]]*//'
}

# cursor_row — the tree row the cursor rests on, read from the styled frame.
# The cursor's selection background ('48;' SGR) is emitted BEFORE the row's
# tree marker on the cursor's line only — the same rule verify_row_cursor
# applied on #5280 — so the first line whose first '48;' precedes its first
# tree glyph (▾/▸/├/└) is the cursor row. Escapes are stripped for the record.
cursor_row() {
    af_capture_styled | awk '
        {
            line = $0
            m = match(line, /▾|▸|├|└/)   # first tree glyph
            e = index(line, "48;")       # first background SGR
            if (m > 0 && e > 0 && e < m) {
                gsub(/\033\[[0-9;]*m/, "", line)
                sub(/^[[:space:]]+/, "", line)
                sub(/[[:space:]]+$/, "", line)
                print line
                exit
            }
        }'
}

# record_delete <leg> <case> — save the pre-delete frame, press w, accept the
# consent dialog with y, let the post-delete frame settle, and record one TSV
# row: <case>\t<selected-instance>\t<cursor-row>. The raw frames (plain +
# styled, before + after) are saved as evidence either way.
record_delete() {
    local leg="$1" case="$2" sel cur
    af_capture >"$EVID/$leg-$case-before-w.txt"
    af_send w || return 1
    _af_confirm_tab_delete || return 1
    af_wait_gone 'Delete tab' "$AF_DRIVER_TIMEOUT" "$leg/$case: consent dialog closed" || return 1
    sleep 1 # let the reconcile + re-pin frame paint
    af_capture >"$EVID/$leg-$case-after-wy.txt"
    af_capture_styled >"$EVID/$leg-$case-after-wy-styled.txt"
    sel="$(selected_name)"
    cur="$(cursor_row)"
    printf '%s\t%s\t%s\n' "$case" "$sel" "$cur" >>"$EVID/$leg-results.tsv"
    _af_log "$leg/$case -> selected='$sel' cursor='$cur'"
    return 0
}

run_leg() { # <leg> <binary>
    local leg="$1"
    export AF_DRIVER_BIN="$2"

    af_reset_sandbox
    af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'
    af_boot
    # Prove the running TUI is THIS leg's binary before trusting any capture.
    tui_provenance "$leg" "$2" || return 1

    af_new_instance alpha
    af_select alpha
    af_ensure_nav

    # Two terminal tabs — the same `t` gesture the issue's repro uses. Each
    # leaves its tab selected with its pane open and focused, which is the
    # state that makes the delete relayout (and on master, drift).
    af_new_tab                   # alpha: [agent, Terminal]  — pane opened+focused
    af_new_tab                   # alpha: [agent, Terminal, Terminal]

    # The neighbor the stale flat index lands on: a second session whose row
    # sits immediately below alpha's last tab row.
    af_new_instance beta

    # CASE A — middle tab: cursor on alpha's FIRST terminal (slot 1 of
    # [agent, T, T2]); after the delete the remaining terminal slides into
    # that slot, so the cursor belongs on the `2 Terminal` row INSIDE alpha.
    af_select alpha              # cursor parks on alpha's Agent tab row
    af_send j                    # -> alpha's first Terminal row (slot 1)
    sleep "$AF_DRIVER_POLL"
    record_delete "$leg" a-middle || return 1

    # CASE B — last tab: alpha is now [agent, Terminal]; select that last tab
    # and delete it — the issue's verbatim repro. The cursor belongs on the
    # previous sibling: alpha's `1 Agent` row. On master the stale flat index
    # lands on beta's instance row and the selection jumps sessions.
    af_select alpha              # back onto alpha's Agent row
    af_send j                    # -> alpha's last remaining Terminal row
    sleep "$AF_DRIVER_POLL"
    record_delete "$leg" b-last || return 1
    return 0
}

run_leg master "$HOME/bin/af.master"
run_leg branch "$HOME/bin/af"

# --- verdict -----------------------------------------------------------------
{
    echo "=== master results (case, selected, cursor row) ==="
    cat "$EVID/master-results.tsv"
    echo "=== branch results ==="
    cat "$EVID/branch-results.tsv"
} | tee "$EVID/summary.txt"

for leg in master branch; do
    for st in a-middle b-last; do
        for f in "$EVID/$leg-$st-before-w.txt" "$EVID/$leg-$st-after-wy.txt"; do
            [ -f "$f" ] || continue
            echo "=== raw frame: $leg / $st / $(basename "$f") ==="
            cat "$f"
        done
    done
done

# The branch's contract — both deletes keep the ▾ selection on alpha, and the
# cursor lands on the same-session sibling: the tab that slid into the deleted
# slot for the middle case, the previous sibling (Agent) for the last tab.
awk -F'\t' '$1=="a-middle" && ($2!="alpha" || $3 !~ /^(├|└)[[:space:]]+2[[:space:]]+Terminal/){print "branch a-middle: expected alpha/`2 Terminal`, got "$2"/`"$3"`"; bad=1}
           $1=="b-last"   && ($2!="alpha" || $3 !~ /^(├|└)[[:space:]]+1[[:space:]]+Agent/){print "branch b-last: expected alpha/`1 Agent`, got "$2"/`"$3"`"; bad=1}
           END{exit bad}' "$EVID/branch-results.tsv" || { cat "$EVID/branch-results.tsv" >&2; exit 1; }

# The control must actually reproduce the reported failure: master's last-tab
# delete must move the selection OFF alpha. If it did not, this run cannot
# distinguish the builds — report inconclusive rather than attesting.
master_sel="$(awk -F'\t' '$1=="b-last"{print $2}' "$EVID/master-results.tsv")"
if [ "$master_sel" = "alpha" ]; then
    echo "CONTROL-INCONCLUSIVE: master's last-tab delete kept the selection on alpha — no session jump reproduced; report on #5298"
    exit 3
fi

echo "PASS: master's last-tab delete jumped the selection to '$master_sel'; branch keeps every delete on alpha's sibling tab"
