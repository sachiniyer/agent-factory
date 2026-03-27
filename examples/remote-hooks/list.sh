#!/usr/bin/env bash
# Skeleton list script for Agent Factory remote hooks.
# Lists all running remote agent sessions.
#
# Required args: --json
# stdout: JSON [{"name": "...", "status": "running"}, ...]

set -euo pipefail

# TODO: Replace with your infrastructure query logic
# Examples:
#   - Query cloud API for running containers
#   - List tmux sessions on a remote host via SSH
#   - Query Kubernetes for running pods

echo "[]"
