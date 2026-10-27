#!/usr/bin/env bash
# Real-TUI regression for the archive-confirm race: a background snapshot poll
# settles a row to LiveArchived WHILE the archive confirmation overlay is open,
# then the user confirms. Pre-fix this emitted a spurious startArchiveMsg the
# daemon rejected with ErrAlreadyArchived, surfacing the contradictory
# "Cannot archive session ... already archived" recovery modal. Post-fix the
# refused BeginArchive on a LiveArchived row is a silent no-op: no redundant
# RPC, no contradictory modal — the row is already where the user asked it to
# go, matching handleArchive's own press-time no-op for an Archived row.
#
#   scripts/testbox.sh scenario scripts/tui-archive-confirm-race-scenario.sh
#
# The race leg is driven for real: a SECOND archiving surface (`af sessions
# archive`) completes the archive out-of-band while the TUI confirm dialog is
# open, then the TUI's 750 ms snapshot poll settles the same row to LiveArchived
# (the leg the unit test TestHandleArchive_ConfirmRefusedSuppressesStartArchive
# drives through Update(snapshotFetchedMsg)). Then the user confirms on the
# now-stale row.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
# A fast mock agent so the scenario never depends on a real Claude binary.
af_set_config 'default_program = "claude"
[program_overrides]
claude = "bash"'

af_boot
af_new_instance worker

bin="$(_af_resolve_bin)"

# 1. Open the archive confirmation on the live row.
af_select worker
af_send a
af_wait_for 'Archive session' "$AF_DRIVER_TIMEOUT" 'archive confirmation opened'

# 2. Inject the race: a SECOND archiving surface completes the archive of the
#    SAME session out-of-band while this TUI's confirm dialog is still open.
#    The daemon archives the session directly; this TUI's next snapshot poll
#    observes LiveArchived and reconciles the row into the Archived folder.
#    Run from the session's repo cwd: `af sessions` resolves the repo scope from
#    cwd, and from /src the worker session is out of scope ([]), so the archive
#    would not find it.
cd "$AF_DRIVER_REPO"
"$bin" sessions archive worker >/tmp/archive-race-out-of-band.log 2>&1 || {
    _af_fail "race: out-of-band 'af sessions archive worker' failed; see /tmp/archive-race-out-of-band.log"
    cat /tmp/archive-race-out-of-band.log >&2
    exit 1
}

# 3. Wait for the TUI's snapshot poll to reconcile the same row into the
#    Archived folder — the real Update(snapshotFetchedMsg) leg. 'Archived (1)'
#    is the rail marker for one archived session.
af_wait_for 'Archived \(1\)' "$AF_DRIVER_TIMEOUT" 'snapshot poll settled the row to Archived'

# 4. The reconcile must NOT have dismissed the confirm overlay (the snapshot
#    handler never touches m.state/confirmationOverlay) — so the user can still
#    confirm on the now-stale row. This is the leg that makes the race
#    non-vacuous and that the fix must handle.
af_assert_screen 'Archive session' 'confirm overlay survived the snapshot reconcile'

# 5. Confirm on the now-LiveArchived row. BeginArchive refuses (its predicate
#    is s.op == OpNone && s.liveness != LiveArchived); the fix suppresses the
#    emit so no redundant archive RPC is sent and no contradictory recovery
#    modal is shown.
af_send y

# 6. Settle: give the event loop a moment to process the confirm. The
#    contradictory recovery modal must NOT appear — the fix suppressed the
#    emit, so no ErrAlreadyArchived ever reaches handleInstanceArchived. Allow
#    a short window for a (would-be) modal to render, then refute it.
sleep 1
af_refute_screen 'Cannot archive session' 'no contradictory "Cannot archive session" recovery modal'
af_refute_screen 'already archived' 'no "already archived" prose from a redundant archive RPC'

# 7. The row stays Archived (the suppressed confirm changed nothing; the
#    daemon already had it archived via the out-of-band surface).
af_assert_screen 'Archived \(1\)' 'row remains Archived after the suppressed confirm'

af_assert_no_orphan_clients
af_quit
echo 'PASS: archive-confirm race — a LiveArchived settle mid-dialog suppresses the spurious archive RPC and the contradictory recovery modal'
