#!/usr/bin/env bash
# Real-TUI drive for #3854 (the Accounts row says the sign-in is a device code),
# via:
#
#   scripts/testbox.sh scenario scripts/tui-3854-scenario.sh
#
# What is under test here is COPY, and copy is exactly the class of change a unit
# test can assert and still ship broken: ui's tests hold the sentence, but they
# cannot say whether an operator ever SEES it — whether the row can be reached,
# whether the purpose line renders for it, whether the added clause survives the
# pane's wrap at a real width. Those are the ways a hint turns out to be
# unreachable, and they are all visible only here.
#
# The claim: an account with no credential yet, selected in the config overlay's
# Accounts section, explains that the login is a DEVICE CODE — the pane prints a
# URL and you finish it in your own browser — rather than only that af "hands you
# the terminal", which on a headless daemon host left the operator waiting for a
# browser af has deliberately made sure will not open.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# NOT logged in on purpose: the device-code sentence belongs to the row that is
# about to be logged in, not to the one that already holds a credential.
readonly ACCOUNT='playtest-devicecode'

register_account() {
    local bin
    bin="$(_af_resolve_bin)"
    "$bin" accounts add claude "$ACCOUNT" >/dev/null
    _af_log "registered claude account: $ACCOUNT (not logged in)"
}

# select_account_row walks the config overlay's cursor down to the Accounts
# section. The section is rendered LAST — the config keys are what the overlay is
# for — so the number of rows above it depends on the manifest, and the loop
# stops on the screen rather than on a count.
select_account_row() {
    local i
    for i in $(seq 1 120); do
        if af_capture | grep -qF -- "$ACCOUNT"; then
            if af_capture | grep -qF 'device code'; then
                _af_log "account row selected after ${i} moves"
                return 0
            fi
        fi
        af_send Down
        sleep "$AF_DRIVER_POLL"
    done
    _af_log "never reached the ${ACCOUNT} row"
    printf '%s\n' "$(af_capture)" >&2
    return 1
}

drive_accounts_hint() {
    export AF_DRIVER_COLS=140 AF_DRIVER_ROWS=40
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    register_account
    af_boot
    af_open_config
    # ACCOUNTS, not Accounts: the pane renders section headings in caps.
    af_wait_for 'ACCOUNTS' "$AF_DRIVER_TIMEOUT" 'the Accounts section heading'
    select_account_row

    # The claim, on the screen an operator is looking at.
    af_assert_screen 'device code' 'the Accounts row calls the sign-in a device code'
    af_assert_screen 'prints a URL' 'the row says the pane prints a URL'
    af_assert_screen 'own browser' 'the row says you finish it in your own browser'
    # And the property the whole feature rests on is still said beside it.
    af_assert_screen 'never reads the credential' 'the row still says af never reads the credential'

    _af_log '----- Accounts section as an operator sees it -----'
    af_capture >&2
    _af_log '--------------------------------------------------'

    af_close_config
    af_assert_no_orphan_clients
}

drive_accounts_hint
_af_log 'tui-3854 scenario: PASS'
