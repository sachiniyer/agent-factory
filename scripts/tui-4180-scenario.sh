#!/usr/bin/env bash
# Real-TUI drive for #4180: the task form's Max runs field — gated on the same
# shape the daemon requires (a watch trigger, no target session), refusing the
# strings the shared vectors refuse, and persisting the cap the user sets.
#
# Runs inside the #1130 container sandbox:
#   scripts/testbox.sh scenario scripts/tui-4180-scenario.sh
#
# The persisted cap is asserted on the daemon's own record (tasks.json in the
# sandbox AF home) — the screen proves the form; the file proves the wire.
#
# The form scrolls the FOCUSED field into view, so every assertion on the Max
# runs row is made while focus sits on or near the bottom of the form.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30

TASKS_JSON="$AGENT_FACTORY_HOME/tasks.json"

# cap_of <task-name> — the stored max_concurrent_runs, or empty when absent.
cap_of() {
    jq -r --arg n "$1" '[.tasks[] | select(.name == $n)][0].max_concurrent_runs // empty' "$TASKS_JSON" 2>/dev/null
}

# wait_cap <task-name> <expected> — poll the daemon's record until the cap
# lands; the TUI's save is an async daemon write, not a synchronous file edit.
wait_cap() {
    local name="$1" want="$2" deadline=$(( $(date +%s) + ${AF_DRIVER_TIMEOUT:-20} ))
    while :; do
        [ "$(cap_of "$name")" = "$want" ] && return 0
        [ "$(date +%s)" -ge "$deadline" ] && break
        sleep "$AF_DRIVER_POLL"
    done
    echo "FAIL: $name stored max_concurrent_runs='$(cap_of "$name")', want $want" >&2
    cat "$TASKS_JSON" >&2
    return 1
}

af_reset_sandbox
af_boot
af_open_tasks

# --- Create: the cron default shows the reason, not an input -----------------
af_send n
af_wait_for 'New task' "$AF_DRIVER_TIMEOUT" 'task create form'
af_send_literal 'cap-watch'
# Focus stops: name -> trigger -> value -> prompt -> target -> on done -> max
# runs. Descend to the last stop so the Delivery group is scrolled into view.
af_send Tab Tab Tab Tab Tab Tab
af_wait_for 'Max runs:' "$AF_DRIVER_TIMEOUT" 'the Max runs row'
af_wait_for 'Cron fires already coalesce\.' "$AF_DRIVER_TIMEOUT" \
    'cap reason on the default cron shape'

# --- Flip to watch; the reason becomes an input ------------------------------
af_send BTab BTab BTab BTab BTab   # -> trigger selector
af_send Right                      # -> watch
af_send Tab                        # -> watch command
af_send_literal 'while sleep 60; do echo tick; done'
af_send Tab                        # -> prompt (optional for watch)
af_send Tab                        # -> target session
af_send_literal 'reuse-me'
af_send Tab                        # -> on done
af_send Tab                        # -> max runs (reason row while targeted)
af_wait_for 'Deliveries into one session already serialize\.' "$AF_DRIVER_TIMEOUT" \
    'cap reason on a targeted watch shape'
af_send_literal '9'   # refused: the row shows the reason, not an input
af_send BTab BTab    # -> target session
af_send C-u          # clear the target
af_send Tab Tab      # -> max runs
# The placeholder only renders on an empty buffer — seeing it proves both that
# the input came back and that the refused '9' never entered it.
af_wait_for 'unlimited' "$AF_DRIVER_TIMEOUT" 'the cap input returns once target is gone'
af_send_literal '3'
af_send Enter      # submit from a non-prompt field
af_wait_for 'cap-watch' "$AF_DRIVER_TIMEOUT" 'task created in the list'
wait_cap cap-watch 3

# --- Validate: a refused value keeps the form open ---------------------------
af_send n
af_wait_for 'New task' "$AF_DRIVER_TIMEOUT" 'task create form (2nd)'
af_send_literal 'cap-neg'
af_send Tab
af_send Right      # watch
af_send Tab        # -> watch command
af_send_literal 'while sleep 60; do echo tick; done'
af_send Tab Tab Tab Tab  # -> prompt -> target -> on done -> max runs
af_send_literal '-1'
af_send Enter
af_wait_for 'max runs must be a non-negative integer' "$AF_DRIVER_TIMEOUT" \
    'inline refusal of a negative cap'
# The title scrolls off while focus sits at the bottom; the pinned footer is
# what proves the form held instead of submitting.
af_wait_for 'esc cancel' "$AF_DRIVER_TIMEOUT" 'form stays open on a refused cap'
af_send C-u
af_send_literal '4'
af_send Enter
af_wait_for 'cap-neg' "$AF_DRIVER_TIMEOUT" 'task created after the fix'
wait_cap cap-neg 4

# --- Edit: the stored cap seeds the field, and a new value persists ----------
# After a create the overlay is back in list mode; Escape here would close it.
af_send k || true  # cursor may already sit on the top task
af_send Enter      # enter edit on the selected task
af_wait_for 'Edit task' "$AF_DRIVER_TIMEOUT" 'task edit form'
# Which task did we open? Anchor on the Name field row — the list behind the
# modal can show either name at its edges and must not satisfy this check.
screen="$(af_capture)"
if grep -q 'Name:.*cap-watch' <<<"$screen"; then edited=cap-watch
elif grep -q 'Name:.*cap-neg' <<<"$screen"; then edited=cap-neg
else
    echo "FAIL: edit form shows neither task name:" >&2
    printf '%s\n' "$screen" >&2
    exit 1
fi
af_send Tab Tab Tab Tab Tab Tab  # -> max runs, scrolling it into view
af_wait_for 'Max runs:' "$AF_DRIVER_TIMEOUT" 'Max runs row in edit mode'
screen="$(af_capture)"
if ! grep -Eq 'Max runs:.*[34]' <<<"$screen"; then
    echo "FAIL: edit form's Max runs row did not seed the stored cap:" >&2
    printf '%s\n' "$screen" >&2
    exit 1
fi
af_send C-u
af_send_literal '7'
af_send Enter
af_wait_for "$edited" "$AF_DRIVER_TIMEOUT" 'edited task back in the list'
# Edits are staged dirty and flushed by saveContentPaneState when the overlay
# closes — the daemon record cannot update while the list is still open.
af_close_tasks
wait_cap "$edited" 7

echo "PASS: #4180 real-TUI scenario — gate reasons shown, cap set + persisted, refusal held the form, edit re-persists"
