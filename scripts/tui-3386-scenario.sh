#!/usr/bin/env bash
# Real-TUI drive for #3386 (a project defaults to a credential account), via:
#
#   scripts/testbox.sh scenario scripts/tui-3386-scenario.sh
#
# The behavior under test: with `default_accounts.claude` set for a project, the
# naming form PRESELECTS that account — visibly, before the user touches the
# account field — instead of the daemon filling it in silently on the create. The
# issue's complaint is the silence, so "the session ends up scoped" is not the
# property; "the user can see and change it first" is.
#
# Why a driver scenario and not only unit tests: app/account_default_test.go swaps
# the registry seam, so it proves the model applies a default it is handed. It
# cannot prove that the daemon COMPUTED one from a project's config file, that the
# answer crossed the wire, or that a user looking at the form would know. Each of
# those is invisible to a unit test and visible here — and the first `account ✓`
# below, appearing with no keypress, is the whole feature in one assertion.
#
# WHAT THIS SANDBOX CAN AND CANNOT SHOW. The play-test config points `claude` at a
# bash stand-in (scripts/container/configure-playtest-agent.sh), so an
# account-scoped create is REFUSED here by the account boundary: it will not launch
# an agent whose command it cannot prove is a direct invocation of that agent.
# That is turned into the end-to-end evidence rather than worked around, exactly as
# scripts/tui-3844-scenario.sh does it: the refusal comes from the DAEMON and NAMES
# THE ACCOUNT, which is only possible if a value nobody typed was resolved from the
# project's config, applied to the create, and carried across the wire. A session
# that actually RUNS as the account needs a real agent binary; that is the manual
# leg in docs/dev/tui-manual-testing.md.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# PROJECT_ACCOUNT is registered and holds a credential; MISSING_ACCOUNT is named
# by config and never registered, which is the misconfiguration the picker has to
# SHOW rather than hide behind an "Ambient identity" row that would be a lie.
#
# The credential placeholder is an EMPTY-of-secrets `{}` at the path claude's own
# login would write, because logged-in state is a stat of that path and no agent
# runs in this sandbox to create one. af never opens the file (see
# internal/agentaccount/login.go LoggedIn), so this is not credential material.
readonly PROJECT_ACCOUNT='playtest-project'
readonly MISSING_ACCOUNT='playtest-gone'

register_accounts() {
    local bin; bin="$(_af_resolve_bin)"
    "$bin" accounts add claude "$PROJECT_ACCOUNT" >/dev/null
    printf '{}\n' >"$AGENT_FACTORY_HOME/accounts/claude/$PROJECT_ACCOUNT/.credentials.json"
    _af_log "registered claude account: $PROJECT_ACCOUNT (logged in)"
}

# set_project_default writes the key through af's OWN verbs — `af projects
# register` then `af config set --project` — rather than by hand-writing the
# project's config.toml. That matters: a hand-written file would prove the TUI
# renders a value, while this proves the documented gesture in
# docs/usage-limits.md produces one. It runs AFTER af_boot because registration is
# daemon-routed; the config write is local, and both are read fresh on the next
# ListAccounts, so no restart is needed (default_accounts is EffectAppliedLive).
set_project_default() {
    local account="$1"
    local bin; bin="$(_af_resolve_bin)"
    "$bin" projects register "$AF_DRIVER_REPO" >/dev/null
    # stderr is kept: for an unregistered account this WARNS, and the warning is
    # part of what #3386 promises at the command that takes the value.
    "$bin" config set default_accounts.claude "$account" --project "$AF_DRIVER_REPO" >/dev/null
    _af_log "project default set: default_accounts.claude = $account"
}

# drive_project_default is the headline: a registered account configured for this
# project is preselected, labelled, changeable, and reaches the daemon.
drive_project_default() {
    export AF_DRIVER_COLS=140 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    register_accounts
    af_boot
    set_project_default "$PROJECT_ACCOUNT"
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1

    # THE ASSERTION THIS SCENARIO EXISTS FOR. No ctrl+o, no pick, no keypress
    # beyond `n`: the status bar confirms a non-ambient identity is attached
    # because the daemon resolved the project's config and the form applied it.
    # Before #3386 this could only appear after a manual pick, so a regression
    # that reverts to "the daemon fills it in silently" fails right here.
    af_wait_for 'account ✓' "$AF_DRIVER_TIMEOUT" \
        'the project default is preselected with no keypress' || return 1
    _af_log 'assert OK: the naming form preselects the project default account'

    # And it is VISIBLE as the project's choice rather than an arbitrary pick.
    af_send C-o
    af_wait_for 'Select claude account' "$AF_DRIVER_TIMEOUT" 'account field opened' || return 1
    af_wait_for "$PROJECT_ACCOUNT.*project default" "$AF_DRIVER_TIMEOUT" \
        'the configured account is listed and labelled "project default"' || return 1
    _af_log 'assert OK: the default row says WHY it is selected'

    af_send Escape
    af_wait_gone 'Select claude account' "$AF_DRIVER_TIMEOUT" 'esc closed the field' || return 1
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'esc returns to naming' || return 1

    # An account belongs to ONE agent. Changing the program must drop a claude
    # default rather than carry the name into codex's registry, where the same
    # spelling is a different identity or none at all.
    af_send Tab
    af_wait_for 'Select program' "$AF_DRIVER_TIMEOUT" 'program field opened' || return 1
    af_send Down
    af_send Enter
    af_wait_gone 'Select program' "$AF_DRIVER_TIMEOUT" 'program field closed' || return 1
    af_wait_gone 'account ✓' "$AF_DRIVER_TIMEOUT" \
        'changing the program drops the claude default' || return 1
    _af_log 'assert OK: a program change clears the previous agent default'

    # Back to claude: the default returns, which proves the re-fetch is per agent
    # rather than a one-shot on form open.
    af_send Tab
    af_wait_for 'Select program' "$AF_DRIVER_TIMEOUT" 'program field reopened' || return 1
    af_send Up
    af_send Enter
    af_wait_gone 'Select program' "$AF_DRIVER_TIMEOUT" 'program field closed' || return 1
    af_wait_for 'account ✓' "$AF_DRIVER_TIMEOUT" \
        'returning to claude restores its project default' || return 1
    _af_log 'assert OK: the default follows the program, per agent'

    # The create carries it. The stand-in agent makes the account boundary refuse,
    # and the refusal NAMES THE ACCOUNT — which is only possible if a value nobody
    # typed came from the project config and rode CreateSessionRequest.Account.
    af_send_literal 'defaulted-create'
    af_send Enter
    af_wait_for "account \"$PROJECT_ACCOUNT\"" "$AF_DRIVER_TIMEOUT" \
        'the daemon answered about the account the project configured' || return 1
    _af_log 'assert OK: the project default reached the daemon on the create'

    echo "PASS: #3386 project default preselected at 140x30"
}

# drive_unregistered_default is the misconfiguration case, and it is the one a
# user is most likely to meet: a project configured before the account exists, or
# one deleted since. The picker must SHOW it — hiding it would leave the form
# reporting the ambient identity while the config says otherwise, which is the
# same silence the feature exists to end.
drive_unregistered_default() {
    export AF_DRIVER_COLS=140 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    register_accounts
    af_boot
    set_project_default "$MISSING_ACCOUNT"
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1

    af_send C-o
    af_wait_for 'Select claude account' "$AF_DRIVER_TIMEOUT" 'account field opened' || return 1
    # Offered, appended last, and labelled with BOTH facts: it is the project's
    # choice, and it is not registered. A picker that dropped the row would be
    # hiding the create's coming refusal.
    af_wait_for "$MISSING_ACCOUNT.*project default.*not registered" "$AF_DRIVER_TIMEOUT" \
        'an unregistered project default is offered and labelled' || return 1
    # The registered account is still listed and is NOT marked as the default,
    # which is the control: the label follows the config, not the row order.
    af_wait_for "$PROJECT_ACCOUNT" "$AF_DRIVER_TIMEOUT" 'the registered account is still offered' || return 1
    local screen; screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE "$PROJECT_ACCOUNT.*project default"; then
        _af_fail '#3386: the "project default" label is on an account the config did not name:'
        printf '%s\n' "$screen" >&2
        return 1
    fi
    _af_log 'assert OK: the label marks the configured account and only that one'

    af_send Escape
    af_wait_gone 'Select claude account' "$AF_DRIVER_TIMEOUT" 'esc closed the field' || return 1
    af_send C-c
    af_wait_gone 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form cancelled' || return 1

    echo "PASS: #3386 unregistered project default is shown, not hidden"
}

drive_project_default
drive_unregistered_default
af_quit || true
echo 'PASS: #3386 TUI project default account'
