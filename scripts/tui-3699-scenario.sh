#!/usr/bin/env bash
# Real-daemon regression for #3699, via:
#
#   scripts/testbox.sh scenario scripts/tui-3699-scenario.sh
#
# The invariant: a root agent whose tmux session vanishes while a marker-carrying
# process outlives it must heal WITHOUT a human killing that process.
#
# The unit test in daemon/rootagent_reap_cohort_test.go models the tmux layer with
# a backend double, because daemon tests cannot spawn real panes. This scenario is
# the honest oracle for the same defect: a real daemon, a real tmux server, a real
# escaped process carrying the vanished session's OWN AF_SESSION/AF_HOME/
# AF_SESSION_GEN — which is exactly what `claude daemon run` leaves behind, since
# it inherits those from the pane and reparents away from it.
#
# Pre-fix this scenario hangs at "the root came back" until the timeout:
# reapDeadRoot called the strict Kill, the blind sweep's generation cohort was
# empty, markedOrphanProcesses refused the survivor, deleteSessionRecord refused
# to drop the record, and the always-ensure loop could never re-create the title.
# There is no in-product exit from that state, which is why it is worth a real-TUI
# gate and not only a unit test.
#
# It deliberately asserts BOTH halves. "The root came back" alone would also pass
# for a fix that deleted the record and left the escaped process running, which is
# the shortcut #1917 exists to forbid.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

ROOT_WAIT=90 # generous: the ensure loop is backoff-throttled after a failure

# _root_session — the root's tmux session name (af_<repohash>_root), or empty.
# `|| true` is load-bearing, not defensive noise: under `set -euo pipefail` a
# grep that matches nothing fails the pipeline, and a bare assignment from a
# failing command substitution exits the script. "No root session right now" is
# the NORMAL state between the kill and the heal — the exact window this
# scenario is about — so without this the script dies silently there, printing
# no failure of its own. Measured: it did, on the first run.
_root_session() {
    tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E '^af_.*_root$' | head -1 || true
}

# _wait_root_session <timeout> <label> — wait for a root tmux session to exist.
_wait_root_session() {
    local deadline; deadline=$(( $(_af_now) + $1 ))
    while [ "$(_af_now)" -lt "$deadline" ]; do
        [ -n "$(_root_session)" ] && return 0
        sleep 1
    done
    _af_fail "timed out waiting for $2"
    return 1
}

_seed_config() {
    # bash as the root program: cheap, deterministic, and this defect is about
    # session teardown, not about which agent runs in the pane.
    af_set_config "default_program = \"claude\"

[program_overrides]
claude = \"bash\"

[root_agents]
\"$AF_DRIVER_REPO\" = { program = \"bash\" }"
}

# --- 1. a root agent exists ---------------------------------------------------
export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
af_reset_sandbox
_seed_config
af_boot

_wait_root_session "$ROOT_WAIT" 'the daemon to ensure a root agent' || exit 1
ORIG_ROOT="$(_root_session)"
_af_log "root tmux session: $ORIG_ROOT"

# --- 2. plant a marked survivor, carrying the root's OWN markers ---------------
#
# Read the markers off the live pane rather than reconstructing them: AF_HOME must
# match this sandbox EXACTLY or markedOrphanProcesses silently skips the process as
# a foreign home (#1122) and the wedge never reproduces — a green run that proved
# nothing. Same for the generation, which is minted per launch.
PANE_PID="$(tmux list-panes -t "=$ORIG_ROOT" -F '#{pane_pid}' 2>/dev/null | head -1 || true)"
[ -n "$PANE_PID" ] || { _af_fail "could not read the root pane pid"; exit 1; }

_marker() { tr '\0' '\n' < "/proc/$PANE_PID/environ" | grep "^$1=" | head -1 | cut -d= -f2- || true; }
M_SESSION="$(_marker AF_SESSION)"
M_HOME="$(_marker AF_HOME)"
M_GEN="$(_marker AF_SESSION_GEN)"

[ "$M_SESSION" = "$ORIG_ROOT" ] || { _af_fail "pane AF_SESSION '$M_SESSION' != '$ORIG_ROOT'"; exit 1; }
[ -n "$M_HOME" ] || { _af_fail "root pane carries no AF_HOME marker"; exit 1; }
[ -n "$M_GEN" ] || { _af_fail "root pane carries no AF_SESSION_GEN marker"; exit 1; }
_af_log "root markers: AF_SESSION=$M_SESSION AF_HOME=$M_HOME AF_SESSION_GEN=$M_GEN"

# setsid + a new session id is the `claude daemon run` shape: it detaches from the
# pane's process group so killing the tmux session cannot take it with it.
SURVIVOR_PIDFILE="$(mktemp)"
# shellcheck disable=SC2016  # $$ must expand in the INNER sh, not here
env AF_SESSION="$M_SESSION" AF_HOME="$M_HOME" AF_SESSION_GEN="$M_GEN" \
    setsid sh -c 'echo $$ > "$1"; exec sleep 600' _ "$SURVIVOR_PIDFILE" &
sleep 1
SURVIVOR="$(cat "$SURVIVOR_PIDFILE" 2>/dev/null || true)"
if [ -z "$SURVIVOR" ] || ! kill -0 "$SURVIVOR" 2>/dev/null; then
    _af_fail "could not plant the marked survivor"; exit 1
fi
# The cleanup trap SIGKILLs the survivor if it is still around. That is a
# safety net for an aborted run, and it is also a trap in the other sense: on
# the first run the script died early and the trap's own kill printed a
# job-control "Killed" line that reads exactly like the daemon reaping it.
# Step 5 records its verdict in SURVIVOR_REAPED before this can ever fire, and
# the trap says who did it, so the two can never be confused again.
SURVIVOR_REAPED=unknown
# shellcheck disable=SC2064
trap "[ \"\$SURVIVOR_REAPED\" = yes ] || _af_log \"cleanup trap killing survivor $SURVIVOR (scenario verdict: \$SURVIVOR_REAPED)\"; kill -9 $SURVIVOR 2>/dev/null || true; rm -f $SURVIVOR_PIDFILE" EXIT
_af_log "planted marked survivor pid $SURVIVOR"

# The precondition the whole scenario rests on: this process really does carry the
# vanished session's own markers, so the strict sweep really will refuse it.
grep -qa "AF_SESSION=$M_SESSION" "/proc/$SURVIVOR/environ" \
    || { _af_fail "precondition: the survivor does not carry AF_SESSION=$M_SESSION"; exit 1; }
grep -qa "AF_SESSION_GEN=$M_GEN" "/proc/$SURVIVOR/environ" \
    || { _af_fail "precondition: the survivor does not carry the root's own generation"; exit 1; }

# --- 3. vanish the root's tmux session ----------------------------------------
#
# kill-session, not `af sessions kill`: the outage class is "tmux went away under a
# healthy daemon" (#1104), with no kill intent on record. A user kill takes an
# entirely different path (rootkill.go) and would not exercise this.
tmux kill-session -t "=$ORIG_ROOT"
_af_log "killed tmux session $ORIG_ROOT; survivor $SURVIVOR still alive: $(kill -0 "$SURVIVOR" 2>/dev/null && echo yes || echo no)"
kill -0 "$SURVIVOR" 2>/dev/null \
    || { _af_fail "the survivor died with the pane, so there is nothing for the sweep to refuse"; exit 1; }

# --- 4. the root must come back, unattended -----------------------------------
deadline=$(( $(_af_now) + ROOT_WAIT ))
NEW_ROOT=""
while [ "$(_af_now)" -lt "$deadline" ]; do
    candidate="$(_root_session)"
    if [ -n "$candidate" ]; then NEW_ROOT="$candidate"; break; fi
    sleep 2
done
[ -n "$NEW_ROOT" ] || {
    _af_fail "the root agent never came back within ${ROOT_WAIT}s — this is the #3699 wedge"
    exit 1
}
_af_log "root re-created as $NEW_ROOT"

# It must be a genuinely new runtime, not the old pane resurfacing.
NEW_PANE="$(tmux list-panes -t "=$NEW_ROOT" -F '#{pane_pid}' 2>/dev/null | head -1 || true)"
if [ -z "$NEW_PANE" ] || [ "$NEW_PANE" = "$PANE_PID" ]; then
    _af_fail "the re-created root reused pane pid $PANE_PID; that is not a fresh runtime"; exit 1
fi
NEW_GEN="$(tr '\0' '\n' < "/proc/$NEW_PANE/environ" | grep '^AF_SESSION_GEN=' | head -1 | cut -d= -f2- || true)"
if [ -z "$NEW_GEN" ] || [ "$NEW_GEN" = "$M_GEN" ]; then
    _af_fail "the re-created root carries the old generation '$M_GEN'; it was not re-created"; exit 1
fi
_af_log "replacement generation $NEW_GEN differs from the reaped $M_GEN"

# --- 5. and the survivor must be gone ------------------------------------------
#
# Without this the scenario would also pass for a fix that dropped the record and
# left the escaped process running — the exact shortcut #1917 forbids. The trusted
# sweep is supposed to REAP it, so give the bounded TERM->KILL escalation a moment
# and then insist.
deadline=$(( $(_af_now) + 30 ))
while [ "$(_af_now)" -lt "$deadline" ]; do
    kill -0 "$SURVIVOR" 2>/dev/null || break
    sleep 1
done
if kill -0 "$SURVIVOR" 2>/dev/null; then
    _af_fail "the root healed but its marked survivor (pid $SURVIVOR) is still running: the record was dropped around an unsettled teardown, not cleared by it"
    exit 1
fi
SURVIVOR_REAPED=yes
_af_log "marked survivor $SURVIVOR was reaped by the teardown (not by this script)"

# --- 6. the TUI agrees ---------------------------------------------------------
af_wait_for 'root.*●' "$AF_DRIVER_TIMEOUT" 'a ready root row in the sidebar' || exit 1

_af_log "#3699 scenario: PASS"
