#!/usr/bin/env bash
# Real-TUI drive for #2413: dismissing the first-run interactive help with the
# very key that help names (ctrl+]) must leave the user IN interactive mode.
#
# Runs inside the #1130 container sandbox:
#   scripts/testbox.sh scenario scripts/tui-2413-scenario.sh
#
# AF_DRIVER_HELP_SEEN=7 clears the interactive bit (1<<3) so the first-run
# overlay actually appears — every other one-time overlay stays suppressed, so
# the scenario is still deterministic. af_reset_sandbox between geometries wipes
# state.json, so the overlay is genuinely "first run" both times.
set -euo pipefail

export AF_DRIVER_HELP_SEEN=7

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

run_at() {
    export AF_DRIVER_COLS="$1" AF_DRIVER_ROWS="$2"

    af_reset_sandbox
    af_boot
    af_new_instance alpha
    af_select alpha
    af_open_pane

    # Enter on the focused pane surfaces the first-run interactive help. Sync on
    # the overlay's own title so we know we are looking at it, not a raced frame.
    #
    # Deliberately NOT synced on the nav menu's `↵ interact` hint first: that
    # hint is optional and the menu's width clamp sheds it at 80 columns, so
    # waiting on it would fail the narrow geometry for a reason that has nothing
    # to do with this fix. af_open_pane already synced on `x hide pane`.
    af_send Enter
    af_wait_for 'Interactive pane' "$AF_DRIVER_TIMEOUT" 'first-run interactive help'
    af_wait_for 'return to navigation' "$AF_DRIVER_TIMEOUT" 'the help line that names ctrl+]'

    # The user does what the screen just told them to do.
    af_send 'C-]'
    af_wait_gone 'Interactive pane' "$AF_DRIVER_TIMEOUT" 'help overlay dismissed'

    # THE ASSERTION. The interactive menu shows only `ctrl+] nav mode`, so its
    # presence means the mode is live. Before #2413 the replayed ctrl+] exited
    # immediately: the pane frame dropped back to a single border and the full
    # nav menu returned, with the user never having typed anything.
    af_wait_for 'nav mode' "$AF_DRIVER_TIMEOUT" 'STILL in interactive mode after the ctrl+] dismiss'

    # The mode is genuinely usable, not merely flagged: type into the pane and
    # see the shell answer.
    af_send_to_pane 'echo af2413ok'
    af_wait_for 'af2413ok' "$AF_DRIVER_TIMEOUT" 'the pane actually received input'

    # And ctrl+] still exits, exactly as the help promised — the fix drops the
    # synthetic replay, never the key.
    af_exit_interactive

    echo "PASS: #2413 scenario at ${AF_DRIVER_COLS}x${AF_DRIVER_ROWS}"
}

run_at 100 30
run_at 80 24

echo "PASS: #2413 real-TUI scenario at both geometries"
