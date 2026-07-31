#!/usr/bin/env bash
# Real-TUI regression for #2578, via:
#
#   scripts/testbox.sh scenario scripts/tui-2578-scenario.sh
#
# The text overlay is centered over the whole terminal, so its height must
# reserve enough vertical margin to stop above the two-row status bar. Check
# the actual footer row at both geometries from the issue.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=80 AF_DRIVER_ROWS=24
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

assert_help_stops_above_menu() {
    local menu_row screen
    screen="$(af_capture)"
    menu_row="$(printf '%s\n' "$screen" | sed -n "$((AF_DRIVER_ROWS - 1))p")"
    if printf '%s\n' "$menu_row" | grep -qE '╰|╯'; then
        printf '%s\n' "$screen" >&2
        _af_fail "#2578: help border paints through the footer menu at ${AF_DRIVER_COLS}x${AF_DRIVER_ROWS}"
        return 1
    fi
    _af_log "assert OK: footer menu row is unobstructed at ${AF_DRIVER_COLS}x${AF_DRIVER_ROWS}"
}

af_reset_sandbox
af_boot
af_send '?'
af_wait_for 'Agent Factory v' "$AF_DRIVER_TIMEOUT" 'general help opens'
assert_help_stops_above_menu

af_resize 200 50
af_wait_for 'Agent Factory v' 5 'help survives resize to 200x50'
assert_help_stops_above_menu

echo 'PASS: #2578 help border stops above the footer menu'
