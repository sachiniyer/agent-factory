#!/usr/bin/env bash
# Real-TUI regression for #2640, via:
#
#   scripts/testbox.sh scenario scripts/tui-2640-scenario.sh
#
# Bubble Tea emits bracketed paste as ONE KeyRunes message. Inject the actual
# bracketed-paste protocol around a multi-line payload so this cannot
# accidentally pass by testing one ordinary key event per character.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_boot
af_ensure_nav
af_focus_tree
af_send n
af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form opens'

# One bracketed paste: embedded newline plus carriage-return, tab, and escape.
# The title row must remain one line and concatenate the printable runes.
af_send_literal $'\e[200~alpha\nbeta\r\t\e[201~'
af_wait_for 'alphabeta' 5 'multi-line paste renders as one title row'

af_send Enter
af_wait_for 'alphabeta.*●' "$AF_DRIVER_TIMEOUT" 'sanitized pasted title creates successfully'

echo 'PASS: #2640 bracketed multi-line paste keeps the title and sidebar single-line'
