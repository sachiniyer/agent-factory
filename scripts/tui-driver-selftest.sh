#!/usr/bin/env bash
# tui-driver-selftest.sh — the acceptance proof for the #1161 TUI driver.
#
# Runs the exact scenario that failed in #1156 — and it is now a handful of
# self-synchronizing lines instead of a hand-rolled tmux harness:
#
#   boot → create TWO instances → select each (assert selection) →
#   open a pane → enter interactive → type into the pane → exit →
#   attach full-screen → detach → assert selection preserved →
#   assert no orphan attach clients.
#
# It is BOTH the acceptance proof and the bitrot guard. Run it in the
# container sandbox:
#
#   make tui-driver-selftest
#   # or directly against a running sandbox (name is unique per run, #1171):
#   docker exec "$AF_PLAYTEST_NAME" bash /src/scripts/tui-driver-selftest.sh
#
# Exit status: 0 = every step green; non-zero = the first failed step (with
# the offending screen dumped to stderr by the driver).

set -uo pipefail

# Resolve the driver next to this script (works from /src in the container).
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/tui-driver.sh
source "$SELF_DIR/tui-driver.sh"

PASS=0
FAIL=0

# step <description> <command...> — run a driver step, tally, and abort the
# whole run on the first failure (a broken premise makes later steps noise).
step() {
    local desc="$1"; shift
    printf '\n>>> %s\n' "$desc"
    if "$@"; then
        printf '    ✓ %s\n' "$desc"
        PASS=$((PASS + 1))
    else
        printf '    ✗ FAILED: %s\n' "$desc"
        FAIL=$((FAIL + 1))
        printf '\n=== SELF-TEST FAILED at: %s ===\n' "$desc"
        printf 'passed %d step(s) before the failure.\n' "$PASS"
        exit 1
    fi
}

# _expect_resize_rejected — the NEGATIVE check for af_resize (Greptile, #1201):
# a resize tmux cannot honor must FAIL LOUDLY, never masquerade as success (or a
# tiny-size gate would keep running at the wrong size). We point af_resize at a
# session that does not exist; it must return non-zero. AF_DRIVER_SESSION is
# saved/restored so the rest of the run is unaffected; af_resize leaves
# AF_DRIVER_COLS/ROWS untouched because it returns before recording them on a
# failed resize.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_resize_rejected() {
    local saved="$AF_DRIVER_SESSION" rc=0
    AF_DRIVER_SESSION="no-such-driver-session-selftest"
    af_resize 60 15 >/dev/null 2>&1 || rc=$?
    AF_DRIVER_SESSION="$saved"
    if [ "$rc" -eq 0 ]; then
        _af_log "af_resize wrongly SUCCEEDED on a missing session (should fail loudly)"
        return 1
    fi
    return 0
}

# _expect_wrapped_send_no_timeout — regression proof for #1287. At a narrow
# width the echoed command wraps inside the framed pane; af_send_to_pane must
# still confirm it without burning the old 8s false timeout.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_wrapped_send_no_timeout() {
    local saved_cols="$AF_DRIVER_COLS" saved_rows="$AF_DRIVER_ROWS"
    local started elapsed rc=0
    # shellcheck disable=SC2016
    local long_cmd='printf "%s\n" "wrap-check-abcdefghijklmnopqrstuvwxyz-0123456789-abcdefghijklmnopqrstuvwxyz-0123456789"; echo WRAP_SEND_DONE_$((1200+87))'

    af_resize 60 18 || return 1
    af_wait_for 'nav mode' 10 'interactive mode after narrow resize' || rc=$?
    if [ "$rc" -eq 0 ]; then
        started="$(_af_now)"
        af_send_to_pane "$long_cmd" || rc=$?
        elapsed=$(( $(_af_now) - started ))
        if [ "$elapsed" -ge 7 ]; then
            _af_log "wrapped command echo confirmation took ${elapsed}s; likely hit the old false timeout"
            rc=1
        fi
        af_wait_for 'WRAP_SEND_DONE_1287([^0-9]|$)' 10 'wrapped command output' || rc=$?
    fi

    af_resize "$saved_cols" "$saved_rows" || return 1
    return "$rc"
}

# _enter_interactive_and_probe_literal_send — regression proof for #1504. The
# literal sender used to pass the text as tmux command arguments, so leading
# hyphens were parsed as flags and repeated semicolons were parsed as command
# separators. Type but do not run the probe, then clear the shell line.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_enter_interactive_and_probe_literal_send() {
    local text='- item ;; literal-send-1504'
    af_enter_interactive || return 1
    af_send_literal "$text" || return 1
    _af_wait_for_pane_echo "$text" 8 "literal send preserves leading hyphen and ;;" || return 1
    af_send C-u
    sleep "$AF_DRIVER_POLL"
}

printf '=== tui-driver self-test (#1161) ===\n'
printf 'session=%s size=%sx%s home=%s\n' \
    "$AF_DRIVER_SESSION" "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS" "$AGENT_FACTORY_HOME"

# Start from a clean slate so the run is deterministic even in a reused
# container (scoped to the sandbox; fails closed on a non-sandbox home).
step "reset sandbox to a clean state"                       af_reset_sandbox
# af_boot routes launch geometry through af_resize, which verifies the window
# actually took the requested size — so a green boot is also positive proof
# af_resize works (#1174 item 2 / #1201).
step "boot af at ${AF_DRIVER_COLS}x${AF_DRIVER_ROWS}"        af_boot
step "af_resize fails loudly on an impossible resize"       _expect_resize_rejected
step "create instance 'alpha'"                              af_new_instance alpha

# --- #1174 item 1 / #1199 regression: the SINGLE-instance false positive ---
# With exactly one instance the row auto-display-selects (sticky ▾) while the
# tree cursor still sits on the section header — so GetSelectedInstance() is
# nil and cursor-driven verbs (o/D) silently NO-OP even though the row LOOKS
# selected. The old af_select returned on iteration 0 (▾ present) and a
# play-test could wrongly "pass". af_select now lands the cursor ON the row, so
# `o attach` MUST actually fire here. If the bug returns, either af_select
# fails (no 'D kill') or af_attach times out waiting for the chrome to vanish.
step "select the SOLE instance alpha"                       af_select alpha
step "assert alpha display-selected (sole instance)"        af_expect_selected alpha
step "attach the SOLE instance (proves 'o' is NOT a no-op)" af_attach
step "detach from the single-instance attach"              af_detach
step "assert alpha survived the single-instance round trip"  af_expect_selected alpha

step "create instance 'beta'"                               af_new_instance beta

# The #1156 failure, now two lines each.
step "select alpha"                                         af_select alpha
step "assert alpha is selected"                             af_expect_selected alpha
step "select beta"                                          af_select beta
step "assert beta is selected"                              af_expect_selected beta

step "open beta's tab as a pane"                            af_open_pane
step "enter interactive mode and probe literal send"         _enter_interactive_and_probe_literal_send
step "send a wrapped command without echo false-timeout"     _expect_wrapped_send_no_timeout
# The command COMPUTES its marker (arithmetic expansion), so the sentinel we
# assert on — SELFTEST_42 — appears ONLY in the command's output, never in the
# echoed input line (which shows the literal $((6*7))). A shell that echoed
# input but failed to RUN it would therefore fail this step (Greptile P2).
# Single quotes are REQUIRED: the arithmetic must reach the pane's shell
# unexpanded, not be computed by this script.
# shellcheck disable=SC2016
step "type a command into the pane"                         af_send_to_pane 'echo SELFTEST_$((6*7))'
step "assert the pane RAN it (computed marker, not input echo)" \
    af_wait_for 'SELFTEST_42([^0-9]|$)' 10 'computed pane output'
step "exit interactive mode"                                af_exit_interactive

step "attach full-screen"                                   af_attach
step "detach (and prove the attach client was reaped)"      af_detach

step "assert selection survived attach/detach"              af_expect_selected beta
step "assert no orphan tmux attach clients"                 af_assert_no_orphan_clients

printf '\n=== SELF-TEST PASSED — %d/%d steps green ===\n' "$PASS" "$PASS"
printf 'the #1156 mis-drive is now a deterministic scenario.\n'

# Leave the sandbox as we found it (best-effort; container teardown is the
# real cleanup).
af_quit >/dev/null 2>&1 || true
exit 0
