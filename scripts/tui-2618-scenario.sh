#!/usr/bin/env bash
# Real-TUI regression for #2618, via:
#
#   scripts/testbox.sh scenario scripts/tui-2618-scenario.sh
#
# Open two panes at 80 columns so the real layout emits a clipped auto-hide
# notice. Wait for the three-second visual timeout, then recover the full tail
# through E details.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=80 AF_DRIVER_ROWS=24
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_boot
af_new_instance details-check
af_select details-check
af_new_tab
af_open_pane

af_wait_for 'E details' "$AF_DRIVER_TIMEOUT" 'auto-hide notice is clipped'
af_wait_gone 'E details' 6 'transient notice times out'

af_send E
af_wait_for 'Message details' 5 'E opens details after the timeout'
af_assert_screen 'resize wider or use' 'details recover the clipped notice tail'

echo 'PASS: #2618 error details survive the transient notice timeout'
