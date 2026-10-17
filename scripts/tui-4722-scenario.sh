#!/usr/bin/env bash
# Real-TUI regression for #4722 — a key consumed as a TUI mode switch must
# never reach the pane's PTY.
#
#   scripts/testbox.sh scenario scripts/tui-4722-scenario.sh
#
# or, on a parked sandbox (`scripts/testbox.sh playtest -d`), inject the
# provenance inputs yourself — the container cannot resolve them from the
# linked-worktree /src:
#
#   git archive --format=tar fd419f47d56eb0826ba8df2097d2312b8a25b834 >/tmp/m.tar
#   chmod 644 /tmp/m.tar
#   docker cp /tmp/m.tar <name>:/tmp/af-master-src.tar
#   docker exec -e AF_BRANCH_SHA="$(git rev-parse HEAD)" \
#               -e AF_MASTER_SHA=fd419f47d56eb0826ba8df2097d2312b8a25b834 \
#               -e AF_MASTER_TAR=/tmp/af-master-src.tar \
#               <name> bash /src/scripts/tui-4722-scenario.sh
#
# The bug: the key that switched a focused pane from nav to interactive mode
# was REPLAYED into the pane's PTY on activation — with text sitting on the
# shell's input line, that Enter submits it (a half-typed command executes).
# The first-run interactive help's dismiss key leaked the same way.
#
# Both legs run in THIS sandbox against binaries built inside it:
#   af.master — git archive of the pinned pre-fix control revision, which MUST
#               show both leaks (proves the scenario can detect them)
#   af        — the playtest entry's branch build, which must show NEITHER
# Provenance (git SHAs + sha256sums + per-leg /proc/<pid>/exe) is recorded
# first so every capture can be tied to exact code.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=36
# Clear bit 8 (interactive-pane help) so each fresh home shows the first-run
# interactive help on the first entry — one of the two leak paths under test.
export AF_DRIVER_HELP_SEEN=7

# The control must stay a KNOWN-BUGGY revision: once this fix merges, archiving
# the moving origin/master would build a fixed control that can never show the
# leak, and the master leg would fail forever after. `testbox.sh scenario`
# archives this pinned SHA in place of origin/master; a manual run passes the
# same SHA as AF_MASTER_SHA with a tar of it. fd419f47d5 is v1.0.312 — the
# master this fix is cut from.
AF_CONTROL_SHA=fd419f47d56eb0826ba8df2097d2312b8a25b834

EVID="$HOME/sandbox/enter-leak-4722"
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

# marker_output <file> <marker> — true iff a screen line carries the marker
# WITHOUT its 'echo ' prefix: the command's OUTPUT line. This is THE leak
# signature — after the command runs, scrollback keeps the echoed input line
# ('…$ echo MARKER') on screen forever, so asserting on the marker's bare
# presence or absence cannot distinguish ran-vs-intact.
marker_output() { grep -F "$2" "$1" | grep -vqF "echo $2"; }

# marker_count <file> <marker> — occurrences of the marker string in a capture.
# 1 = only the typed input line (intact, never ran); >=2 = the submitted
# command line plus its output line (leaked).
marker_count() { { grep -oF "$2" "$1" || true; } | wc -l; }

# wait_for_marker_output <file> <marker> — poll capture-pane until the
# echo-less output line appears. Self-synchronizing for the positive asserts
# (negative asserts get a fixed settle instead — a refute can never poll).
wait_for_marker_output() {
    local file="$1" marker="$2" deadline
    deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while :; do
        af_capture >"$file"
        marker_output "$file" "$marker" && return 0
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "marker output '$marker' never appeared"
            cat "$file" >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
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

# --- leg ---------------------------------------------------------------------
#
# Same keystrokes on both legs; only the assertions differ.
#
#   1. Stage text on the shell's input line BEFORE the first interactive
#      entry — via full-screen attach — so the help-dismiss keypress has
#      something to submit if it leaks.
#   2. First entry pops the first-run interactive help (HELP_SEEN=7). Dismiss
#      it explicitly with Enter: on master the dismissal replays the key and
#      submits 'echo DISMISS-ENTER'; on the fix it is consumed by the overlay.
#   3. Clear the line in-mode (C-c forwards on both legs — a real SIGINT on
#      the branch's still-typed line, a harmless interrupt on master's empty
#      one), then type 'echo LEAKED-COMMAND' WITHOUT Enter, exit to nav, and
#      press Enter — the transition key under test.
#   4. Branch-only: press Enter once more (now in-mode) and confirm the command
#      actually executes — the earlier pass was suppression, not a dead pane.

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

    af_new_instance leakctl
    af_select leakctl

    # Stage 'echo DISMISS-ENTER' on the shell's input line without submitting
    # it, through a full-screen attach — the pane then opens over live input.
    af_attach
    af_send_literal 'echo DISMISS-ENTER'
    sleep 1 # let the paste land in the shell's line editor before detaching
    af_detach
    af_open_pane
    af_capture >"$EVID/$label-1-staged-input.txt"
    af_assert_screen 'echo DISMISS-ENTER' "staged input visible in pane" || return 1

    # First interactive entry: the first-run help intercepts the Enter.
    af_send Enter
    af_wait_for 'to navigate' "$AF_DRIVER_TIMEOUT" 'first-run interactive help' || return 1
    af_capture >"$EVID/$label-2-help-overlay.txt"

    # Dismiss it explicitly, with Enter — the sibling leak path.
    af_send Enter
    af_wait_for 'nav mode' "$AF_DRIVER_TIMEOUT" 'interactive mode after help dismiss' || return 1
    if [ "$label" = master ]; then
        # Control: the dismissal Enter replays into the pane and submits the
        # staged command — its output line (bare DISMISS-ENTER) must appear.
        wait_for_marker_output "$EVID/$label-3-after-help-dismiss.txt" 'DISMISS-ENTER' || return 1
    else
        # The dismissal Enter is consumed by the overlay: no output line, and
        # the staged text is still sitting on the input line.
        sleep 2 # settle: give a would-be leak time to render before refuting
        af_capture >"$EVID/$label-3-after-help-dismiss.txt"
        if marker_output "$EVID/$label-3-after-help-dismiss.txt" 'DISMISS-ENTER'; then
            _af_fail "branch leg: help-dismiss Enter submitted the input line (#4722 not fixed)"
            return 1
        fi
        grep -qF 'echo DISMISS-ENTER' "$EVID/$label-3-after-help-dismiss.txt" ||
            { _af_fail "branch leg: 'echo DISMISS-ENTER' not on the input line after help dismiss"; return 1; }
    fi

    # Clean prompt on both legs, then the main repro: type the command, leave
    # interactive with Ctrl-], and press Enter to re-enter.
    af_send C-c
    sleep "$AF_DRIVER_POLL"
    af_send_literal 'echo LEAKED-COMMAND'
    af_exit_interactive
    af_capture >"$EVID/$label-4-before-transition.txt"
    af_assert_screen 'echo LEAKED-COMMAND' "repro text typed on input line" || return 1

    af_send Enter
    af_wait_for 'nav mode' "$AF_DRIVER_TIMEOUT" 'interactive mode re-entered' || return 1

    if [ "$label" = master ]; then
        # Control: the transition Enter replays and submits the half-typed
        # command — the #4722 leak, reproduced.
        wait_for_marker_output "$EVID/$label-5-after-transition.txt" 'LEAKED-COMMAND' || return 1
    else
        sleep 2 # settle before refuting
        af_capture >"$EVID/$label-5-after-transition.txt"
        if marker_output "$EVID/$label-5-after-transition.txt" 'LEAKED-COMMAND'; then
            _af_fail "branch leg: transition Enter submitted the input line (#4722 not fixed)"
            return 1
        fi
        [ "$(marker_count "$EVID/$label-5-after-transition.txt" 'LEAKED-COMMAND')" -eq 1 ] ||
            { _af_fail "branch leg: input line not byte-for-byte intact (marker count != 1)"; return 1; }
        grep -qF 'echo LEAKED-COMMAND' "$EVID/$label-5-after-transition.txt" ||
            { _af_fail "branch leg: 'echo LEAKED-COMMAND' not sitting on the input line"; return 1; }

        # In-mode forwarding is intact: a real Enter now submits the line.
        af_send Enter
        wait_for_marker_output "$EVID/$label-6-inmode-enter.txt" 'LEAKED-COMMAND' || return 1
    fi

    af_quit || return 1
}

run_leg master "$HOME/bin/af.master"
run_leg branch "$HOME/bin/af"

printf 'PASS #4722: master leaks both transition keys into the pane; branch consumes both and keeps the input line intact\n'
