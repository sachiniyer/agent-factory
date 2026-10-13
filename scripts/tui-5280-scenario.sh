#!/usr/bin/env bash
# Real-TUI regression for #5280 — `R` on a tab kind that can never display a
# custom name (the Agent tab, and Terminal/shell tabs whose label always reads
# "Terminal") must answer the key with a visible refusal notice instead of
# looking dead.
#
#   scripts/testbox.sh scenario scripts/tui-5280-scenario.sh
#
# One container, two legs. The MASTER leg (built in-container from
# AF_MASTER_TAR, the same shape as tui-5259-5260-scenario.sh) drives R in each
# focus state and RECORDS the outcome — notice / prompt / silent — so the
# control can fail: wherever master was silent, the branch leg must show the
# notice. If master was silent in NO state, the run reports itself
# inconclusive (the issue then narrows to wording; say so on #5280).
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# The driver's real default geometry (100x30) — the #5280 re-scope attestation
# wants the clipping question answered at the width the harness actually
# renders, not an inflated one: master's per-tab refusal is ~118 chars and
# clips through errBox.statusLine at this width; the unified sentence fits.
export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# The control must stay a KNOWN-CLIPPING revision: once this fix merges,
# archiving the moving origin/master would build a fixed control that can
# never show the truncated per-tab refusal, and the master leg would fail
# forever after. `testbox.sh scenario` archives this pinned SHA in place of
# origin/master; a manual run passes the same SHA as AF_MASTER_SHA with a tar
# of it. bd7b89b7 is v1.0.311 — the master the issue was filed against.
AF_CONTROL_SHA=bd7b89b700d3343ef575c540789d5966f91f6079

EVID="$HOME/sandbox/rename-5280"
# Start empty: under `AF_SELFTEST_NAME` reuse the sandbox (and this dir) can
# survive from a previous run, and record_r APPENDS — stale verdict rows
# would otherwise feed the verdict awk below.
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
# Clean the extracted tree on ANY exit, not just the happy path — in a
# retained sandbox a failed tar/build (set -e exits before a trailing rm)
# would strand a full repo copy per attempt.
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
# ~/bin/af can be stale while AF_BRANCH_SHA already names the new HEAD —
# provenance would attribute a stale binary to the wrong commit. /src is
# read-only; go writes only GOCACHE and the -o target.
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

# record_r <leg> <state> — wait for any previous notice to clear (isolation,
# not an expiry assertion), press R, capture, and record one TSV row:
# <state>\t<notice|prompt|silent>\t<status-bar row>. A prompt is dismissed
# defensively so the drive can continue; every state here is non-renameable,
# so 'prompt' is already a verdict the branch must not produce.
record_r() {
    local leg="$1" state="$2" screen verdict bar
    af_wait_gone "can't be renamed" 8 "$leg/$state: previous notice cleared before R" || return 1
    af_send R
    sleep 1
    screen="$(af_capture)"
    printf '%s\n' "$screen" >"$EVID/$leg-$state-after-R.txt"
    if printf '%s\n' "$screen" | grep -q 'Rename tab'; then
        verdict=prompt
    elif printf '%s\n' "$screen" | grep -qE "can't be renamed|no longer exists"; then
        verdict=notice
    else
        verdict=silent
    fi
    # Record the notice line itself when there is one — the two builds answer
    # with DIFFERENT refusal sentences, so the text is part of the verdict
    # (a bare 'notice' column cannot tell them apart); fall back to the menu
    # row for prompt/silent states.
    bar="$(printf '%s\n' "$screen" | grep -oE ".{0,80}can't be renamed.{0,40}|.{0,60}no longer exists.{0,40}" | head -1 | tr '\t' ' ')"
    [ -n "$bar" ] || bar="$(printf '%s\n' "$screen" | _af_menu_row | tr '\t' ' ')"
    printf '%s\t%s\t%s\n' "$state" "$verdict" "${bar:0:140}" >>"$EVID/$leg-results.tsv"
    _af_log "$leg/$state -> $verdict"
    if [ "$verdict" = prompt ]; then af_send Escape; sleep "$AF_DRIVER_POLL"; fi
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

    af_new_instance rename
    af_select rename
    af_ensure_nav           # Ctrl-]: the host owns the keyboard (no interactive pane)
    sleep 4                 # let any create/select notice expire out of the bar

    # (a) tree focus, cursor on the SESSION row — af_select leaves the cursor
    # there with the instance's sticky active tab (the agent tab, idx 0).
    record_r "$leg" a-session-row || return 1

    # (b) tree focus, cursor on the AGENT tab row — the issue's verbatim repro.
    af_send j                    # session row -> first tab row (Agent)
    record_r "$leg" b-agent-row || return 1

    # (c) tree focus, cursor on the TERMINAL tab row.
    af_new_tab                   # t -> Terminal; selects the new row + opens its pane
    af_focus_tree || return 1    # tree focus; the cursor stays on the Terminal row
    record_r "$leg" c-terminal-row || return 1

    # (d) pane focus — the reporter's "cursor on its Agent tab row" under the
    # default layout: the agent pane focused (preview, not interactive).
    af_send k                    # Terminal row -> Agent row
    af_open_pane                 # s: agent pane opened + focused
    record_r "$leg" d-agent-pane || return 1
    af_hide_pane                 # x: hide the focused pane back to the tree
    af_send j                    # Agent row -> Terminal row
    af_open_pane                 # s: focus the terminal pane af_new_tab opened
    record_r "$leg" e-terminal-pane || return 1
    af_hide_pane

    # E details: the retained copy of the LAST notice raised this leg —
    # asserted wherever the leg produced at least one notice.
    if grep -q 'notice' "$EVID/$leg-results.tsv"; then
        af_send E
        af_wait_for 'Last notice' "$AF_DRIVER_TIMEOUT" "$leg: retained refusal via E details" || return 1
        af_capture >"$EVID/$leg-last-notice.txt"
        af_send Escape
    fi
    return 0
}

run_leg master "$HOME/bin/af.master"
run_leg branch "$HOME/bin/af"

# --- verdict -----------------------------------------------------------------
{
    echo "=== master results (state, verdict, status-bar row) ==="
    cat "$EVID/master-results.tsv"
    echo "=== branch results ==="
    cat "$EVID/branch-results.tsv"
} | tee "$EVID/summary.txt"

# Dump the raw post-R frames into the run log — they are the attestation's
# evidence (the container and its $EVID go away on teardown).
for leg in master branch; do
    for st in a-session-row b-agent-row c-terminal-row d-agent-pane e-terminal-pane; do
        f="$EVID/$leg-$st-after-R.txt"
        [ -f "$f" ] || continue
        echo "=== raw frame: $leg / $st (after R) ==="
        cat "$f"
    done
    [ -f "$EVID/$leg-last-notice.txt" ] && { echo "=== raw frame: $leg / E details ==="; cat "$EVID/$leg-last-notice.txt"; }
done

# The branch's observable contract holds regardless of what master did:
# every driven state must end in a notice, and that notice must be the one
# unified sentence — the exact text is what distinguishes this build from a
# control that already refused with older, divergent wordings.
awk -F'\t' '$2!="notice"{print "branch state "$1" did not answer with a refusal notice ("$2")"; bad=1} END{exit bad}' \
    "$EVID/branch-results.tsv" || { cat "$EVID/branch-results.tsv" >&2; exit 1; }
awk -F'\t' '$3 !~ /Agent and Terminal tabs can.t be renamed/{print "branch state "$1" did not show the unified refusal ("$3")"; bad=1} END{exit bad}' \
    "$EVID/branch-results.tsv" || { cat "$EVID/branch-results.tsv" >&2; exit 1; }

silents="$(awk -F'\t' '$2=="silent"{print $1}' "$EVID/master-results.tsv")"
if [ -z "$silents" ]; then
    echo "CONTROL-INCONCLUSIVE: master answered R in every driven state — no silent path reproduced; report on #5280"
    exit 3
fi

# The control observed real silence. The branch must answer every one of
# those states.
for st in $silents; do
    v="$(awk -F'\t' -v s="$st" '$1==s{print $2}' "$EVID/branch-results.tsv")"
    [ "$v" = notice ] || { _af_fail "branch still non-responsive in state $st (master was silent, branch: $v)"; exit 1; }
done

echo "PASS: master silent in [$silents]; branch answers R with the unified refusal notice in every driven state"
