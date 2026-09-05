#!/usr/bin/env bash
# Real-TUI drive for #3844 (the naming form's account field), via:
#
#   scripts/testbox.sh scenario scripts/tui-3844-scenario.sh
#
# The behavior under test: the TUI can scope a session to a registered credential
# ACCOUNT, the way `af sessions create --account` already could. The field is the
# fourth field of the naming form (ctrl+o), and its rows come from the daemon's
# ListAccounts registry — so this scenario is the only place that proves the round
# trip happens against a real daemon rather than a fixture.
#
# Why a driver scenario and not only unit tests: the app/ tests swap the registry
# seam, so they prove the wiring but not that a user can REACH the field. The ways
# a shipped capability turns out to be unreachable — an unlabeled affordance, a
# picker rendering internal names, a hint clipped away by width — are all invisible
# to a unit test and all visible here.
#
# WHAT THIS SANDBOX CAN AND CANNOT SHOW. The play-test config points `claude` at a
# bash stand-in (scripts/container/configure-playtest-agent.sh), so an
# account-scoped create is REFUSED here by the account boundary: it will not launch
# an agent whose command it cannot prove is a direct invocation of that agent. That
# is not a limitation to work around — it is turned into the end-to-end evidence
# below, because the refusal comes from the DAEMON and NAMES THE ACCOUNT, which is
# only possible if the value the picker attached crossed the wire on
# CreateSessionRequest.Account. A session that actually runs as the account needs a
# real agent binary; that is the manual leg in docs/dev/tui-manual-testing.md.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# The two account states the picker has to render differently. Both are made with
# af's own verb; the only hand-made thing is an EMPTY-of-secrets placeholder at the
# path claude's login would write, because logged-in state is a stat of that path
# and no agent runs in this sandbox to create one. `{}` is not credential material
# — af never opens the file, it only asks whether a non-empty regular file is
# there (internal/agentaccount/login.go LoggedIn).
readonly SIGNED_IN_ACCOUNT='playtest-in'
readonly REGISTERED_ACCOUNT='playtest-out'

register_accounts() {
    local bin; bin="$(_af_resolve_bin)"
    "$bin" accounts add claude "$SIGNED_IN_ACCOUNT" >/dev/null
    "$bin" accounts add claude "$REGISTERED_ACCOUNT" >/dev/null
    printf '{}\n' >"$AGENT_FACTORY_HOME/accounts/claude/$SIGNED_IN_ACCOUNT/.credentials.json"
    _af_log "registered claude accounts: $SIGNED_IN_ACCOUNT (logged in), $REGISTERED_ACCOUNT (not)"
}

# drive_account_field is the wide-terminal pass: the hint is advertised, the field
# opens over the daemon's registry with both account states rendered, a pick is
# confirmed in the status bar, a program change drops it, and the account reaches
# the daemon on the create.
drive_account_field() {
    export AF_DRIVER_COLS=140 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    register_accounts
    af_boot
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1
    # Discoverability: at 140 columns the status bar advertises the field. This is
    # the assertion an unreachable-but-shipped capability fails.
    af_wait_for 'ctrl\+o account' "$AF_DRIVER_TIMEOUT" 'account hint advertised' || return 1

    af_send C-o
    # The title names the AGENT, which is the one property a create-time picker
    # owes the user: this list is scoped to the program on the form.
    af_wait_for 'Select claude account' "$AF_DRIVER_TIMEOUT" 'account field opened' || return 1
    af_wait_for 'Ambient identity' "$AF_DRIVER_TIMEOUT" 'ambient row is first' || return 1
    af_wait_for "$SIGNED_IN_ACCOUNT" "$AF_DRIVER_TIMEOUT" 'a registered account is listed' || return 1
    # Constraint 3: a not-logged-in account is LISTED and says so. A round trip that
    # returned nothing, or one that hid the row, fails here.
    af_wait_for "$REGISTERED_ACCOUNT.*not logged in" "$AF_DRIVER_TIMEOUT" \
        'a not-logged-in account is listed and labelled' || return 1
    _af_log 'assert OK: the account field lists the daemon registry with per-row state'

    # Esc backs out of the FIELD, not the create. Wait on the overlay being GONE,
    # never on 'submit name': the naming form's status bar is painted UNDERNEATH the
    # overlay, so a wait for it matches instantly and synchronizes nothing.
    af_send Escape
    af_wait_gone 'Select claude account' "$AF_DRIVER_TIMEOUT" 'esc closed the field' || return 1
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'esc returns to naming' || return 1

    # Pick the first real account (the row below Ambient identity) and confirm the
    # status bar says a non-ambient identity is attached.
    af_send C-o
    af_wait_for 'Select claude account' "$AF_DRIVER_TIMEOUT" 'account field reopened' || return 1
    af_send Down
    af_send Enter
    af_wait_for 'account ✓' "$AF_DRIVER_TIMEOUT" 'hint confirms an attached account' || return 1
    _af_log 'assert OK: an account is attached and confirmed in the status bar'

    # An account belongs to ONE agent, so changing the program must drop the pick.
    # Keeping it would send a claude account name into codex's registry, where the
    # same spelling is a different identity or none at all.
    af_send Tab
    af_wait_for 'Select program' "$AF_DRIVER_TIMEOUT" 'program field opened' || return 1
    af_send Down
    af_send Enter
    af_wait_gone 'Select program' "$AF_DRIVER_TIMEOUT" 'program field closed' || return 1
    local screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE 'account ✓'; then
        _af_fail '#3844: a program change kept the previous agent'"'"'s account attached:'
        printf '%s\n' "$screen" >&2
        return 1
    fi
    _af_log 'assert OK: changing the program drops the account'

    # And the reopened field follows the NEW program: codex has no registered
    # accounts here, so the honest answer is that there is nothing to pick.
    af_send C-o
    af_wait_for 'no codex accounts are registered' "$AF_DRIVER_TIMEOUT" \
        'the field follows the program' || return 1
    _af_log 'assert OK: the list follows the program the form currently has'

    # Back to claude, re-attach, and submit. The account boundary refuses a
    # stand-in program — and the refusal is the evidence: it comes from the daemon
    # and it names the account, which is only possible if the picked value rode
    # CreateSessionRequest.Account across the wire.
    af_send Tab
    af_wait_for 'Select program' "$AF_DRIVER_TIMEOUT" 'program field opened' || return 1
    af_send Up
    af_send Enter
    af_wait_gone 'Select program' "$AF_DRIVER_TIMEOUT" 'program field closed' || return 1
    af_send C-o
    af_wait_for 'Select claude account' "$AF_DRIVER_TIMEOUT" 'account field reopened' || return 1
    af_send Down
    af_send Enter
    af_wait_for 'account ✓' "$AF_DRIVER_TIMEOUT" 'account re-attached' || return 1
    af_send_literal 'scoped-create'
    af_send Enter
    af_wait_for "account \"$SIGNED_IN_ACCOUNT\"" "$AF_DRIVER_TIMEOUT" \
        'the daemon answered about the account it was sent' || return 1
    _af_log 'assert OK: the picked account reached the daemon on the create request'

    # The next create starts on the ambient identity — no leak.
    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'second naming form' || return 1
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE 'account ✓'; then
        _af_fail '#3844: the previous create leaked its account into the next form:'
        printf '%s\n' "$screen" >&2
        return 1
    fi
    af_send C-c
    af_wait_gone 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form cancelled' || return 1

    echo 'PASS: #3844 account field at 140x30'
}

# drive_account_field_at_80_cols is the narrow-terminal pass. The account hint
# sheds FIRST (ui/menu.go hintDropOrder), which is a deliberate degradation — so
# the field itself must still work at 80 columns and the picker must still fit on
# screen. A field that only functions when its hint is visible would make the shed
# a real break.
drive_account_field_at_80_cols() {
    export AF_DRIVER_COLS=80 AF_DRIVER_ROWS=24
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    register_accounts
    af_boot
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1
    local screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE 'ctrl\+o account'; then
        _af_log 'note: the account hint fits at 80 columns after all — the shed threshold moved'
    fi
    # The prompt hint is the one #1936 promised an 80-column bar would keep, and
    # #3844 must not have taken it.
    af_wait_for 'initial prompt' "$AF_DRIVER_TIMEOUT" 'prompt hint survives 80 cols' || return 1

    af_send C-o
    af_wait_for 'Select claude account' "$AF_DRIVER_TIMEOUT" 'account field opens at 80 cols' || return 1
    af_wait_for 'Ambient identity' "$AF_DRIVER_TIMEOUT" 'ambient row at 80 cols' || return 1
    # The overlay's own bottom hint proves it painted in full inside the pane: an
    # overlay wider or taller than the terminal loses its last rows to the clamp
    # (the #1998 class).
    af_wait_for 'esc cancel' "$AF_DRIVER_TIMEOUT" 'picker painted in full at 80 cols' || return 1
    af_send Escape
    af_wait_gone 'Select claude account' "$AF_DRIVER_TIMEOUT" 'esc closed the field' || return 1
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'esc returns to naming' || return 1
    af_send C-c
    af_wait_gone 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form cancelled' || return 1

    echo 'PASS: #3844 account field at 80x24 (hint shed, field still reachable)'
}

drive_account_field
drive_account_field_at_80_cols
af_quit || true
echo 'PASS: #3844 TUI account field'
