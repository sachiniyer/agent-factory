#!/usr/bin/env bash
# Real-TUI regression for the same-title swap path's #5298 ordering window:
# when an open pane sits on the viewed tab and the daemon kills+recreates the
# session under the same title with a reordered roster (fresh tab ids), the
# swap's pane reconcile closes/re-binds the pane and its relayout re-reads the
# sidebar BEFORE the reconcile's tail SelectInstance re-pin. On master the
# stale/clamped flat index is committed by pushSelection, clobbering the active
# tab remapActiveTabForTabs resolved and rewriting lastCursor* so the tail
# re-pin's replaced-session (by-name) tab lookup is defeated — the workspace
# drops onto the agent tab (slot 0) instead of the equivalent replacement tab.
#
# The fix pre-pins the cursor (SelectInstance) ahead of the relayout so the
# sidebar sync becomes a cursor move on intact lastCursor* evidence.
#
#   scripts/testbox.sh scenario scripts/tui-swap-pane-repin-scenario.sh
#
# One container, two legs (master control first, branch second). The control
# MUST be able to fail: the master leg's swap-with-open-pane is expected to land
# the active tab on the agent (slot 0) instead of the equivalent tab "a" — if it
# does not, the run reports CONTROL-INCONCLUSIVE.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# The control is the base of this branch (the #5298 fix commit, which still has
# the swap-path window open). Replaced by testbox.sh with --archive of this SHA.
AF_CONTROL_SHA=78d031456fa890b943a89524a8d3d0adff92883d

EVID="$HOME/sandbox/swap-pane-repin"
rm -rf "$EVID"
mkdir -p "$EVID"

# --- provenance --------------------------------------------------------------
git config --global --add safe.directory /src 2>/dev/null || true
BRANCH_SHA="${AF_BRANCH_SHA:-$(git -C /src rev-parse HEAD 2>/dev/null || true)}"
MASTER_SHA="${AF_MASTER_SHA:-$(git -C /src rev-parse origin/master 2>/dev/null || true)}"
[ -n "$BRANCH_SHA" ] || { _af_fail "branch SHA unknown — pass AF_BRANCH_SHA"; exit 1; }
[ -n "$MASTER_SHA" ] || { _af_fail "master SHA unknown — pass AF_MASTER_SHA"; exit 1; }
MASTER_SRC="$(mktemp -d /tmp/af-master.XXXXXX)"
trap 'rm -rf "$MASTER_SRC"' EXIT
if [ -n "${AF_MASTER_TAR:-}" ]; then
    tar -xf "$AF_MASTER_TAR" -C "$MASTER_SRC"
elif ! git -C /src archive "$MASTER_SHA" | tar -x -C "$MASTER_SRC"; then
    _af_fail "no master source: pass AF_MASTER_TAR (or give /src a reachable gitdir)"
    exit 1
fi
(cd "$MASTER_SRC" && go build -buildvcs=false -o "$HOME/bin/af.master" .)
(cd /src && go build -buildvcs=false -o "$HOME/bin/af" .)
{
    echo "branch HEAD (bin/af):      $BRANCH_SHA"
    echo "control (af.master):       $MASTER_SHA (pinned: $AF_CONTROL_SHA)"
    sha256sum "$HOME/bin/af" "$HOME/bin/af.master"
} | tee "$EVID/provenance.txt"

# --- helpers -----------------------------------------------------------------

tui_provenance() {
    local label="$1" bin="$2" pane_pid tui_pid exe sha want comm
    pane_pid="$(tmux display-message -p -t "$AF_DRIVER_SESSION" '#{pane_pid}')"
    comm="$(basename "$bin")"
    tui_pid="$(pgrep -P "$pane_pid" -x "$comm" 2>/dev/null | head -1 || true)"
    [ -n "$tui_pid" ] || { _af_fail "$label: no TUI process found"; return 1; }
    exe="$(readlink "/proc/$tui_pid/exe")"
    sha="$(sha256sum "$exe" | cut -d' ' -f1)"
    want="$(sha256sum "$bin" | cut -d' ' -f1)"
    [ "$exe" = "$bin" ] || { _af_fail "$label: running TUI exe '$exe' != intended '$bin'"; return 1; }
    [ "$sha" = "$want" ] || { _af_fail "$label: running TUI sha256 != $bin"; return 1; }
    _af_log "$label provenance ok ($comm pid $tui_pid)"
}

# active_tab_label — the tab name the footer/pane header shows as the active
# tab of the display-selected session. The pane header row reads
# " <title> · <tab-label> " where <tab-label> is "Agent" for the agent tab or
# "› <name>" for a shell/process tab. Strip the "· " prefix and the "› " tab
# glyph to recover the bare tab name.
active_tab_label() {
    af_capture | awk '/╭|┌|╔/ { if ((getline line) > 0) print line; exit }' \
        | grep -oE '·[[:space:]]+(›[[:space:]]+)?[^[:space:]]+' | head -1 \
        | sed -E 's/·[[:space:]]*//; s/›[[:space:]]*//'
}

# selected_name — the title on the ▾ display-selection marker row.
selected_name() {
    af_capture | grep -oE '▾[[:space:]]+[^[:space:]●○◌]+' | head -1 | sed 's/▾[[:space:]]*//'
}

run_leg() { # <leg> <binary>
    local leg="$1" bin="$2"
    export AF_DRIVER_BIN="$2"

    af_reset_sandbox
    af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'
    af_boot
    tui_provenance "$leg" "$2" || return 1

    # Three sessions so the replacement's fresh CreatedAt re-sorts and drifts
    # the cursor's flat index — the reported trigger. af_new_instance leaves the
    # newest selected, so re-select dup so its subtree is the expanded one the
    # tab rows render under.
    af_new_instance alpha
    af_new_instance dup
    af_new_instance omega

    # Build dup's corpse roster: [agent, a, b] via two named process tabs that
    # sleep so they survive the snapshot poll.
    (cd "$AF_DRIVER_REPO" && "$bin" sessions tab-create dup --command 'sleep 600' --name a) >/dev/null 2>&1
    (cd "$AF_DRIVER_REPO" && "$bin" sessions tab-create dup --command 'sleep 600' --name b) >/dev/null 2>&1
    sleep 2 # let the snapshot poll project the two new tabs

    # Expand dup so its tab rows render, and put the cursor on "a" (slot 1).
    af_select dup || return 1
    af_focus_tree || return 1
    # dup's subtree now renders: ├ 2 › a, └ 3 › b (slot 1 = "a"). Sync on the
    # "a" tab row before jumping, so the jump has exactly the expected roster.
    af_wait_for '› a' "$AF_DRIVER_TIMEOUT" "$leg: corpse tab a row visible" || return 1
    af_wait_for '› b' "$AF_DRIVER_TIMEOUT" "$leg: corpse tab b row visible" || return 1
    af_send 2                       # digit-jump to slot 1 ("a"); SyncCursorToActiveTab
    sleep "$AF_DRIVER_POLL"
    af_open_pane || return 1        # pane on "a"

    # Kill+recreate dup under the SAME title with a reordered roster
    # [agent, b, a] (a moved 1->2, fresh ids). The pane on "a" rebinds 1->2,
    # firing the swap's pane-reconcile relayout mid-swap.
    #
    # CRITICAL: the kill and create must land in the SAME 750ms snapshot poll so
    # reconcileSnapshot sees same-title/different-CreatedAt (a SWAP) rather than
    # a remove-then-add. So run them back-to-back with no wait between (any wait
    # lets a poll fire that removes the corpse first, surfacing as an add).
    (cd "$AF_DRIVER_REPO" && "$bin" sessions kill dup) >/dev/null 2>&1 \
        || { _af_fail "$leg: af sessions kill dup failed"; return 1; }
    (cd "$AF_DRIVER_REPO" && "$bin" sessions create dup) >/dev/null 2>&1 \
        || { _af_fail "$leg: af sessions create dup failed"; return 1; }
    (cd "$AF_DRIVER_REPO" && "$bin" sessions tab-create dup --command 'sleep 600' --name b) >/dev/null 2>&1
    (cd "$AF_DRIVER_REPO" && "$bin" sessions tab-create dup --command 'sleep 600' --name a) >/dev/null 2>&1
    sleep 3 # let the snapshot poll reconcile the swap + re-pin
    af_capture >"$EVID/$leg-after-swap.txt"

    local sel tab
    sel="$(selected_name)"
    tab="$(active_tab_label)"
    printf '%s\t%s\t%s\n' "swap-panerepin" "$sel" "$tab" >>"$EVID/$leg-results.tsv"
    _af_log "$leg/swap-panerepin -> selected='$sel' active_tab='$tab'"
    return 0
}

run_leg master "$HOME/bin/af.master"
run_leg branch "$HOME/bin/af"

# --- verdict -----------------------------------------------------------------
{
    echo "=== master results (case, selected, active tab) ==="
    cat "$EVID/master-results.tsv" 2>/dev/null || echo "(none)"
    echo "=== branch results ==="
    cat "$EVID/branch-results.tsv" 2>/dev/null || echo "(none)"
} | tee "$EVID/summary.txt"

for leg in master branch; do
    f="$EVID/$leg-after-swap.txt"
    [ -f "$f" ] || continue
    echo "=== raw frame: $leg / after-swap ==="
    cat "$f"
done

# The branch's contract: the ▾ selection stays on dup and the active tab
# follows the equivalent tab "a" by name to its new slot — not the agent tab.
awk -F'\t' '$1=="swap-panerepin" && ($2!="dup" || $3 !~ /^a$/){print "branch: expected dup/active-tab=a, got "$2"/"$3; bad=1} END{exit bad}' \
    "$EVID/branch-results.tsv" \
    || { cat "$EVID/branch-results.tsv" >&2; exit 1; }

# The control must reproduce the reported failure: master's active tab lands on
# the agent (or anything other than "a"), proving the swap-path window was open
# on the control. If it did not, report INCONCLUSIVE.
master_tab="$(awk -F'\t' '$1=="swap-panerepin"{print $3}' "$EVID/master-results.tsv" 2>/dev/null || true)"
if [ "$master_tab" = "a" ]; then
    echo "CONTROL-INCONCLUSIVE: master's swap-with-open-pane kept the active tab on 'a' — no drift reproduced; report on the swap-path fix"
    exit 3
fi

echo "PASS: master's swap-with-open-pane dropped the active tab onto '$master_tab'; branch follows the equivalent tab 'a' by name"
