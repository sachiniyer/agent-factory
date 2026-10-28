#!/usr/bin/env bash
# Real-TUI regression for #5279 — adjacent panes must not draw a '│││' seam.
#
#   scripts/testbox.sh scenario scripts/tui-5279-scenario.sh
#
# or, on a parked sandbox (`scripts/testbox.sh playtest -d`), inject the
# provenance inputs yourself — the container cannot resolve them from the
# linked-worktree /src:
#
#   git archive --format=tar bd7b89b700d3343ef575c540789d5966f91f6079 >/tmp/m.tar
#   chmod 644 /tmp/m.tar
#   docker cp /tmp/m.tar <name>:/tmp/af-master-src.tar
#   docker exec -e AF_BRANCH_SHA="$(git rev-parse HEAD)" \
#               -e AF_MASTER_SHA=bd7b89b700d3343ef575c540789d5966f91f6079 \
#               -e AF_MASTER_TAR=/tmp/af-master-src.tar \
#               <name> bash /src/scripts/tui-5279-scenario.sh
#
# The bug: each pane frames itself AND the layout's 1-col divider was drawn as
# '│', so every pane boundary rendered three adjacent vertical bars ('│││',
# corners '┐│┌'/'┘│└'). The fix renders the divider column as a blank gap, so a
# seam reads '│ │' with the column count unchanged.
#
# Both legs run in THIS sandbox against binaries built inside it:
#   af.master — git archive of the pinned pre-fix control revision, which MUST
#               show the bug (proves the scenario can detect it)
#   af        — the playtest entry's branch build, which must NOT
# Provenance (git SHAs + sha256sums) is recorded first so every capture can be
# tied to exact code.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
export AF_DRIVER_COLS=200 AF_DRIVER_ROWS=50

# The control must stay a KNOWN-BUGGY revision: once this fix merges, archiving
# the moving origin/master would build a fixed control that can never show the
# seam, and the master leg would fail forever after. `testbox.sh scenario`
# archives this pinned SHA in place of origin/master; a manual run passes the
# same SHA as AF_MASTER_SHA with a tar of it. bd7b89b7 is v1.0.311 — the master
# the issue was filed against.
AF_CONTROL_SHA=bd7b89b700d3343ef575c540789d5966f91f6079

EVID="$HOME/sandbox/pane-seam-5279"
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

# count_glyph <file> <glyph> — occurrences of a literal multi-byte glyph in a
# capture. grep -o exits 1 on zero matches; '|| true' keeps a legitimate zero
# (the fixed branch) from tripping the script's set -e + pipefail.
count_glyph() { { grep -oF "$2" "$1" || true; } | wc -l; }

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

# --- legs --------------------------------------------------------------------

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

    # Three cheap stand-in sessions tiled as three panes → two seams.
    local name
    for name in one two three; do
        af_new_instance "$name"
    done
    for name in one two three; do
        af_select "$name"
        af_open_pane
    done
    sleep 1 # settle past the capture tick so the frame is the steady state
    af_capture >"$EVID/$label-3panes.txt"

    local seams
    seams="$(count_glyph "$EVID/$label-3panes.txt" '│││')"
    {
        echo "$label: '│││' seams in capture: $seams"
        echo "$label: '┐│┌' corner seams:     $(count_glyph "$EVID/$label-3panes.txt" '┐│┌')"
        echo "$label: '│ │' gap seams:        $(count_glyph "$EVID/$label-3panes.txt" '│ │')"
    } | tee "$EVID/$label-seam-counts.txt"

    if [ "$label" = master ]; then
        # The control MUST reproduce the bug: two seams per body row across
        # three panes, so ≥2 '│││' occurrences — else the scenario cannot see
        # the defect and the branch leg proves nothing.
        if [ "$seams" -lt 2 ]; then
            _af_fail "control leg: expected ≥2 '│││' seams on master, saw $seams — bug not reproduced"
            return 1
        fi
    else
        if [ "$seams" -ne 0 ] ||
            grep -qF '┐│┌' "$EVID/$label-3panes.txt" ||
            grep -qF '┘│└' "$EVID/$label-3panes.txt"; then
            _af_fail "branch leg: seam still draws a third border (#5279 not fixed)"
            af_capture >&2
            return 1
        fi
        grep -qF '│ │' "$EVID/$label-3panes.txt" ||
            { _af_fail "branch leg: no '│ │' gap seam — the panes did not tile as expected"; return 1; }
    fi
    af_quit || return 1
}

run_leg master "$HOME/bin/af.master"
run_leg branch "$HOME/bin/af"

printf 'PASS #5279: master control reproduces the │││ seam; branch renders │ │ with no triple border\n'
