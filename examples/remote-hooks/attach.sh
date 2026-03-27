#!/usr/bin/env bash
# Skeleton attach script for Agent Factory remote hooks.
# Gives interactive terminal access to a running session.
#
# Required args: <name>
# This command takes over the terminal (no JSON output).

set -euo pipefail

NAME="${1:?Usage: attach.sh <session-name>}"

# TODO: Replace with your connection logic
# Examples:
#   - ssh -t user@host "tmux attach -t $NAME"
#   - kubectl exec -it pod-$NAME -- tmux attach -t main

echo "Error: attach not implemented — replace this script with your connection logic" >&2
exit 1
