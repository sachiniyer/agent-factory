#!/usr/bin/env bash
# Real-TUI regression for #2596, via:
#
#   scripts/testbox.sh scenario scripts/tui-2596-scenario.sh
#
# Opening a Custom (cron) task in the editor used to print its expression three
# times in two lines: once in the editable Cron input, then twice more on the
# preview line, because Describe() ("Custom: <raw>") and Cron() (<raw>) collapse
# to the same string for Custom. A render-string unit test can miss this — the
# duplicate is only obviously wrong on the assembled screen — so this drives the
# real TUI and reads the pane.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# The month field is restricted, so no preset matches and schedule.ParseCron
# falls the picker back to Custom with the expression preserved verbatim.
CRON='0 0 1 1 *'

af_reset_sandbox
af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'

af_boot

bin="$(_af_resolve_bin)"
"$bin" tasks add --repo "$AF_DRIVER_REPO" --name yearly-audit \
    --cron "$CRON" --prompt 'echo TASK-RAN-OK' >/dev/null

# `m` opens straight into the RAIL-selected task's config (#1249), so the rail
# has to have picked the new task up before the editor can show its schedule.
af_wait_for 'yearly-audit' "$AF_DRIVER_TIMEOUT" 'task lands in the automations rail'
af_open_tasks
af_wait_for 'Custom \(cron\)' "$AF_DRIVER_TIMEOUT" 'schedule picker opens on Custom'

cron_re="$(_af_regex_escape "$CRON")"

# The defect itself: the same expression twice on one rendered line. Matched
# without naming the separator, so a later copy change to " · " cannot quietly
# stop this from detecting a repeat.
af_refute_screen "${cron_re}.*${cron_re}" \
    '#2596: the expression is not printed twice on one line'
af_refute_screen "Custom: ${cron_re}" \
    "#2596: Describe()'s \"Custom: <raw>\" echo is gone from the preview line"

# What must survive: the editable input still carries the expression, and the
# preview line spends itself on the one thing that input cannot say.
af_assert_screen "Cron.*${cron_re}" 'the editable Cron input still shows the expression'
af_assert_screen 'Next run [A-Z][a-z][a-z] [0-9][0-9] [0-9][0-9]:[0-9][0-9]' \
    'the preview line shows the next fire time instead'

# Belt and braces on the whole pane: with the editor open over the rail the
# expression is on screen exactly once.
occurrences="$(af_capture | grep -cF -- "$CRON" || true)"
if [ "$occurrences" != "1" ]; then
    _af_fail "#2596: expected the cron expression on exactly 1 line, found ${occurrences}"
    af_capture >&2
    exit 1
fi

af_close_tasks
af_assert_no_orphan_clients
af_quit
echo 'PASS: #2596 a Custom cron renders once in the real task editor, with a next-run preview'
