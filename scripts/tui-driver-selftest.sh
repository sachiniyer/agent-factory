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

# _expect_multipane_hide — regression proof for #1822. Hiding a pane while
# ANOTHER pane is still open advances focus to the pane that takes its slot, NOT
# back to the tree; af_hide_pane waited for the tree menu (`n new`) and so
# false-timed-out on the first `x` of every multi-pane flow even though the pane
# was hidden correctly.
#
# The multi-pane state needs no wide terminal: below MultiPaneMinWidth (110
# cols) only ONE pane is visible at a time, so at the driver's default 100x30
# the second pane is merely auto-hidden — still open, and it takes focus on the
# next `x`. Two af_open_pane calls reach that state deterministically (the `S`
# split verb in the #1822 report is just one of several ways in; it additionally
# depends on a live preview transaction, so it is the wrong lever for a
# regression gate).
#
# Both flows are proven here: hide #1 is the multi-pane case (a pane remains),
# hide #2 is the single-pane case (the workspace empties and the tree takes
# focus back).
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_multipane_hide() {
    af_select alpha || return 1
    af_open_pane || return 1
    af_select beta || return 1
    af_open_pane || return 1

    # #1822 proper: a pane remains, so focus advances to it.
    af_hide_pane || return 1
    # Assert the PREMISE actually held — a pane is still focused. If the tree
    # menu is up here, only one pane was open and this step would be silently
    # re-testing the single-pane path that already worked.
    if af_capture | grep -qE 'n new'; then
        _af_log "#1822: tree menu is up after hiding one of TWO panes — premise broken"
        return 1
    fi

    # Single-pane case: the last pane leaves, focus returns to the tree.
    af_hide_pane || return 1
    af_assert_screen 'n new' 'focus back on the tree after the last pane is hidden' || return 1
}

# _expect_hide_pane_rejects_tree_focus — the NEGATIVE check for af_hide_pane
# (cf. _expect_resize_rejected). With focus on the TREE, `x` is a no-op — and
# the old tree-menu marker was ALREADY satisfied, so af_hide_pane returned 0 and
# reported success for a hide that never happened. It must now fail. A short
# AF_DRIVER_TIMEOUT keeps the negative probe from burning the full 25s.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_hide_pane_rejects_tree_focus() {
    local saved="$AF_DRIVER_TIMEOUT" rc=0
    af_ensure_nav
    af_focus_tree || return 1
    AF_DRIVER_TIMEOUT=3
    af_hide_pane >/dev/null 2>&1 || rc=$?
    AF_DRIVER_TIMEOUT="$saved"
    if [ "$rc" -eq 0 ]; then
        _af_log "af_hide_pane wrongly SUCCEEDED with focus on the tree (no pane to hide)"
        return 1
    fi
    return 0
}

# _expect_pane_text_not_counted_as_tabs — regression proof for the #1561
# review finding. _af_tab_count must count sidebar tab rows (including
# custom-named ones) but MUST NOT count tree-like text a shell prints inside a
# workspace pane — otherwise af_close_tab regains a false timeout from the
# opposite direction. Drive this off a synthetic capture (stub af_capture in a
# subshell) so it is deterministic and does not depend on a live pane happening
# to print the right thing: two real sidebar tab rows on the left (one built-in
# `Preview`, one CUSTOM `cli-check`), and a bordered pane on the right whose
# `tree` output prints `├ 1 foo` / `└ 2 bar`. Exactly the two sidebar rows must
# count.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_pane_text_not_counted_as_tabs() {
    local screen count
    # Two dangerous pane rows are represented: one where the pane's `├ 1 foo`
    # shares a line with a sidebar tab row, and one where the sidebar column is
    # BLANK so only the pane's `│` border precedes the pane's `└ 2 bar`.
    screen="$(cat <<'SCREEN'
 ▾ beta                        │ ╭─ 2 cli-check ──────────────╮
   ├ 1 Preview                 │ │ $ tree                     │
   └ 2 cli-check               │ │ ├ 1 foo                    │
 ▸ alpha                       │ │   subdir                   │
                               │ │ └ 2 bar                    │
                               │ ╰────────────────────────────╯
SCREEN
)"
    # Override af_capture only inside this command substitution's subshell.
    count="$(af_capture() { printf '%s\n' "$screen"; }; _af_tab_count)"
    if [ "$count" != "2" ]; then
        _af_log "pane text inflated the tab count: got '$count', want 2 (the two sidebar rows)"
        _af_log "----- synthetic screen -----"
        printf '%s\n' "$screen" >&2
        _af_log "----------------------------"
        return 1
    fi
    return 0
}

# _expect_af_select_boundary — regression proof for #1759. af_select must
# evaluate its ready condition AFTER the final downward `j`, not only before it.
# Drive af_select against stubbed send/capture (no live TUI, no real sleeps) in
# a subshell so nothing leaks: a counter tracks `j` presses, and the row is
# rendered actionable (▾ + `D kill`) ONLY once the count reaches 40 — exactly
# the loop boundary. The old loop pressed the 40th `j` and exited without a
# trailing capture, so it never saw the actionable frame and returned non-zero
# here; the fixed loop checks the post-`j` state and returns 0.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_af_select_boundary() {
    (
        _AF_BOUNDARY_JCOUNT=0
        # Stubs local to this subshell; the sourced af_select resolves them
        # dynamically when it calls them.
        af_ensure_nav() { :; }
        af_focus_tree() { return 0; }
        sleep() { :; }
        af_send() { [ "${1:-}" = j ] && _AF_BOUNDARY_JCOUNT=$((_AF_BOUNDARY_JCOUNT + 1)); return 0; }
        af_capture() {
            if [ "$_AF_BOUNDARY_JCOUNT" -ge 40 ]; then
                printf '%s\n' ' ▾ target                       │ menu: D kill'
            else
                printf '%s\n' ' ▸ target                       │ menu: n new'
            fi
        }
        af_select target
    )
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
# play-test could wrongly "pass". af_select now lands the cursor on an
# actionable tab row, so `o attach` MUST actually fire here. If the bug returns,
# either af_select fails (no 'D kill') or af_attach times out waiting for the
# chrome to vanish.
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

# --- #1759 regression: af_select must check AFTER the final scan step ---
# A row that only becomes actionable on the loop's boundary `j` used to be
# missed (false selection failure). Deterministic stub proof — no live TUI.
step "af_select evaluates the boundary step (#1759)"        _expect_af_select_boundary

# --- #1757 regression: the task-overlay run action ---
# `m` drops straight into the selected task's EDIT form when a task exists, and
# at 80x24 its footer collapses `r run now` to `r run`. af_open_tasks used to
# wait for the stale `run now` and time out here; it now syncs on `r run`.
step "seed a task via the create form"                      af_add_task selftest-task
step "close the tasks overlay after create"                 af_close_tasks
step "reopen tasks — edit-mode overlay recognized (#1757)"  af_open_tasks
step "assert the task editor shows the run action"          af_assert_screen "$_AF_TASKS_RUN_HINT" 'task-overlay run action'
step "close the tasks overlay"                              af_close_tasks

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
step "pane text must not inflate the tab count (#1561 review)" _expect_pane_text_not_counted_as_tabs

# --- #1822 regression: af_hide_pane across the multi-pane and single-pane flows ---
# Runs last: it deliberately ends with an empty workspace.
step "hide a pane while another remains, then the last (#1822)" _expect_multipane_hide
step "af_hide_pane fails loudly with no pane focused (#1822)"  _expect_hide_pane_rejects_tree_focus

printf '\n=== SELF-TEST PASSED — %d/%d steps green ===\n' "$PASS" "$PASS"
printf 'the #1156 mis-drive is now a deterministic scenario.\n'

# Leave the sandbox as we found it (best-effort; container teardown is the
# real cleanup).
af_quit >/dev/null 2>&1 || true
exit 0
