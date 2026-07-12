#!/usr/bin/env bash
# Skeleton delete_cmd for the Agent Factory remote-hook backend. Tears down
# whatever launch_cmd provisioned (the af agent-server + its workspace).
#
# Args:   --name <slug>
# stdout: anything (a {"deleted": true} ack is conventional, not required)
# stderr: progress logs

set -euo pipefail

NAME=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --name) NAME="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ -n "$NAME" ]] || { echo "Error: --name is required" >&2; exit 1; }

echo "Tearing down session $NAME..." >&2

# TODO: Replace with your cleanup logic — the inverse of launch.sh:
#   - kill the af agent-server you started (e.g. by a recorded PID)
#   - remove the workspace / stop the container / delete the pod / close the tunnel

echo "{\"deleted\": true}"
