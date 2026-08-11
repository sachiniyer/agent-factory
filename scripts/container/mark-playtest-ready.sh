#!/usr/bin/env bash
# Publish detached-sandbox readiness only after every component a driver uses
# exists. The marker is atomic so observing it is a deterministic setup join.
set -euo pipefail

SANDBOX="${AF_PLAYTEST_SANDBOX:-$HOME/sandbox}"
AF_HOME="${AGENT_FACTORY_HOME:-$SANDBOX/home}"
READY_FILE="$SANDBOX/playtest-ready"

require() {
    if ! "$@"; then
        echo "play-test: refusing readiness marker; incomplete sandbox" >&2
        exit 1
    fi
}

require test -x "$HOME/bin/af"
require test -s "$AF_HOME/config.json"
require test -s "$SANDBOX/playtest-agent.txt"
require test -d "$SANDBOX/mock-repo/.git"

ready_tmp="$READY_FILE.tmp.$$"
trap 'rm -f "$ready_tmp"' EXIT
printf '%s\n' ready >"$ready_tmp"
mv "$ready_tmp" "$READY_FILE"
trap - EXIT
