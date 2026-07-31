#!/usr/bin/env bash
# Real-TUI regression for #2577, via:
#
#   scripts/testbox.sh scenario scripts/tui-2577-scenario.sh
#
# The help surface must advertise and honor page-sized navigation, and its
# wrapped descriptions must stay aligned under the description rather than
# masquerading as extra bindings in the key column.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=80 AF_DRIVER_ROWS=24
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_boot
af_send '?'
af_wait_for 'Agent Factory v' "$AF_DRIVER_TIMEOUT" 'general help opens'
af_assert_screen 'pgup/pgdn' 'help advertises page-sized navigation'

af_send PageDown
af_wait_for '↑ more' 5 'PageDown moves the help viewport'
af_refute_screen 'Agent Factory v' 'PageDown moves a screenful rather than one row'

af_send End
af_wait_for 'Other:' 5 'End jumps to the bottom of help'

screen="$(af_capture)"
quit_line="$(printf '%s\n' "$screen" | grep -m1 'Quit the' || true)"
continuation_line="$(printf '%s\n' "$screen" | grep -m1 'application' || true)"
if [ -z "$quit_line" ] || [ -z "$continuation_line" ]; then
    printf '%s\n' "$screen" >&2
    _af_fail '#2577: the quit description did not wrap in the real 80-column TUI'
    exit 1
fi
quit_payload="${quit_line#*│}"
continuation_payload="${continuation_line#*│}"
quit_prefix="${quit_payload%%Quit*}"
continuation_prefix="${continuation_payload%%[! ]*}"
if [ "${#quit_prefix}" -ne "${#continuation_prefix}" ]; then
    printf '%s\n%s\n' "$quit_line" "$continuation_line" >&2
    _af_fail "#2577: wrapped description starts in the key column (description col ${#quit_prefix}, continuation col ${#continuation_prefix})"
    exit 1
fi
_af_log 'assert OK: wrapped help description uses the description column'

af_send Home
af_wait_for 'Agent Factory v' 5 'Home jumps to the top of help'

af_send C-d
af_wait_gone 'Agent Factory v' 5 'Ctrl-D pages by a viewport'

echo 'PASS: #2577 help navigation and wrapped-column alignment'
