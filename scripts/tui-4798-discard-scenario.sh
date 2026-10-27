#!/usr/bin/env bash
# Real-TUI drive for the #4798 follow-up (DiscardDeletedDraft must close the
# edit form bound to the row it discards), via:
#
#   scripts/testbox.sh scenario scripts/tui-4798-discard-scenario.sh
#
# The bug: when the daemon positively proves a task the pane held an unsaved
# edit for was deleted out-of-band, DiscardDeletedDraft removed the row but
# never closed the open edit form. With a single task the next render panicked
# at `s.tasks[s.selectedIdx]` (index out of range [0] with length 0) and
# killed the TUI; with two tasks the cursor silently re-clamped onto a
# neighbour and the form's stale buffers wrote into it on the next Enter
# (silent cross-task clobber, the #1700 class).
#
# Why this cannot be a unit test alone: the trigger is the real ctrl+c →
# handleQuit → saveContentPaneState path (#1727 routes ctrl+c to handleQuit
# before the form's own cancel branch), and the not-found must come back from
# the daemon's RPC, not a stub. The fix closes the form on the discard; this
# scenario pins that end-to-end — the overlay returns to the list (no
# "Edit task" form) and the discarded-draft notice is shown.
#
# The single-task leg is the crash repro; the two-task leg is the clobber
# repro. The out-of-band deletion is a CLI `af tasks remove` run from this
# script (same AGENT_FACTORY_HOME / repo as the TUI) between opening the
# form and pressing ctrl+c, so the daemon's UpdateTask answers not-found.

set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# _af_tasks_remove_by_name runs `af tasks remove <name>` against the same
# daemon the TUI is talking to. `af tasks remove` accepts an id OR a name
# (api/tasks.go:264, resolveTaskArg), so the name resolves on the daemon
# side. The daemon (sole writer) drops the task from its in-memory store
# AND tasks.json, so the TUI's next UpdateTask for that task returns
# not-found — the positive not-found that triggers DiscardDeletedDraft.
_af_tasks_remove_by_name() {
    local name="$1"
    ( cd "$AF_DRIVER_REPO" && AGENT_FACTORY_HOME="$AGENT_FACTORY_HOME" \
        af tasks remove "$name" >/dev/null 2>&1 ) || {
        _af_fail "out-of-band remove: af tasks remove '$name' failed"
        return 1
    }
    _af_log "out-of-band: removed task name=$name from the daemon"
}

# drive_single_task_close_on_discard is the crash repro. One task, form open
# on it, dirty, ctrl+c quits: the daemon returns not-found, the discard closes
# the form, the next render shows the (now empty) list — not a panic.
drive_single_task_close_on_discard() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    af_boot
    af_ensure_nav
    af_focus_tree

    # Seed a single task; af_add_task leaves the overlay open in list mode.
    af_add_task 'single-discard' || return 1
    af_close_tasks || return 1

    # Reopen: #1249's one-step flow drops straight into the edit form on the
    # selected task. The bug report's exact entry.
    af_open_tasks || return 1
    af_wait_for 'Edit task' "$AF_DRIVER_TIMEOUT" 'edit form opened on the single task' || return 1

    # Delete the task out-of-band so the daemon's save returns not-found.
    _af_tasks_remove_by_name 'single-discard' || return 1

    # Mark the form-bound task dirty from inside the form: Tab to the trigger
    # selector (a non-text stop where `x` routes to toggleSelectedTask per
    # #4838), then `x`. The form stays open and the row is dirty — the
    # in-form dirty state the save-on-quit catches.
    af_send Tab
    af_send x

    # ctrl+c is routed unconditionally to handleQuit (#1727): the form's own
    # ctrl+c-cancel branch never runs from the tasks overlay.
    af_send C-c

    # The fix: the form closes on the discard. The next render shows the
    # empty list ("No tasks"), NOT the orphaned "Edit task" form. Before the
    # fix the render panicked here (ErrProgramKilled: ErrProgramPanic).
    af_wait_gone 'Edit task' "$AF_DRIVER_TIMEOUT" 'orphaned edit form closed after discard' || return 1
    af_wait_for 'No tasks' "$AF_DRIVER_TIMEOUT" 'empty list rendered (no panic)' || return 1

    # The quit was aborted to show the discarded-draft notice.
    af_wait_for 'Discarded unsaved edits to' "$AF_DRIVER_TIMEOUT" 'discarded-draft notice shown' || return 1

    _af_log 'assert OK: single-task discard closes the form, no render panic, notice shown'
    echo 'PASS: #4798-followup single-task discard closes the form (no crash)'
}

# drive_two_task_neighbour_untouched is the clobber repro. Two tasks, form
# open on alpha, ctrl+c quits: the daemon returns not-found for alpha, the
# discard closes the form, and bravo (the neighbour the cursor would have
# re-clamped onto) keeps its own name — not alpha's stale buffer.
drive_two_task_neighbour_untouched() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    af_boot
    af_ensure_nav
    af_focus_tree

    af_add_task 'alpha-discard' || return 1
    af_add_task 'bravo-keep' || return 1
    af_close_tasks || return 1

    af_open_tasks || return 1
    af_wait_for 'Edit task' "$AF_DRIVER_TIMEOUT" 'edit form opened on alpha' || return 1
    # The form opens on the first task (alpha-discard), per the #1249 flow.
    af_wait_for 'alpha-discard' "$AF_DRIVER_TIMEOUT" 'form is on alpha-discard' || return 1

    # Delete alpha out-of-band; bravo stays on disk.
    _af_tasks_remove_by_name 'alpha-discard' || return 1

    af_send Tab
    af_send x            # toggle alpha dirty inside the form; form stays open

    af_send C-c          # ctrl+c → handleQuit → save → not-found → discard

    # The fix: the form closes (no "Edit task bravo-keep" with alpha's stale
    # buffers). Before the fix the cursor re-clamped onto bravo with the
    # form still open, titling "Edit task bravo-keep" while the buffers
    # still held alpha's values.
    af_wait_gone 'Edit task' "$AF_DRIVER_TIMEOUT" 'orphaned form closed (not re-targeted onto bravo)' || return 1

    # The discarded-draft notice names alpha, the actually-deleted task.
    af_wait_for 'Discarded unsaved edits to' "$AF_DRIVER_TIMEOUT" 'discarded-draft notice shown' || return 1

    _af_log 'assert OK: two-task discard closes the form on alpha; bravo never became the edit target'
    echo 'PASS: #4798-followup two-task discard closes the form (no cross-task clobber)'
}

drive_single_task_close_on_discard
drive_two_task_neighbour_untouched
af_quit || true
echo 'PASS: #4798-followup TUI discard closes the open edit form'
