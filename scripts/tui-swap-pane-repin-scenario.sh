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

# run_leg_once is invoked as `run_leg_once ... || return 1`. Bash (POSIX) IGNORES
# errexit inside any function called as part of a || list, so `set -e` gives it
# NO protection: an unchecked step that fails is silently swallowed and the leg
# can still record a result (#5730 — e.g. a failed final `tab-create --name a`
# left a non-`a` roster that the master leg accepted as a control reproduction).
# Every step therefore checks its own status explicitly, via leg_step/leg_af
# below or an inline `|| return 1`. Do NOT rely on set -e in this function.
#
# leg_step runs a command and, on failure, fails the leg with a message naming it.
leg_step() { # <description> <cmd...>
    local what="$1"
    shift
    "$@" || { _af_fail "${LEG:-leg}: $what failed"; return 1; }
}

# leg_af runs the leg's af binary from the mock repo, quietly.
leg_af() { # <af args...>
    (cd "$AF_DRIVER_REPO" && "$LEG_BIN" "$@") >/dev/null 2>&1
}

run_leg_once() { # <leg> <binary>
    local leg="$1" bin="$2"
    LEG="$leg" LEG_BIN="$bin"
    export AF_DRIVER_BIN="$2"

    leg_step "reset sandbox" af_reset_sandbox || return 1
    leg_step "write config" af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"' || return 1
    leg_step "boot TUI" af_boot || return 1
    tui_provenance "$leg" "$2" || return 1

    # Three sessions so the replacement's fresh CreatedAt re-sorts and drifts
    # the cursor's flat index — the reported trigger. af_new_instance leaves the
    # newest selected, so re-select dup so its subtree is the expanded one the
    # tab rows render under.
    leg_step "create session alpha" af_new_instance alpha || return 1
    leg_step "create session dup" af_new_instance dup || return 1
    leg_step "create session omega" af_new_instance omega || return 1

    # Build dup's corpse roster: [agent, a, b] via two named process tabs that
    # sleep so they survive the snapshot poll.
    leg_af sessions tab-create dup --command 'sleep 600' --name a \
        || { _af_fail "$leg: corpse tab-create a failed"; return 1; }
    leg_af sessions tab-create dup --command 'sleep 600' --name b \
        || { _af_fail "$leg: corpse tab-create b failed"; return 1; }
    sleep 2 # let the snapshot poll project the two new tabs

    # Expand dup so its tab rows render, and put the cursor on "a" (slot 1).
    af_select dup || return 1
    af_focus_tree || return 1
    # dup's subtree now renders: ├ 2 › a, └ 3 › b (slot 1 = "a"). Sync on the
    # "a" tab row before jumping, so the jump has exactly the expected roster.
    af_wait_for '› a' "$AF_DRIVER_TIMEOUT" "$leg: corpse tab a row visible" || return 1
    af_wait_for '› b' "$AF_DRIVER_TIMEOUT" "$leg: corpse tab b row visible" || return 1
    leg_step "digit-jump to slot 1" af_send 2 || return 1  # SyncCursorToActiveTab
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
    LOG_MARK="$(wc -l <"$AGENT_FACTORY_HOME/agent-factory.log" 2>/dev/null || echo 0)"
    leg_af sessions kill dup \
        || { _af_fail "$leg: af sessions kill dup failed"; return 1; }
    leg_af sessions create dup \
        || { _af_fail "$leg: af sessions create dup failed"; return 1; }
    leg_af sessions tab-create dup --command 'sleep 600' --name b \
        || { _af_fail "$leg: replacement tab-create b failed"; return 1; }
    leg_af sessions tab-create dup --command 'sleep 600' --name a \
        || { _af_fail "$leg: replacement tab-create a failed"; return 1; }
    sleep 3 # let the snapshot poll reconcile the swap + re-pin
    af_capture >"$EVID/$leg-after-swap.txt" \
        || { _af_fail "$leg: capturing the post-swap screen failed"; return 1; }

    local sel tab
    sel="$(selected_name)" || { _af_fail "$leg: reading the selected session failed"; return 1; }
    tab="$(active_tab_label)" || { _af_fail "$leg: reading the active tab failed"; return 1; }
    printf '%s\t%s\t%s\n' "swap-panerepin" "$sel" "$tab" >>"$EVID/$leg-results.tsv"
    _af_log "$leg/swap-panerepin -> selected='$sel' active_tab='$tab'"
    return 0
}

# run_leg retries run_leg_once, but ONLY to absorb the setup race behind #5620.
# The replacement's tabs are created by separate `af` invocations after the
# create, so a 750ms snapshot poll can land between them and swap in a PARTIAL
# roster ([agent, b], "a" not yet created). That correctly closes the pane on
# "a" and lands on "b", the sibling in the vacated slot (#5298) — a different
# timeline from the equivalent-tab-by-name path this scenario exercises (#5620:
# it passed on #5308's own head by timing luck). The partial-roster timeline
# itself is pinned by TestSwapSameTitle_PartialRosterThenTabArrives in app/.
#
# An attempt is discarded only on evidence that the race happened in it:
#  - branch leg: the TUI logs the roster it observed at swap time ("same-title
#    swap of "dup": replacement roster [...]"). A missing line (the corpse was
#    removed and re-added instead of swapped) or a roster other than the full
#    [agent,b,a] is the race. A FULL roster that lands anywhere but "a" is a
#    real re-pin defect and fails the scenario at once — no retry.
#  - control leg (pre-fix binary, no roster log): the bug lands on the agent tab;
#    the partial-roster race lands on "b". "Agent" is accepted, "b"/blank is
#    discarded as the race, and "a" (impossible from a partial roster) means the
#    control did not reproduce — CONTROL-INCONCLUSIVE at once.
# Every discarded attempt is printed with its reason; three discards fail.
run_leg() { # <leg> <binary>
    local leg="$1" bin="$2" attempt tab roster
    for attempt in 1 2 3; do
        rm -f "$EVID/$leg-results.tsv"
        run_leg_once "$leg" "$bin" || return 1
        tab="$(awk -F'\t' '{print $3}' "$EVID/$leg-results.tsv")"
        if [ "$leg" = master ]; then
            case "$tab" in
                Agent) return 0 ;;
                a) echo "CONTROL-INCONCLUSIVE: master landed on 'a' — no drift reproduced"; exit 3 ;;
            esac
            _af_log "master: attempt $attempt DISCARDED — landed on '${tab:-?}', the partial-roster signature, not the bug's agent landing; retrying"
            continue
        fi
        roster="$(tail -n +$((LOG_MARK + 1)) "$AGENT_FACTORY_HOME/agent-factory.log" 2>/dev/null \
            | grep -F 'same-title swap of "dup"' | tail -1 || true)"
        if [ -z "$roster" ]; then
            _af_log "branch: attempt $attempt DISCARDED — no same-title swap logged (corpse removed and re-added); retrying"
        elif ! printf '%s' "$roster" | grep -Eq 'replacement roster \[[^],]+,b,a\]$'; then
            _af_log "branch: attempt $attempt DISCARDED — swap observed a partial roster: ${roster#*replacement roster }; retrying"
        elif [ "$tab" = a ]; then
            return 0
        else
            _af_fail "branch: swap observed the full roster (${roster#*replacement roster }) but the active tab is '${tab:-?}', not 'a' — real re-pin defect, not the setup race"
            exit 1
        fi
    done
    _af_fail "$leg: all 3 attempts hit the partial-roster setup race; no verdict"
    exit 1
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
