#!/usr/bin/env bash
# Real-TUI regression for #2660, via:
#
#   scripts/testbox.sh scenario scripts/tui-2660-scenario.sh
#
# Registry mode is a supported launch from outside a git repository. It carries
# an intentionally empty repo ID until the user selects a project, so startup
# must not try to restore repo-scoped TUI state and report that sentinel as an
# invalid persisted key.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
nonrepo="$(mktemp -d "${TMPDIR:-/tmp}/af-2660-nonrepo.XXXXXX")"
export AF_DRIVER_REPO="$nonrepo"

cleanup() {
    af_quit >/dev/null 2>&1 || true
    rm -rf "$nonrepo"
}
trap cleanup EXIT

af_reset_sandbox
af_boot

log_file="$AGENT_FACTORY_HOME/agent-factory.log"
if grep -Fq 'invalid repo id: empty' "$log_file"; then
    _af_fail '#2660: registry-mode startup treated its empty repo ID as invalid TUI state:'
    grep -F 'invalid repo id: empty' "$log_file" >&2
    exit 1
fi

_af_log 'assert OK: registry-mode startup emitted no empty-repo TUI-state warning'
echo 'PASS: #2660 registry-mode startup skips repo-scoped TUI-state restore'
