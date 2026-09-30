#!/usr/bin/env bash
# Real-TUI symptom contrast for the archive-confirm race's BUSY-OP leg: a
# second surface starts (but does not complete) an archive of the same session
# while this TUI's confirm dialog is open, so the snapshot reconciles the row
# to LiveRunning+OpArchiving. At confirm, BeginArchive refuses on the op clause
# (s.op != OpNone), the fix's LiveArchived-only guard does NOT match (liveness
# is LiveRunning), so the closure falls through and emits startArchiveMsg. The
# daemon authoritatively refuses with its accurate "busy; try again in a
# moment" recovery modal — the preserved UX the report does not change, which
# this scenario proves survives the narrow fix (no over-broad suppression).
#
#   scripts/testbox.sh scenario scripts/tui-archive-busy-op-scenario.sh
#
# The on_archive hook SLEEPS so the second surface's archive stays in-flight
# (OpArchiving, LiveRunning) for a deterministic window; the out-of-band archive
# is run in the background so the TUI can confirm while it is still mid-hook.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
# A fast mock agent and an on_archive hook that SLEEPS, so the out-of-band
# archive holds OpArchiving over a deterministic window instead of completing.
af_set_config 'default_program = "claude"
on_archive_command = "sleep 30"
[program_overrides]
claude = "bash"'

af_boot
af_new_instance worker
bin="$(_af_resolve_bin)"

# 1. Open the archive confirmation on the live row.
af_select worker
af_send a
af_wait_for 'Archive session' "$AF_DRIVER_TIMEOUT" 'archive confirmation opened'

# 2. Inject the busy-op race: a SECOND surface STARTS archiving the same
#    session in the background. Its on_archive hook sleeps, so the daemon holds
#    OpArchiving (liveness still LiveRunning) — the snapshot reconciles that op
#    onto this TUI's row. Run from the session's repo cwd (see the race
#    scenario) and in the background so the TUI can confirm while it is stuck.
cd "$AF_DRIVER_REPO"
"$bin" sessions archive worker >/tmp/archive-busy-out-of-band.log 2>&1 &

# 3. Poll the DAEMON's own state until its instance is mid-archive (OpArchiving
#    composes to the "deleting" status_name). This guarantees the daemon's busy
#    gate (instance.GetInFlightOp() != OpNone, evaluated BEFORE the op-lock wait)
#    fires immediately when the TUI's redundant archive RPC arrives — without
#    this wait the TUI's confirm can race ahead of the background archive's
#    BeginArchive, leaving the TUI's RPC blocked on the 30 s op-lock instead of
#    refused with "busy".
busy_ready=0
for _ in $(seq 1 40); do
    if "$bin" sessions get worker 2>/dev/null | grep -q '"status_name": *"_deleting"\|"status_name": *"deleting"'; then
        busy_ready=1
        break
    fi
    sleep 0.5
done
[ "$busy_ready" = 1 ] || { _af_fail "race: daemon never reached the OpArchiving (deleting) state the busy gate needs"; exit 1; }
_af_log "daemon is mid-archive (deleting) — busy gate is armed"

# 4. The confirm overlay must still be open (the reconcile does not dismiss
#    it). Then confirm.
af_assert_screen 'Archive session' 'confirm overlay survived the busy-op reconcile'
af_send y

# 5. The fix falls through (liveness is LiveRunning, NOT LiveArchived) and
#    emits startArchiveMsg; the redundant RPC reaches the daemon. NOTE: the
#    daemon serializes ArchiveSession calls behind taskTargetMu (held for the
#    whole archive, daemon/archive.go:79-80), so while the background archive's
#    on_archive hook is sleeping this TUI's RPC BLOCKS on that mutex rather than
#    refusing at the in-process busy gate. Once the background archive reaches
#    its terminal (Archived) state ~30 s later, the TUI's RPC acquires the
#    mutex and the daemon returns the terminal-state refusal. The load-bearing
#    signal here is that a recovery modal DOES appear at all — the fix did NOT
#    suppress the busy-op emit (unlike the LiveArchived race, where the race
#    scenario proved NO modal appears). That contrast is the narrowing proof.
af_wait_for 'Cannot archive session' 50 'busy-op recovery modal appears (fix did NOT suppress the busy-op emit)'
af_assert_screen 'already archived' 'terminal-state refusal detail renders (daemon serialized the RPC past the busy window)'

af_quit 2>/dev/null || true
echo 'PASS: archive busy-op symptom contrast — the daemon accurate "busy; try again" modal survives the narrow LiveArchived-only fix'
