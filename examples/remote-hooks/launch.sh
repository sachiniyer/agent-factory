#!/usr/bin/env bash
# Skeleton launch_cmd for the Agent Factory remote-hook backend (provision-and-
# expose, #1592 Phase 4 PR7). It provisions the workspace on YOUR infrastructure,
# starts an `af agent-server` there, and echoes that server's authed endpoint.
#
# Args:  --name <slug> --title <title> --repo <url> [--branch <b>] [--program <p>] [--auto-yes]
# stdout: one JSON object {"url","token","tls_fingerprint"}  (the agent-server banner)
# stderr: progress logs
#
# See docs/remote-hooks.md for the full contract and a ready-to-use reference.

set -euo pipefail

NAME="" TITLE="" REPO="" BRANCH="" PROGRAM="" AUTOYES=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --name)     NAME="$2";    shift 2 ;;
    --title)    TITLE="$2";   shift 2 ;;
    --repo)     REPO="$2";    shift 2 ;;
    --branch)   BRANCH="$2";  shift 2 ;;
    --program)  PROGRAM="$2"; shift 2 ;;
    --auto-yes) AUTOYES="--auto-yes"; shift ;;
    *) shift ;;
  esac
done
[[ -n "$NAME" ]] || { echo "Error: --name is required" >&2; exit 1; }

echo "Provisioning session $NAME..." >&2

# TODO: Replace with your infrastructure provisioning. The essentials:
#   1. Get a workspace on your infra (ssh to a host, launch a pod / container /
#      Modal / Daytona sandbox) that has `af`, `git`, and `tmux` on PATH.
#   2. Clone the repo and, on restore, materialize the archived branch:
#         git clone -q "$REPO" "$WORKDIR/workspace"
#         [[ -n "$BRANCH" ]] && git -C "$WORKDIR/workspace" fetch -q origin "$BRANCH:$BRANCH"
#   3. Start the agent-server, capturing its one-line JSON banner:
#         af agent-server --listen 0.0.0.0:0 --repo "$WORKDIR/workspace" \
#            --title "$TITLE" ${PROGRAM:+--program "$PROGRAM"} $AUTOYES >banner.json 2>log &
#   4. Re-emit its {addr,token,fingerprint} as the endpoint contract below.
#
# The daemon dials the URL you print, so it must be reachable from the daemon
# (a public/forwarded address, or a tunnel you open here).

# echo '{"url": "wss://HOST:PORT", "token": "TOKEN", "tls_fingerprint": "FINGERPRINT"}'
echo "Error: fill in your provisioning logic in launch.sh" >&2
exit 1
