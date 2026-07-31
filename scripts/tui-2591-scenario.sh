#!/usr/bin/env bash
# Real-TUI drive for #2591 and its exact-head review findings, via:
#
#   scripts/testbox.sh scenario scripts/tui-2591-scenario.sh
#
# It provisions a real local session on an explicitly named private tmux socket,
# then makes only that af_* session's kill-session command fail. The daemon must
# retain a durable UserKilled row while the real tmux binding remains reachable.
# That is the state unit app tests cannot create: they replace the backend with a
# fake before the interaction code runs.
set -euo pipefail

# Route the driver, the TUI, and the daemon through one explicitly isolated tmux
# server. The wrapper also provides a deterministic unknown-state teardown: it
# rejects kill-session only for af_* runtime targets, never the driver's session.
real_tmux="$(command -v tmux)"
# Agent Factory deliberately sanitizes PATH before it starts the daemon. Its
# preserved prefix is $HOME/bin (where the sandbox binary also lives), so the
# wrapper must live there or only the driver—not the daemon—would see it.
wrapper="$HOME/bin/tmux"
# The single-quoted lines are the wrapper's source; their variables must expand
# when that generated script runs, not while this scenario writes it.
# shellcheck disable=SC2016
printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'printf "call %s\n" "$*" >>"$AGENT_FACTORY_HOME/tmux-wrapper.log"' \
    'is_kill=0' \
    'runtime_target=' \
    'for arg in "$@"; do' \
    '    case "$arg" in' \
    '        kill-session) is_kill=1 ;;' \
    '        =af_*|af_*) runtime_target="$arg" ;;' \
    '    esac' \
    'done' \
    'if [ "$is_kill" = 1 ] && [ -n "$runtime_target" ]; then' \
    '    printf "blocked %s\n" "$runtime_target" >>"$AGENT_FACTORY_HOME/tmux-wrapper.log"' \
    '    exec sleep 30' \
    'fi' \
    "exec '$real_tmux' -L af-2591 \"\$@\"" >"$wrapper"
chmod +x "$wrapper"
export AF_DRIVER_SESSION=drive-2591

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

af_reset_sandbox
af_boot
af_new_instance killed-handoff
af_select killed-handoff

# Commit the kill through the real UI. The wrapper leaves the runtime present,
# so the daemon retains the tombstone and reports the unknown teardown outcome.
af_send D
af_wait_for "Kill session 'killed-handoff'" "$AF_DRIVER_TIMEOUT" 'kill confirmation'
af_send y
af_wait_for "failed to kill session 'killed-handoff'" "$AF_DRIVER_TIMEOUT" \
    'unknown teardown is surfaced to the user'
grep -q '^blocked =af_' "$AGENT_FACTORY_HOME/tmux-wrapper.log" ||
    _af_fail '#2591 fixture failed: the daemon did not execute the isolated tmux timeout wrapper'

runtime_session="$(tmux list-sessions -F '#{session_name}' | grep '^af_' | head -1)"
[ -n "$runtime_session" ] || _af_fail '#2591 fixture failed: the real runtime did not survive kill-session'
tmux has-session -t "=$runtime_session"
_af_log "assert OK: retained tombstone still has real tmux runtime $runtime_session on socket af-2591"

# Restart the disposable container's real daemon from persisted state, then
# replace only the named driver session. This is the actual cold-restore edge in
# #2591; neither operation reaches the host daemon or the default tmux server.
"$HOME/bin/af" daemon restart
af_boot

# The cold daemon projection must expose the retry. af_select requires the
# row-scoped `D kill` menu item, so it fails if CanKill is false.
af_select killed-handoff
_af_log 'assert OK: retained live tombstone remains explicitly killable after reload'
# af_select anchors with a burst of navigation keys. Let Bubble Tea drain their
# highlight/replay messages before attributing the next edge-triggered key.
sleep 2

# Enter is the real-TUI proof from the cold-restored row. Without the guard it
# opens/focuses the still-live runtime instead of producing the message below;
# focused-pane and full-screen attach use the same guard and are covered at
# their direct action funnels in app/dead_session_test.go.
af_send Enter
af_wait_for "session 'killed-handoff' was killed and is pending deletion" \
    "$AF_DRIVER_TIMEOUT" 'Enter is fenced by durable kill intent'
af_refute_screen 'nav mode' 'Enter did not enter the retained runtime'

echo 'PASS: #2591 retained live tombstone stays killable and non-interactive'
