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

# 144 cols so the left rail reaches its TreeMaxWidth=36 cap (clamp(22, 25%·W,
# 36)); the rail's detail line is indented 6 and reads "0 0 31 2 * · no
# upcoming run" (28), which clips at the 30-col rail a narrower terminal gives.
export AF_DRIVER_COLS=144 AF_DRIVER_ROWS=30
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
task_id="$("$bin" tasks add --repo "$AF_DRIVER_REPO" --name yearly-audit \
    --cron "$CRON" --prompt 'echo TASK-RAN-OK' | jq -er '.id')"

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

# A cron can be syntactically legal and still match no date: "0 0 31 2 *" is
# February 31st. ValidateCronExpr accepts it and ParseCron succeeds, but Next
# gives up after five years and hands back the ZERO time.Time, which formats as
# a thoroughly plausible "Jan 01 00:00". Both readouts have to name the absence
# rather than promise a fire time the task will never reach.
"$bin" tasks update "$task_id" --repo "$AF_DRIVER_REPO" --cron '0 0 31 2 *' >/dev/null

# The rail's next/last detail only renders on the FOCUSED row (#1126 made
# collapsed rows title-only), and `]` is the "next section" binding that walks
# focus from the instances tree onto the automations rail (#1706).
af_ensure_nav
af_send ']'
af_wait_for '▾\[✓\]  yearly-audit' "$AF_DRIVER_TIMEOUT" 'automations row focused and expanded'
af_wait_for '0 0 31 2 \* · no upcoming run' "$AF_DRIVER_TIMEOUT" \
    'the automations rail names the absence instead of a zero-time next run'
# Refute the PREFIX, not the whole timestamp: a clipped "next Jan 0…" would slip
# past a refute that spells out "next Jan 01 00:00" in full.
af_refute_screen '· next Jan' 'the rail never formats the zero time as a real next run'

af_open_tasks
af_wait_for 'No upcoming run' "$AF_DRIVER_TIMEOUT" \
    'the schedule picker names the absence too'
af_refute_screen 'Next run' 'the picker never promises a next run for a cron that cannot match'
af_close_tasks

af_assert_no_orphan_clients
af_quit
echo 'PASS: #2596 a Custom cron renders once in the real task editor, with an honest next-run preview'
