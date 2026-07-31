#!/usr/bin/env bash
# Real-TUI regression drive for #2580:
#
#   scripts/testbox.sh scenario scripts/tui-2580-scenario.sh
#
# It checks the roomy chrome copy first, then opens a second real pane at 80
# columns and inspects the rendered transient notice before its timer clears.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=200 AF_DRIVER_ROWS=30
af_reset_sandbox
af_boot
af_new_instance alpha
af_select alpha

failures=0
if ! af_assert_screen 'ctrl\+u/ctrl\+d preview scroll' \
    'preview scrolling has one combined hint'; then
    failures=$((failures + 1))
fi
if ! af_refute_screen 'preview scroll.*preview scroll' \
    'preview-scroll description appears only once'; then
    failures=$((failures + 1))
fi
if ! af_assert_screen 'Projects \([0-9]+\) · enter switch' \
    'Projects header has one space before its hint'; then
    failures=$((failures + 1))
fi

af_resize 80 24
af_open_pane
af_focus_tree
af_new_instance beta
af_select beta
af_open_pane
# Backticks are literal TUI copy, not shell command substitution.
# shellcheck disable=SC2016
if ! af_assert_screen '`s` open pane.*resize wider' \
    'narrow auto-hide notice preserves the pane recovery action'; then
    failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    _af_fail "#2580: ${failures} chrome regression assertion(s) failed"
    exit 1
fi

echo 'PASS: #2580 TUI chrome copy and narrow recovery notice'
