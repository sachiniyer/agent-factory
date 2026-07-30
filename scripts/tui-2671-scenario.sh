#!/usr/bin/env bash
# Real-TUI regression for #2671, via:
#
#   scripts/testbox.sh scenario scripts/tui-2671-scenario.sh
#
# A server-side pane exit ends the full-screen WS attach without a detach key.
# The first key after Bubble Tea reclaims the terminal must reach the TUI rather
# than the attach's old stdin reader. The test sends exactly one `?` and expects
# the help screen; retrying would hide the bug.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_boot
af_new_instance disconnect-me
af_select disconnect-me
af_attach

# Stop only this disposable sandbox's serving daemon, forcing the real WS attach
# connection to end without a detach key. Resolve and verify the exact pid first;
# the host daemon is outside the container and cannot be reached from here.
daemon_pid="$("$HOME/bin/af" daemon status --json | jq -er '.data.serving_pid')"
daemon_cmd="$(tr '\0' ' ' <"/proc/$daemon_pid/cmdline")"
if ! printf '%s\n' "$daemon_cmd" | grep -qE '/af --daemon([[:space:]]|$)'; then
    _af_fail "#2671: refusing to signal unverified sandbox daemon pid $daemon_pid: $daemon_cmd"
    exit 1
fi
kill -TERM "$daemon_pid"
af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'server disconnect returns to TUI'

# One keypress, no retry: the leaked attach reader wins the terminal FIFO on the
# unfixed tree and consumes this byte before Bubble Tea can see it.
af_send '?'
af_wait_for 'Agent Factory v' 5 'first post-disconnect keystroke opens help'

echo 'PASS: #2671 first post-disconnect keystroke reaches the TUI'
