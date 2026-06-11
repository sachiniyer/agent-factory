#!/usr/bin/env bash
# Skeleton terminal script for Agent Factory remote hooks (optional).
# Opens an interactive shell in a session's remote workspace — this powers
# the TUI's Terminal tab for remote sessions. Unlike attach.sh, which
# connects to the agent's own session, this should drop the user into a
# plain shell next to the agent's work.
#
# Required args: <name>
# This command takes over the terminal (no JSON output).

set -euo pipefail

NAME="${1:?Usage: terminal.sh <session-name>}"

# TODO: Replace with your connection logic
# Examples:
#   - ssh -t user@host "cd /workspace/$NAME && exec \$SHELL -l"
#   - kubectl exec -it pod-$NAME -- /bin/bash

echo "Error: terminal not implemented — replace this script with your connection logic" >&2
exit 1
