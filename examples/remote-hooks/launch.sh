#!/usr/bin/env bash
# Skeleton launch script for Agent Factory remote hooks.
# Starts a remote agent session.
#
# Required args: --name <name> --prompt <prompt> --json
# stdout: JSON {"name": "...", "status": "running"}
# stderr: progress logs

set -euo pipefail

NAME=""
PROMPT=""
JSON_OUTPUT=false

while [[ $# -gt 0 ]]; do
  case $1 in
    --name) NAME="$2"; shift 2 ;;
    --prompt) PROMPT="$2"; shift 2 ;;
    --json) JSON_OUTPUT=true; shift ;;
    *) shift ;;
  esac
done

if [[ -z "$NAME" ]] || [[ -z "$PROMPT" ]]; then
  echo "Error: --name and --prompt are required" >&2
  exit 1
fi

# TODO: Replace with your infrastructure provisioning logic
# Examples:
#   - SSH to a machine and start a tmux session
#   - Launch a cloud container (Modal, AWS, GCP, etc.)
#   - Start a Kubernetes pod

echo "Launching session $NAME..." >&2

# Output required JSON
if [[ "$JSON_OUTPUT" == true ]]; then
  echo "{\"name\": \"$NAME\", \"status\": \"running\"}"
fi
