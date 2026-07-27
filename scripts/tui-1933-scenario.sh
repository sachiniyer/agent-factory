#!/usr/bin/env bash
# Real-TUI drive for #1933 (the naming form's backend field), via:
#
#   scripts/testbox.sh scenario scripts/tui-1933-scenario.sh
#
# The behavior under test: the TUI can choose a session's BACKEND per session,
# the way `af sessions create --backend` and the web's picker already could. The
# field is the third field of the naming form (ctrl+r), and its rows come from the
# daemon's ListBackends catalog — so this scenario is also the only place that
# proves the round trip happens against a real daemon rather than a fixture.
#
# Why a driver scenario and not only unit tests: the app/ tests swap the catalog
# seam, so they prove the wiring but not that a user can REACH the field. The
# three ways a shipped capability turns out to be unreachable — an unlabeled
# affordance, a picker rendering internal names, a hint clipped away by width —
# are all invisible to a unit test and all visible here.
#
# The mock sandbox repo has no `origin` remote and no docker/ssh/remote_hooks
# config, so the catalog's honest answer is: local available, everything else
# unavailable with a reason. That makes it a good fixture for the refusal path.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# drive_backend_field is the wide-terminal pass: the hint is advertised, the field
# opens over the daemon's catalog, an unusable row is refused with the daemon's
# reason, and an explicit pick is confirmed in the status bar and reaches a create.
drive_backend_field() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    af_boot
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1
    # Discoverability: at 120 columns the status bar advertises the field. This is
    # the assertion an unreachable-but-shipped capability fails.
    af_wait_for 'ctrl\+r backend' "$AF_DRIVER_TIMEOUT" 'backend hint advertised' || return 1

    af_send C-r
    af_wait_for 'Select backend' "$AF_DRIVER_TIMEOUT" 'backend field opened' || return 1
    # The first row names the backend the repo actually resolves to, so a user can
    # see the default without picking it. A round trip that returned nothing would
    # leave this row missing.
    af_wait_for 'Repo default' "$AF_DRIVER_TIMEOUT" 'repo-default row' || return 1
    # The catalog is honest about what this repo cannot use. Without the daemon's
    # answer every row would render as usable — the promise this must not make.
    af_wait_for 'unavailable|cannot check' "$AF_DRIVER_TIMEOUT" 'an unusable row is marked' || return 1
    _af_log 'assert OK: the backend field lists the daemon catalog with per-row status'

    # Esc backs out of the FIELD, not the create.
    #
    # Wait on the overlay being GONE, never on 'submit name': the naming form's
    # status bar is painted UNDERNEATH the overlay, so a wait for it matches
    # instantly and synchronizes nothing — the next keypress would race the close
    # and land in whichever surface won. That level-vs-edge mistake is the whole
    # reason this file waits the way it does.
    af_send Escape
    af_wait_gone 'Select backend' "$AF_DRIVER_TIMEOUT" 'esc closed the field' || return 1
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'esc returns to naming' || return 1

    # Refusal path: docker is the row two below "Repo default" (local sits between
    # them, config.SupportedBackends order), and this repo has no origin remote or
    # docker.image, so the daemon reports it unusable. Picking it must state the
    # reason and leave the field unset.
    af_send C-r
    af_wait_for 'Select backend' "$AF_DRIVER_TIMEOUT" 'backend field reopened' || return 1
    af_send Down; af_send Down
    af_send Enter
    # Wait on the NOTICE, not on 'submit name': the status bar is still painted
    # under the overlay, so waiting for it would return before the refusal lands
    # and capture a screen that has not been drawn yet.
    af_wait_for 'backend=docker|docker\.image|origin' "$AF_DRIVER_TIMEOUT" \
        'refusal states the daemon reason' || return 1
    af_wait_gone 'Select backend' "$AF_DRIVER_TIMEOUT" 'refusal closed the field' || return 1
    local screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE 'backend ✓'; then
        _af_fail '#1933: a REFUSED backend was attached to the create anyway:'
        printf '%s\n' "$screen" >&2
        return 1
    fi
    _af_log 'assert OK: an unusable backend is refused with the daemon reason, nothing attached'

    # Explicit pick: local is the row directly below "Repo default" and is usable
    # here, so it must attach and the hint must confirm it.
    af_send C-r
    af_wait_for 'Select backend' "$AF_DRIVER_TIMEOUT" 'backend field reopened' || return 1
    af_send Down
    af_send Enter
    af_wait_for 'backend ✓' "$AF_DRIVER_TIMEOUT" 'hint confirms an explicit backend' || return 1
    _af_log 'assert OK: an explicit backend is attached and confirmed in the status bar'

    # And the create still completes with the backend on the request.
    af_send_literal 'picked-backend'
    af_send Enter
    af_wait_for 'picked-backend.*●' "$AF_DRIVER_TIMEOUT" "session 'picked-backend' ready" || return 1
    _af_log 'assert OK: a create with an explicit backend reaches Ready'

    # The next create starts from the repo default — no leak.
    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'second naming form' || return 1
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE 'backend ✓'; then
        _af_fail '#1933: the previous create leaked its backend into the next form:'
        printf '%s\n' "$screen" >&2
        return 1
    fi
    af_send C-c
    af_wait_gone 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form cancelled' || return 1

    echo 'PASS: #1933 backend field at 120x30'
}

# drive_backend_field_at_80_cols is the narrow-terminal pass. The hint SHEDS below
# ~93 columns (ui/menu.go hintDropOrder gives the prompt hint the last slot, per
# #1936's commitment), and that is a deliberate degradation — so the field itself
# must still work at 80 columns, and the picker must still fit on screen. A field
# that only functions when its hint is visible would make the shed a real break.
drive_backend_field_at_80_cols() {
    export AF_DRIVER_COLS=80 AF_DRIVER_ROWS=24
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    af_boot
    af_ensure_nav
    af_focus_tree

    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form' || return 1
    local screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE 'ctrl\+r backend'; then
        _af_log 'note: the backend hint fits at 80 columns after all — the shed threshold moved'
    fi
    # The prompt hint is the one #1936 promised an 80-column bar would keep.
    af_wait_for 'initial prompt' "$AF_DRIVER_TIMEOUT" 'prompt hint survives 80 cols' || return 1

    af_send C-r
    af_wait_for 'Select backend' "$AF_DRIVER_TIMEOUT" 'backend field opens at 80 cols' || return 1
    af_wait_for 'Repo default' "$AF_DRIVER_TIMEOUT" 'repo-default row at 80 cols' || return 1
    # The overlay's own bottom hint proves it painted in full inside the pane: an
    # overlay wider or taller than the terminal loses its last rows to the clamp
    # (the #1998 class). Line LENGTH is deliberately not measured here — capture
    # output is bytes, and the frame's box glyphs make every row look 80+ wide in
    # the sandbox's C locale (#1994).
    af_wait_for 'esc cancel' "$AF_DRIVER_TIMEOUT" 'picker painted in full at 80 cols' || return 1
    af_send Escape
    # Edge, not level — see the note in drive_backend_field. Without this the C-c
    # below can arrive while the overlay still owns the keyboard, where it cancels
    # the FIELD rather than the create.
    af_wait_gone 'Select backend' "$AF_DRIVER_TIMEOUT" 'esc closed the field' || return 1
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'esc returns to naming' || return 1
    af_send C-c
    af_wait_gone 'submit name' "$AF_DRIVER_TIMEOUT" 'naming form cancelled' || return 1

    echo 'PASS: #1933 backend field at 80x24 (hint shed, field still reachable)'
}

drive_backend_field
drive_backend_field_at_80_cols
af_quit || true
echo 'PASS: #1933 TUI backend field'
