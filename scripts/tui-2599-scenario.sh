#!/usr/bin/env bash
# Real-TUI drive for #2599 (the naming form provisioned the repo's backend), via:
#
#   scripts/testbox.sh scenario scripts/tui-2599-scenario.sh
#
# The bug: pressing `n` built its placeholder row through session.NewInstance,
# which RESOLVES the create's backend and provisions it. In a repo whose config
# declares backend = "docker"/"ssh"/"hook" that ran a real provisioner before the
# user had typed anything — and when the provisioner refused, the refusal came
# out INSTEAD of the naming form. Such a repo could not create a session from the
# TUI at all, by any keystroke.
#
# Why this cannot be a unit test alone, and why it cannot be written against
# master: the app/ tests swap the backend factory (session.SetBackendFactoryForTest),
# so the naming flow never reaches a real Provision under test — a green unit
# suite proves nothing here. And the assertion "the form opens" is unreachable on
# master by construction, because the form not opening IS the bug.
#
# The sandbox has no docker daemon inside it and the mock repo has no `origin`,
# so `backend = "docker"` is a config the daemon will refuse AT CREATE. That is
# the useful fixture: the refusal has to arrive from the daemon, after a naming
# form the user could type into — not from a provisioner running behind a
# keypress.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# declare_repo_backend <backend> — write the in-repo config that opts this repo
# into a non-local backend, the way docs/backends.md tells a user to (#2194).
# Removing the file is how a leg goes back to the local default.
declare_repo_backend() {
    mkdir -p "$AF_DRIVER_REPO/.agent-factory"
    printf '{"backend": "%s"}\n' "$1" >"$AF_DRIVER_REPO/.agent-factory/config.json"
}

clear_repo_backend() {
    rm -f "$AF_DRIVER_REPO/.agent-factory/config.json"
}

# drive_form_opens_in_a_backend_repo is the bug itself. `n`, field untouched, in
# a repo that declares docker: the naming form must open, and the refusal must
# come from the DAEMON at submit and must NAME docker.
#
# Both halves matter and they fail in opposite directions. If the form does not
# open, #2599 is unfixed. If the form opens and the create SUCCEEDS as a local
# session, the fix silently downgraded the repo's declared backend to local —
# which looks fixed, passes every "did the form open" check, and is worse than
# the bug, because the user gets a session that is not the one their repo asked
# for. The daemon naming docker in its refusal is what rules that out.
drive_form_opens_in_a_backend_repo() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    declare_repo_backend docker
    af_boot
    af_ensure_nav
    af_focus_tree

    af_send n
    # THE assertion. On master this times out: the keypress is answered by a
    # BackendConfigError thrown from inside dockerRuntime.Provision and the form
    # never exists.
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" \
        'naming form opens in a docker-declared repo' || return 1

    # And no provisioner ran behind the keypress: the config error the docker
    # runtime raises must not be on screen while we are still naming.
    local screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE 'docker\.image|requires docker'; then
        _af_fail '#2599: a provisioner refused during naming — the placeholder was provisioned:'
        printf '%s\n' "$screen" >&2
        return 1
    fi
    _af_log 'assert OK: n opens the naming form in a repo declaring a non-local backend'

    # Submit. The repo's declared backend has to survive to the daemon, which is
    # the only thing that provisions (#960).
    af_send_literal 'declared-docker'
    af_send Enter
    af_wait_for 'docker' "$AF_DRIVER_TIMEOUT" \
        'the daemon refuses the create naming the declared backend' || return 1
    screen="$(af_capture)"
    if ! printf '%s\n' "$screen" | grep -qE 'docker\.image|requires docker|backend=docker'; then
        _af_fail '#2599: the create was not refused for docker — the declared backend was downgraded to local:'
        printf '%s\n' "$screen" >&2
        return 1
    fi
    _af_log 'assert OK: the create still honors the repo backend — refused by the daemon, naming docker'

    echo 'PASS: #2599 naming form opens in a backend-declaring repo'
}

# drive_create_completes_from_a_backend_repo answers the user-facing complaint:
# "a docker/ssh/hook repo cannot create a session from the TUI at all". Opening
# the form is necessary but not sufficient — the user has to be able to finish.
#
# The sandbox cannot run a container, so the create that completes is one the
# user explicitly points at `local` in the ctrl+r field (#1933). That is the real
# escape hatch this unblocks, and it also re-proves the field's precedence: an
# explicit pick outranks the repo's key, in a repo where the key is not local.
drive_create_completes_from_a_backend_repo() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    declare_repo_backend docker
    af_boot
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1

    af_send C-r
    af_wait_for 'Select backend' "$AF_DRIVER_TIMEOUT" 'backend field opened' || return 1
    # "local" is the row directly below "Repo default"; the repo default here is
    # docker, which this sandbox cannot satisfy.
    af_send Down
    af_send Enter
    af_wait_for 'backend ✓' "$AF_DRIVER_TIMEOUT" 'explicit local pick attached' || return 1
    # Edge, not level: the naming status bar is painted UNDER the overlay, so a
    # wait on 'submit name' would match while the picker still owns the keyboard
    # and the title below would be typed into it (#1933's scenario learned this
    # the expensive way).
    af_wait_gone 'Select backend' "$AF_DRIVER_TIMEOUT" 'field closed' || return 1

    af_send_literal 'escaped-to-local'
    af_send Enter
    af_wait_for 'escaped-to-local.*●' "$AF_DRIVER_TIMEOUT" \
        "session 'escaped-to-local' reaches Ready" || return 1
    _af_log 'assert OK: a session can be created from the TUI in a repo declaring a non-local backend'

    echo 'PASS: #2599 create completes from a backend-declaring repo'
}

# drive_local_repo_unchanged is the no-regression leg. Everything above changes
# how the placeholder is built for EVERY create, so the ordinary path — a repo
# with no `backend` key, `n`, type, enter — has to be exactly what it was.
drive_local_repo_unchanged() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    clear_repo_backend
    af_boot
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1
    af_send_literal 'plain-local'
    af_send Enter
    af_wait_for 'plain-local.*●' "$AF_DRIVER_TIMEOUT" "session 'plain-local' reaches Ready" || return 1
    _af_log 'assert OK: the ordinary local create is unchanged'

    echo 'PASS: #2599 local repo unchanged'
}

drive_form_opens_in_a_backend_repo
drive_create_completes_from_a_backend_repo
drive_local_repo_unchanged
clear_repo_backend
af_quit || true
echo 'PASS: #2599 TUI naming form does not provision'
