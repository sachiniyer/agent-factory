#!/usr/bin/env bash
# Real-TUI log-level regression for #2575, via:
#
#   scripts/testbox.sh scenario scripts/tui-2575-scenario.sh
#
# The transient status surface carries both real operation failures and
# designed action refusals. Drive an ordinary refusal through the live TUI and
# prove its log signal is INFO, not ERROR.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

cleanup() {
    af_quit >/dev/null 2>&1 || true
}
trap cleanup EXIT

af_reset_sandbox
af_boot
af_new_instance notice-target
af_ensure_nav
af_focus_tree
af_select notice-target

af_send c
af_wait_for 'not blocked on a usage limit' "$AF_DRIVER_TIMEOUT" \
    'designed retry refusal appears in the status surface'

log_file="$AGENT_FACTORY_HOME/agent-factory.log"
if grep -E 'ERROR:.*notice-target.*not blocked on a usage limit' "$log_file" >/dev/null; then
    _af_fail '#2575: a designed TUI action refusal was logged as ERROR:'
    grep -E 'ERROR:.*notice-target.*not blocked on a usage limit' "$log_file" >&2
    exit 1
fi
if ! grep -E 'INFO:.*notice-target.*not blocked on a usage limit' "$log_file" >/dev/null; then
    _af_fail '#2575: the truthful action-refusal fact was not retained at INFO'
    exit 1
fi

_af_log 'assert OK: designed TUI action refusal is visible at INFO and absent from ERROR'
echo 'PASS: #2575 TUI notices and operation failures use distinct log levels'
