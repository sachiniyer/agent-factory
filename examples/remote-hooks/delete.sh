#!/usr/bin/env bash
# Skeleton delete script for Agent Factory remote hooks.
# Tears down a remote session and cleans up resources.
#
# Required args: --name <name> --json
# stdout: JSON {"name": "...", "deleted": true}
# stderr: progress logs

set -euo pipefail

NAME=""
JSON_OUTPUT=false

while [[ $# -gt 0 ]]; do
  case $1 in
    --name) NAME="$2"; shift 2 ;;
    --json) JSON_OUTPUT=true; shift ;;
    *) shift ;;
  esac
done

if [[ -z "$NAME" ]]; then
  echo "Error: --name is required" >&2
  exit 1
fi

# TODO: Replace with your cleanup logic
# Examples:
#   - Kill the remote tmux session and remove the machine
#   - Stop and delete the cloud container
#   - Delete the Kubernetes pod

echo "Deleting session $NAME..." >&2

if [[ "$JSON_OUTPUT" == true ]]; then
  echo "{\"name\": \"$NAME\", \"deleted\": true}"
fi
