#!/usr/bin/env bash
# Real-TUI play-test for #5296 — a Lost row must show
# "restore is waiting on the environment …" while a restore's rebuild hits a
# REAL ENOSPC, keep showing it while the condition holds, and clear it when
# the space frees. Master (control) must never show the phrase under the
# identical seeded condition.
#
#   AF_TESTBOX_TMPFS=/home/dev/wt-tmpfs:mode=1777,size=16m \
#       scripts/testbox.sh scenario scripts/tui-5295-scenario.sh
#
# or, on a parked sandbox launched with the same --tmpfs flag
# (`scripts/testbox.sh playtest -d`), inject the provenance inputs yourself —
# the container cannot resolve them from the linked-worktree /src:
#
#   git archive --format=tar 49670742a83874aa0aa9e5e2e3a1782e7c1d91db >/tmp/m.tar
#   chmod 644 /tmp/m.tar
#   docker cp /tmp/m.tar <name>:/tmp/af-master-src.tar
#   docker exec -e AF_BRANCH_SHA="$(git rev-parse HEAD)" \
#               -e AF_MASTER_SHA=49670742a83874aa0aa9e5e2e3a1782e7c1d91db \
#               -e AF_MASTER_TAR=/tmp/af-master-src.tar \
#               <name> bash /src/scripts/tui-5295-scenario.sh
#
# The ENOSPC is real, not seeded: worktree_root=subdirectory puts every
# session worktree under $AGENT_FACTORY_HOME/worktrees, which each leg symlinks
# onto the --tmpfs mount the sandbox launched with — then this script fills it
# to ~100% so the restore's `git worktree add` genuinely fails on a full
# filesystem. (The tmpfs mounts at a scratch path because dockerd creates
# missing mountpoint parents as root:root, which would leave $AGENT_FACTORY_HOME
# unwritable by the dev user; the symlink routes the same writes onto it.) The
# wait stays armed for as long as the disk is full, so observations never
# race the poll window; freeing the filler produces the honest-clear leg.
#
# KNOWN RISK the maintainer asked this run to measure: if the restore's
# first ENOSPC arrives inside a git SUBPROCESS, it surfaces as
# *exec.ExitError with stderr text — not a wrapped errno — and the #5295
# classifier cannot see it (no string matching, by design). In that case
# the branch leg shows NO wait text and this scenario reports FAIL along
# with the daemon-log line naming the call that produced (or failed to
# produce) the classified ENOSPC. The setup deliberately does not dodge it.
#
# Both legs run in THIS sandbox against binaries built inside it:
#   af.master — git archive of the pinned pre-fix control revision, which MUST
#               show no wait text under the identical condition
#   af        — the playtest entry's branch build, which must show it iff a
#               classified ENOSPC is produced
# Provenance (git SHAs + sha256sums) is recorded first so every capture can be
# tied to exact code.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
export AF_DRIVER_COLS=200 AF_DRIVER_ROWS=50

# The control must stay a KNOWN pre-fix revision: once this feature merges,
# archiving the moving origin/master would build a control that renders the
# wait text itself, and the master leg would fail the absence assertion
# forever after. `testbox.sh scenario` archives this pinned SHA in place of
# origin/master; a manual run passes the same SHA as AF_MASTER_SHA with a tar
# of it. 49670742a8 is origin/master as of this scenario's writing.
AF_CONTROL_SHA=49670742a83874aa0aa9e5e2e3a1782e7c1d91db

EVID="$HOME/sandbox/enospc-5295"
# Fresh evidence per run: a retained sandbox's rerun must not mix this run's
# provenance with a prior run's captures.
case "$EVID" in */sandbox/*) rm -rf "$EVID" ;; esac
mkdir -p "$EVID"

WT_TMPFS=/home/dev/wt-tmpfs
WT_LINK="$AGENT_FACTORY_HOME/worktrees"
DAEMON_LOG="$AGENT_FACTORY_HOME/agent-factory.log"
WAIT_PHRASE='restore is waiting on the environment'

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

# --- guards ------------------------------------------------------------------

# The mechanism only works when the tmpfs is really there — on a retained
# sandbox started without AF_TESTBOX_TMPFS the fill would write the container
# overlay instead (and possibly hurt the host).
if [ "$(stat -f -c %T "$WT_TMPFS" 2>/dev/null || echo unknown)" != "tmpfs" ]; then
    _af_fail "$WT_TMPFS is not a tmpfs — launch with AF_TESTBOX_TMPFS=$WT_TMPFS:mode=1777,size=16m"
    exit 1
fi
[ -w "$WT_TMPFS" ] || { _af_fail "$WT_TMPFS is a tmpfs but not writable (needs mode=1777)"; exit 1; }

# tui_provenance <label> <bin> — prove WHICH binary the running TUI is (a leg
# that silently ran the wrong binary voids the control). Same as the #5279
# scenario: resolve the drive pane's af child through /proc/<pid>/exe and
# sha256 it against the intended binary.
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

# fill_tmpfs — write the worktree tmpfs to ~100%. dd stops at ENOSPC; the
# guard then PROVES the filesystem is full so a later absence of wait text
# means classification, not a setup that never produced the condition.
fill_tmpfs() {
    dd if=/dev/zero of="$WT_TMPFS/.filler" bs=1M status=none 2>/dev/null || true
    df -kP "$WT_TMPFS" | tee "$EVID/df-$1.txt"
    local avail
    avail="$(df -kP "$WT_TMPFS" | awk 'NR==2 {print $4}')"
    [ "$avail" -le 1024 ] || {
        _af_fail "tmpfs fill left ${avail}k available — the ENOSPC condition is not established"
        return 1
    }
}

# harvest_restore_log <label> — the evidence the maintainer asked for: which
# call produced (or failed to produce) a CLASSIFIED ENOSPC. The environmental
# arm logs "environmental failure … waiting on the environment"; a structural
# charge logs "failed (attempt N), retrying in"; give-up logs "giving up".
harvest_restore_log() {
    {
        echo "== restore/environmental lines for wait-row =="
        grep -nE 'wait-row.*(restore of lost session|environmental failure|giving up|retrying)' "$DAEMON_LOG" 2>/dev/null | tail -30 || true
        echo "== ENOSPC-class error text in the daemon log =="
        grep -niE 'no space left|enospc|worktree add|worktree unavailable' "$DAEMON_LOG" 2>/dev/null | tail -30 || true
    } | tee "$EVID/$1-daemon-restore.log"
}

# force_lost_enospc — the incident's shape: the session's tmux backend is
# gone AND its worktree vanished, so restore must rebuild the worktree —
# which fails on the now-full tmpfs.
force_lost_enospc() {
    local label="$1" wt tm
    wt="$(find "$WT_TMPFS" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -1)"
    tm="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep '^af_' | head -1 || true)"
    {
        echo "worktree found: ${wt:-<none>}"
        echo "tmux session found: ${tm:-<none>}"
        tmux list-sessions 2>&1 || true
    } | tee "$EVID/$label-prestore.txt"
    [ -n "$wt" ] || { _af_fail "$label: no session worktree under $WT_TMPFS"; return 1; }
    [ -n "$tm" ] || { _af_fail "$label: no af_ tmux session for wait-row"; return 1; }
    tmux kill-session -t "$tm"
    rm -rf "$wt"
    fill_tmpfs "$label" || return 1
}

# --- legs --------------------------------------------------------------------

# leg_body <master|branch> — the per-binary sequence. Kept separate from
# run_leg so a mid-leg failure still reaches the daemon-log harvest: the FAIL
# report the maintainer asked for NEEDS those lines.
leg_body() { # <master|branch>
    local label="$1"

    af_boot
    # Prove the running TUI is THIS leg's binary before trusting any capture
    # (the control is void if it silently ran the branch build).
    tui_provenance "$label" "$AF_DRIVER_BIN" || return 1

    af_new_instance wait-row || return 1
    force_lost_enospc "$label" || return 1

    if [ "$label" = branch ]; then
        # The wait arms the moment a CLASSIFIED failure lands and stays armed
        # while the condition holds — the full tmpfs keeps every attempt
        # environmental, so this is never a race against the poll.
        if af_wait_for "$WAIT_PHRASE" 150 'wait detail on the Lost row'; then
            af_capture >"$EVID/branch-wait-1.txt"
            sleep 15
            af_capture >"$EVID/branch-wait-2.txt"
            grep -qF "$WAIT_PHRASE" "$EVID/branch-wait-2.txt" || {
                _af_fail "branch: wait detail vanished while ENOSPC still holds — the row must keep claiming the environment"
                return 1
            }
            # Free the space: the next attempt's rebuild+spawn succeeds and
            # the row must stop blaming the environment.
            rm -f "$WT_TMPFS/.filler"
            if af_wait_gone "$WAIT_PHRASE" 240 'wait detail clears after relief'; then
                af_capture >"$EVID/branch-cleared.txt"
                BRANCH_WAIT_SEEN=yes
            else
                _af_fail "branch: wait detail did not clear after the disk freed"
                return 1
            fi
        else
            af_capture >"$EVID/branch-nowait.txt"
            BRANCH_WAIT_SEEN=no
        fi
    else
        # Master has no wait mechanism at all: under the identical seeded
        # condition the phrase must never appear. Two captures inside the
        # retry window plus the log harvest proving the ENOSPC attempts
        # genuinely ran keep the absence from being vacuous.
        sleep 45
        af_capture >"$EVID/master-1.txt"
        # A gave-up verdict is also acceptable absence evidence; bounded wait,
        # tolerated either way since the phrase cannot exist on master.
        af_wait_for 'gave up|giving up' 300 'master terminal verdict' || true
        af_capture >"$EVID/master-2.txt"
    fi
}

run_leg() { # <master|branch> <binary>
    local label="$1" rc=0
    export AF_DRIVER_BIN="$2"

    af_reset_sandbox
    # Route the session worktree root onto the size-limited tmpfs. The reset
    # wipes $AGENT_FACTORY_HOME/worktrees (the symlink), and the previous leg's
    # .filler + stale worktrees live in the TMPFS ITSELF — clean both before af
    # can create a real directory there or fail its own create on the leftover.
    mkdir -p "$AGENT_FACTORY_HOME"
    find "$WT_TMPFS" -mindepth 1 -delete 2>/dev/null || true
    ln -sfn "$WT_TMPFS" "$WT_LINK"
    af_set_config 'default_program = "claude"
worktree_root = "subdirectory"

[program_overrides]
claude = "bash"'

    leg_body "$label" || rc=$?
    harvest_restore_log "$label" || true
    af_quit || true
    return "$rc"
}

BRANCH_WAIT_SEEN=no
MASTER_RC=0; run_leg master "$HOME/bin/af.master" || MASTER_RC=$?
[ "$MASTER_RC" -eq 0 ] || _af_log "master leg errored (rc=$MASTER_RC) — verdict still runs on whatever evidence landed"
BRANCH_RC=0; run_leg branch "$HOME/bin/af" || BRANCH_RC=$?
[ "$BRANCH_RC" -eq 0 ] || _af_log "branch leg errored (rc=$BRANCH_RC) — verdict still runs on whatever evidence landed"

# --- verdict -----------------------------------------------------------------

rc=0
if grep -qF "$WAIT_PHRASE" "$EVID/master-1.txt" || grep -qF "$WAIT_PHRASE" "$EVID/master-2.txt"; then
    _af_fail "control leg: master rendered '$WAIT_PHRASE' — the control is invalid"
    rc=1
elif ! grep -qE 'restore of lost session .wait-row.' "$EVID/master-daemon-restore.log"; then
    _af_fail "control leg: no restore attempts reached the daemon log — the seeded condition never exercised anything"
    rc=1
else
    echo "control leg: PASS — no wait text under real ENOSPC; restore attempts confirmed in the daemon log"
fi

if [ "$BRANCH_WAIT_SEEN" = yes ]; then
    echo "branch leg: PASS — wait detail rendered persistently during ENOSPC and cleared on relief"
    grep -nE 'environmental failure' "$EVID/branch-daemon-restore.log" | head -5 || true
elif [ "$BRANCH_WAIT_SEEN" = no ]; then
    echo "branch leg: FAIL — NO wait text under real ENOSPC; the classified producer never fired" >&2
    echo "=== daemon log lines naming what actually failed ===" >&2
    cat "$EVID/branch-daemon-restore.log" >&2
    rc=1
fi

if [ "$rc" -eq 0 ]; then
    printf 'PASS #5295: branch renders the environmental wait under real ENOSPC and clears it on relief; master control shows none\n'
else
    printf 'FAIL #5295: see %s and the daemon-log evidence above\n' "$EVID"
fi
exit "$rc"
