#!/usr/bin/env bash
# Real-TUI drive for #3756: a title that DERIVES the reserved root name must be
# refused in the naming overlay, with the user still standing in it — not
# accepted here and refused a round trip later by the daemon.
#
# Runs inside the #1130 container sandbox:
#   scripts/testbox.sh scenario scripts/tui-3756-scenario.sh
#
# The interesting title is "ro ot": toTmuxName DELETES interior whitespace, so
# it derives the same tmux session name as "root" (#3732), while the overlay's
# old pre-check only TRIMMED whitespace and let it through.
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
af_wait_gone '[Ss]ession [Nn]ame:' 1 'old name prompt label'

# The title a user could plausibly type, and which used to sail through.
af_send_literal 'ro ot'
af_wait_for 'ro ot' 5 'the typed title renders in the naming row'
af_send Enter

# THE ASSERTION: ONE frame carrying BOTH facts. Neither alone is evidence.
#
# The refusal message is not, because the DAEMON refuses this title too (#3732)
# and the TUI shows whatever it says — on the unfixed build the message appears
# just the same, a round trip later, with the naming flow already closed behind
# it. Measured, not assumed: run this scenario against the unfixed overlay and
# it gets past the message and dies on the overlay being gone.
#
# The overlay staying open is not, because an unrelated refusal keeps it open
# too (a missing agent binary at preflight is one).
#
# Together they are the fix: the user is still standing in the naming form,
# reading why the name was refused, able to correct it in place.
_af3756_frame=""
_af3756_deadline=$(( $(date +%s) + AF_DRIVER_TIMEOUT ))
while :; do
    _af3756_frame="$(af_capture)"
    if printf '%s\n' "$_af3756_frame" | grep -q 'reserved for the daemon-managed root agent' &&
        printf '%s\n' "$_af3756_frame" | grep -q 'submit name'; then
        break
    fi
    if [ "$(date +%s)" -ge "$_af3756_deadline" ]; then
        echo "TIMEOUT: no single frame showed both the refusal and the open naming overlay" >&2
        printf '%s\n' "$_af3756_frame" >&2
        exit 1
    fi
    sleep 0.2
done

# Print what a human would be looking at. The `play-tested` label is a claim
# that someone drove the TUI and read the screen, so the screen goes in the
# transcript rather than only the greps that matched it.
echo '----- screen: refusal, with the naming overlay still up -----'
printf '%s\n' "$_af3756_frame"
echo '-------------------------------------------------------------'

# It is genuinely editable: correct the name in place and the create proceeds.
af_send BSpace BSpace BSpace BSpace BSpace
af_send_literal 'rooted'
af_send Enter
af_wait_for 'rooted.*●' "$AF_DRIVER_TIMEOUT" 'a title with a distinct derived name still creates'
echo '----- screen: the corrected title created in place -----'
af_capture
echo '--------------------------------------------------------'

# The neighbouring rule is unchanged: the reserved SPELLING is still refused,
# and refused in the overlay too.
af_ensure_nav
af_focus_tree
af_send n
af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form opens again'
af_send_literal 'Root'
af_send Enter
af_wait_for 'reserved for the daemon-managed root agent' "$AF_DRIVER_TIMEOUT" \
    'the reserved spelling is still refused in the overlay'

echo 'PASS: #3756 the naming overlay refuses a derived-name collision, in place'
