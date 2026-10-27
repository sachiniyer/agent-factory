#!/usr/bin/env bash
# TUI scenario for the handleHandoff IsTearingDown() guard fix.
#
# Bug: pressing F (Handoff) on a tearing-down (deleting) row produced
#   session "going-away" is busy (3); try again in a moment
# instead of the sibling-verb message:
#   session 'going-away' is being deleted
#
# IsTearingDown() returns true for both OpKilling and OpArchiving. There is no
# on_kill_command hook, so we use the archive path (on_archive_command = "sleep
# 30") to hold the row in OpArchiving — the same tearing-down state — for a
# deterministic window. This exercises the identical guard code path:
#   handleHandoff → IsTearingDown() → "is being deleted"
#
#   scripts/testbox.sh scenario scripts/tui-handoff-teardown-scenario.sh
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
# A bash mock agent keeps instance startup cheap; the on_archive hook SLEEPS so
# the row stays in OpArchiving (composes to [deleting]) for a deterministic
# 30-second window — long enough to press F and read the notice bar.
af_set_config 'default_program = "claude"
on_archive_command = "sleep 30"
[program_overrides]
claude = "bash"'

af_boot
af_new_instance going-away

# 1. Select the instance and start the archive (D would race to completion; the
#    archive hook's sleep holds the OpArchiving window open).
af_select going-away
af_send a
af_wait_for 'Archive session' "$AF_DRIVER_TIMEOUT" 'archive confirmation opened' \
    || { _af_fail 'could not open archive confirmation'; exit 1; }

# 2. Confirm — BeginArchive raises OpArchiving synchronously, composing to
#    [deleting]. The on_archive hook sleeps so the row stays in that state.
af_send y
af_wait_for '\[deleting\]' "$AF_DRIVER_TIMEOUT" 'row flipped to [deleting] during archive' \
    || { _af_fail 'row did not transition to [deleting]'; exit 1; }

# 3. Press F (Handoff) on the tearing-down row. The keybind is menu-hidden for
#    teardown rows, but a user pressing it anyway must get the "is being
#    deleted" message — the exact regression class this fix addresses.
af_send F

# 4. Assert the notice bar shows the sibling-verb message, NOT the buggy
#    "busy (3); try again in a moment" from runtimeActionBusyError.
af_wait_for 'is being deleted' "$AF_DRIVER_TIMEOUT" 'handoff on tearing-down row shows "is being deleted"' \
    || { _af_fail 'expected "is being deleted" notice; got something else'; exit 1; }

# 5. Refute the buggy "busy" message.
af_refute_screen 'busy' 'no "busy" message for a tearing-down row' \
    || { _af_fail 'the buggy "busy" message appeared for a tearing-down row'; exit 1; }

af_quit 2>/dev/null || true
echo 'PASS: handoff on a tearing-down (OpArchiving/[deleting]) row shows "is being deleted", not "busy"'
