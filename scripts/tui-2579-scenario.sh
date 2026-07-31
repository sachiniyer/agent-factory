#!/usr/bin/env bash
# Real-TUI separator-copy regression drive for #2579:
#
#   scripts/testbox.sh scenario scripts/tui-2579-scenario.sh
#
# Source-level tests cover every production string literal. This drive checks
# two representative live overlays through the real Bubble Tea application.
set -euo pipefail

export AF_DRIVER_COLS=160 AF_DRIVER_ROWS=30

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

af_reset_sandbox
af_boot

failures=0
af_send /
af_wait_for 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search overlay'
if ! af_assert_screen '↑/↓ navigate · enter select · esc close' \
    'search actions use middle-dot separators'; then
    failures=$((failures + 1))
fi
af_send Escape
af_wait_gone 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search overlay closed'

af_open_tasks
if ! af_assert_screen 'r run now · ↑/↓ · n new' \
    'task actions use middle-dot separators'; then
    failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    _af_fail "#2579: ${failures} separator assertion(s) failed"
    exit 1
fi

echo 'PASS: #2579 real TUI uses the separator copy conventions'
