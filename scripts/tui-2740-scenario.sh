#!/usr/bin/env bash
# Real-TUI regression for #2740, via:
#
#   scripts/testbox.sh scenario scripts/tui-2740-scenario.sh
#
# A failed operator archive hook must be visible without leaving the optimistic
# row live or inviting a retry: the daemon has already archived the session.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
# shellcheck disable=SC2016 # AF_REPO_ROOT expands later inside the archive hook.
af_set_config 'default_program = "claude"
on_archive_command = "find node_modules -depth -delete; printf hook-ran > \"$AF_REPO_ROOT/archive-hook-ran\"; printf \"prune failed visibly\\n\"; exit 23"

[program_overrides]
claude = "bash"'

af_boot
af_new_instance worker

bin="$(_af_resolve_bin)"
worktree="$("$bin" sessions get worker | jq -er '.worktree.worktree_path')"
mkdir -p "$worktree/node_modules/pkg"
printf bulk >"$worktree/node_modules/pkg/bulk.js"

af_select worker
af_send a
af_wait_for 'Archive session' "$AF_DRIVER_TIMEOUT" 'archive confirmation'
af_send y

# Both assertions are required. The warning proves the hook failure surfaced;
# Archived (1) proves the same outcome was committed rather than presented as a
# failed/retryable archive that left the optimistic row live.
af_wait_for 'on-archive hook' "$AF_DRIVER_TIMEOUT" 'failed hook warning surfaces'
af_wait_for 'Archived \(1\)' "$AF_DRIVER_TIMEOUT" 'warning archive settles in Archived'

archived_path="$("$bin" sessions get worker | jq -er '.worktree.worktree_path')"
if [ "$archived_path" = "$worktree" ] || [ ! -d "$archived_path" ]; then
    _af_fail "#2740: session did not relocate despite the committed hook warning: $archived_path"
    exit 1
fi
if [ -d "$archived_path/node_modules" ]; then
    _af_fail "#2740: hook did not prune node_modules before relocation"
    exit 1
fi
if [ ! -f "$AF_DRIVER_REPO/archive-hook-ran" ]; then
    _af_fail "#2740: configured operator hook never ran"
    exit 1
fi

af_assert_no_orphan_clients
af_quit
echo 'PASS: #2740 failed archive hook surfaces while the real TUI settles the row as Archived'
