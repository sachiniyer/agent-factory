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

printf '=== tui-driver self-test (#1161) ===\n'
printf 'session=%s size=%sx%s home=%s\n' \
    "$AF_DRIVER_SESSION" "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS" "$AGENT_FACTORY_HOME"

# Start from a clean slate so the run is deterministic even in a reused
# container (scoped to the sandbox; fails closed on a non-sandbox home).
step "reset sandbox to a clean state"                       af_reset_sandbox
step "boot af at ${AF_DRIVER_COLS}x${AF_DRIVER_ROWS}"        af_boot
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
step "enter interactive mode"                               af_enter_interactive
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
