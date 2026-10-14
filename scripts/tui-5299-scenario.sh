#!/usr/bin/env bash
# Real-TUI regression for #5299 — in the `/` session search, the query "bet"
# must rank beta (a clean prefix match) above bright-newt (a scattered
# b…e…t subsequence). Enter selects the top row, so the wrong order lands the
# user on the wrong session.
#
#   scripts/testbox.sh scenario scripts/tui-5299-scenario.sh
#
# or, on a parked sandbox (`scripts/testbox.sh playtest -d`), inject the
# provenance inputs yourself — the container cannot resolve them from the
# linked-worktree /src:
#
#   git archive --format=tar 8e1e04b27b53f77a7a21f44eae56bd739773cf74 >/tmp/m.tar
#   chmod 644 /tmp/m.tar
#   docker cp /tmp/m.tar <name>:/tmp/af-master-src.tar
#   docker exec -e AF_BRANCH_SHA="$(git rev-parse HEAD)" \
#               -e AF_MASTER_SHA=8e1e04b27b53f77a7a21f44eae56bd739773cf74 \
#               -e AF_MASTER_TAR=/tmp/af-master-src.tar \
#               <name> bash /src/scripts/tui-5299-scenario.sh
#
# The bug: updateResults kept matches in sidebar (oldest-first) order, so any
# scattered subsequence sitting earlier in the rail outranked a clean prefix.
#
# Both legs run in THIS sandbox at exactly 80x24, against binaries built inside
# it, with the same three sessions created in the issue's order — bright-newt,
# alpha, beta — so the pre-query sidebar order matches the repro:
#   af.master — git archive of the pinned pre-fix control revision, which MUST
#               list bright-newt above beta (proves the scenario detects it)
#   af        — the playtest entry's branch build, which must list beta first
#               and land on beta when Enter is pressed.
# Per leg the scenario opens `/`, types `bet`, captures the 80x24 frame, and
# compares the row numbers of the `name (dev/name)` result lines — the
# ` (dev/…)` suffix appears only on overlay rows, never on the rail behind.
# Provenance (git SHAs + binary sha256 + the running TUI's /proc/<pid>/exe
# sha256) is recorded so every capture can be tied to exact code.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
export AF_DRIVER_COLS=80 AF_DRIVER_ROWS=24

# The control must stay a KNOWN-BUGGY revision: once this fix merges, archiving
# the moving origin/master would build a fixed control that can never show
# bright-newt on top, and the master leg would fail forever after.
# `testbox.sh scenario` archives this pinned SHA in place of origin/master; a
# manual run passes the same SHA as AF_MASTER_SHA with a tar of it.
# 8e1e04b2 is the master the fix branches from (post-v1.0.312, which the issue
# was filed against).
AF_CONTROL_SHA=8e1e04b27b53f77a7a21f44eae56bd739773cf74

EVID="$HOME/sandbox/search-rank-5299"
# Fresh evidence per run: a retained sandbox's rerun must not mix this run's
# provenance with a prior run's captures.
case "$EVID" in */sandbox/*) rm -rf "$EVID" ;; esac
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
# (falling back to in-container git only if a non-worktree /src ever has one).
git config --global --add safe.directory /src 2>/dev/null || true
BRANCH_SHA="${AF_BRANCH_SHA:-$(git -C /src rev-parse HEAD 2>/dev/null || true)}"
MASTER_SHA="${AF_MASTER_SHA:-$(git -C /src rev-parse origin/master 2>/dev/null || true)}"
[ -n "$BRANCH_SHA" ] || { _af_fail "branch SHA unknown — pass AF_BRANCH_SHA"; exit 1; }
[ -n "$MASTER_SHA" ] || { _af_fail "master SHA unknown — pass AF_MASTER_SHA"; exit 1; }
if [ "$MASTER_SHA" != "$AF_CONTROL_SHA" ]; then
    _af_log "note: control SHA is $MASTER_SHA, not the pinned $AF_CONTROL_SHA — running anyway"
fi
# Rebuild the branch binary from the mounted tree before it is measured:
# under AF_SELFTEST_NAME sandbox reuse, ~/bin/af can still be the boot build
# of an older HEAD while provenance.txt names the current one. In-place with
# -buildvcs=false, exactly as the entry script does.
(cd /src && go build -buildvcs=false -o "$HOME/bin/af" .)
MASTER_SRC="$(mktemp -d /tmp/af-master.XXXXXX)"
# Clean the extracted tree on ANY exit, not just the happy path — in a retained
# sandbox a failed tar/build (set -e exits before a trailing rm) would strand a
# full repo copy per attempt.
trap 'rm -rf "$MASTER_SRC"' EXIT
if [ -n "${AF_MASTER_TAR:-}" ]; then
    tar -xf "$AF_MASTER_TAR" -C "$MASTER_SRC"
elif ! git -C /src archive "$MASTER_SHA" | tar -x -C "$MASTER_SRC"; then
    _af_fail "no master source: pass AF_MASTER_TAR (or give /src a reachable gitdir)"
    exit 1
fi
(cd "$MASTER_SRC" && go build -buildvcs=false -o "$HOME/bin/af.master" .)
{
    echo "branch HEAD (bin/af):      $BRANCH_SHA"
    echo "control (af.master):       $MASTER_SHA (pinned: $AF_CONTROL_SHA)"
    sha256sum "$HOME/bin/af" "$HOME/bin/af.master"
} | tee "$EVID/provenance.txt"

# --- helpers -----------------------------------------------------------------

# tui_provenance <label> <bin> — prove WHICH binary the running TUI is (a leg
# that silently ran the wrong binary voids the control). The TUI is launched as
# `cd <repo> && <abs bin>` — a direct child of the drive pane's shell — so
# resolve that child's /proc/<pid>/exe and sha256 it against the binary this
# leg intended. Fails loud on any mismatch.
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

# search_rows <capture-file> — locate the two result rows the overlay drew.
# Overlay rows render `● ▸ title (dev/branch)`; the rail behind never prints
# the ` (dev/…)` suffix, so matching on it isolates the overlay. Prints the
# row numbers; exit 2 when either row is missing (the capture is unusable).
search_rows() {
    python3 - "$1" <<'PY'
import sys
lines = open(sys.argv[1], encoding='utf-8').read().split('\n')
bn = next((i for i, l in enumerate(lines) if 'bright-newt (dev/' in l), -1)
bt = next((i for i, l in enumerate(lines) if 'beta (dev/' in l), -1)
print(f"bright-newt row: {bn}\nbeta row: {bt}")
if 'alpha (dev/' in '\n'.join(lines):
    print("note: alpha matched 'bet' — the match SET changed")
sys.exit(0 if bn >= 0 and bt >= 0 else 2)
PY
}

row_of() { # <search_rows output file> <name>
    sed -nE "s/^$2 row: (-?[0-9]+).*/\\1/p" "$1"
}

# --- legs --------------------------------------------------------------------

run_leg() { # <master|branch> <binary>
    local label="$1"
    LEG="$label"
    export AF_DRIVER_BIN="$2"

    af_reset_sandbox
    purge_archived
    # branch_prefix pins the dev/<name> branches the issue's listing shows.
    af_set_config 'default_program = "claude"
branch_prefix = "dev/"

[program_overrides]
claude = "bash"'
    af_boot
    # Prove the running TUI is THIS leg's binary before trusting any capture
    # (the control is void if it silently ran the branch build).
    tui_provenance "$label" "$2" || return 1

    # Creation order IS the sidebar order (oldest-first by CreatedAt), so
    # bright-newt sits above beta in s.all before any query — the repro stack.
    af_new_instance bright-newt || return 1
    af_new_instance alpha || return 1
    af_new_instance beta || return 1

    af_ensure_nav
    af_focus_tree || return 1
    af_send '/'
    af_wait_for 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search overlay' || return 1
    af_send_literal "bet"
    af_wait_for '/ bet' "$AF_DRIVER_TIMEOUT" 'query landed' || return 1
    sleep 1 # settle past the capture tick so the frame is the steady state
    af_capture >"$EVID/$LEG-search.txt"
    echo "--- raw 80x24 frame: $LEG/search ---" # the sandbox is ephemeral; the
    cat "$EVID/$LEG-search.txt"                # run log is the durable copy
    echo "--- end frame ---"
    search_rows "$EVID/$LEG-search.txt" | tee "$EVID/$LEG-rows.txt"

    if [ "$LEG" = branch ]; then
        # The user habit from the issue: /, type, Enter — must land on beta.
        af_send Enter
        sleep 1
        af_capture >"$EVID/$LEG-landed.txt"
        echo "--- raw 80x24 frame: $LEG/landed ---"
        cat "$EVID/$LEG-landed.txt"
        echo "--- end frame ---"
        grep -qE '▾[[:space:]]+beta([[:space:]]|$)' "$EVID/$LEG-landed.txt" \
            || { _af_fail "$LEG: Enter did not land the selection on beta"; return 1; }
        ! grep -qE '▾[[:space:]]+bright-newt([[:space:]]|$)' "$EVID/$LEG-landed.txt" \
            || { _af_fail "$LEG: Enter landed the selection on bright-newt"; return 1; }
    fi

    af_quit || return 1
}

run_leg master "$HOME/bin/af.master"
run_leg branch "$HOME/bin/af"

master_bn="$(row_of "$EVID/master-rows.txt" bright-newt)"
master_bt="$(row_of "$EVID/master-rows.txt" beta)"
branch_bn="$(row_of "$EVID/branch-rows.txt" bright-newt)"
branch_bt="$(row_of "$EVID/branch-rows.txt" beta)"

{
    echo "master leg:  bright-newt row $master_bn, beta row $master_bt"
    echo "branch leg:  bright-newt row $branch_bn, beta row $branch_bt"
} | tee "$EVID/verdict.txt"

# The control MUST reproduce the defect: bright-newt above beta. If it does
# not, this run proves nothing about the fix.
if [ "$master_bn" -ge "$master_bt" ]; then
    _af_fail "control leg: master must rank bright-newt (row $master_bn) above beta (row $master_bt) — the bug did not reproduce"
    exit 1
fi
if [ "$branch_bt" -ge "$branch_bn" ]; then
    _af_fail "branch leg: beta (row $branch_bt) must rank above bright-newt (row $branch_bn) — #5299 not fixed"
    af_capture >&2 || true
    exit 1
fi

printf 'PASS #5299: master control ranks bright-newt (row %s) above beta (row %s); branch ranks beta (row %s) first and Enter lands on it\n' "$master_bn" "$master_bt" "$branch_bt"
