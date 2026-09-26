package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestHandleArchive_ConfirmRefusedSuppressesStartArchive reproduces the race
// where a background snapshot poll settles a row to LiveArchived WHILE the
// archive confirmation overlay is still open, then drives OnConfirm. BeginArchive
// is non-total (s.op == OpNone && s.liveness != LiveArchived), so on the
// LiveArchived row it refuses. The confirm closure must suppress the spurious
// startArchiveMsg (no redundant archive RPC, no contradictory "Cannot archive
// session ... already archived" recovery modal) — the row is already where the
// user asked it to go, exactly matching handleArchive's own press-time no-op for
// an Archived row (TestHandleArchive_RestingRowIsNoOp).
//
// The race leg is driven through the real Update(snapshotFetchedMsg) reconcile
// path (not a hand-rolled SetArchived), so it also proves the snapshot handler
// does NOT dismiss the confirm overlay — the user can still press confirm on the
// now-stale row, which is the leg that makes the race non-vacuous.
func TestHandleArchive_ConfirmRefusedSuppressesStartArchive(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// The app test binary installs a panic-on-illegal-transition hook
	// (transition_hook_test.go) so a mis-ordered edge is a loud test failure.
	// Production leaves the hook nil and a refused transition degrades to a
	// soft error — which is exactly the refusal mode this bug rides on
	// (BeginArchive's predicate refuses a LiveArchived row). Neutralize the hook
	// for this test so the refusal behaves as it does in production, and restore
	// it on cleanup so neighboring tests keep the loud guard.
	restoreHook := session.SetIllegalTransitionHook(nil)
	t.Cleanup(restoreHook)

	// 1. Open the archive confirmation on a live row. The optimistic op is NOT
	//    raised until confirm is pressed, so the row sits at op=OpNone,
	//    liveness=LiveReady while the dialog is open.
	model, _ := h.handleArchive()
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state, "archiving a live row opens the confirmation")
	require.NotNil(t, h.confirmationOverlay, "the confirmation overlay must be present")

	// 2. Inject the race: a background snapshot settles the SAME row to
	//    LiveArchived while the overlay is open. This drives the production
	//    Update(snapshotFetchedMsg) reconcile path, whose ObserveLiveness is
	//    total and preserves the row's op at OpNone — leaving the two axes
	//    (op, liveness) exactly the inputs BeginArchive reads.
	archived := inst.ToInstanceData()
	archived.Liveness = session.LiveArchived
	archived.Status = session.Archived
	archived.InFlightOp = session.OpNone
	model, _ = h.Update(snapshotFetchedMsg{repoID: h.repoID, data: []session.InstanceData{archived}})
	h = model.(*home)

	// 3. The reconcile neither dismisses the overlay nor re-scrolls the
	//    selection — assert it stayed open and the row settled to LiveArchived
	//    with no op in flight. This is the leg that makes the race non-vacuous:
	//    the user can still confirm on the now-Archived row.
	require.Equal(t, stateConfirm, h.state, "snapshot reconcile must not dismiss the confirmation overlay")
	require.NotNil(t, h.confirmationOverlay, "the confirmation overlay must survive the snapshot reconcile")
	require.Equal(t, session.LiveArchived, inst.GetLiveness(), "the row must settle to LiveArchived via the snapshot reconcile")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "no optimistic op is raised while the dialog is open")

	// 4. Confirm. On a LiveArchived row BeginArchive refuses (its predicate is
	//    s.op == OpNone && s.liveness != LiveArchived). The closure must treat
	//    that refusal as a silent no-op — the row is already archived, the user's
	//    goal is met — rather than emitting startArchiveMsg, which would dispatch
	//    an archive RPC the daemon rejects with ErrAlreadyArchived.
	h.confirmationOverlay.OnConfirm()
	require.Nil(t, h.pendingConfirmMsg,
		"a refused BeginArchive on a LiveArchived row must not emit a startArchiveMsg (no redundant archive RPC, no contradictory recovery modal)")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the refused transition must not raise an optimistic op")
	require.Equal(t, session.LiveArchived, inst.GetLiveness(),
		"the row stays archived after the suppressed confirm")
}

// TestHandleInstanceArchived_AlreadyArchivedRaisesRecoveryModal characterizes the
// user-visible symptom path the fix above avoids: when an archive RPC IS issued
// for a row the daemon sees as already archived, the daemon intentionally returns
// ErrAlreadyArchived as a failure-shaped sentinel for the named-session archive
// verb (daemon/archive.go). This test drives that exact error shape through
// handleInstanceArchived and asserts it renders the contradictory "Cannot archive
// session ... already archived" recovery modal — documenting the contract path so
// the fix is provably targeting the right emit, and locking the failure branch's
// behavior against drift. It passes both before and after the fix (the fix keeps
// the RPC from being issued in the first place); it does NOT change
// handleInstanceArchived's handling of an ErrAlreadyArchived that does arrive.
func TestHandleInstanceArchived_AlreadyArchivedRaisesRecoveryModal(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Ready)
	// The confirm closure raised OpArchiving before the RPC was dispatched; mirror
	// that here so the failure branch's ClearOp revert (which reverts the
	// optimistic op) is exercised.
	require.NoError(t, inst.Transition(session.BeginArchive()))
	require.Equal(t, session.OpArchiving, inst.GetInFlightOp())
	h.store.AddInstance(inst)
	target := captureSessionActionTarget(inst, h.repoID)

	// The exact error shape the daemon returns for an already-archived named
	// session (daemon/archive.go:127).
	alreadyArchivedErr := fmt.Errorf("session %q is %w", "worker", daemon.ErrAlreadyArchived)

	// ErrAlreadyArchived is a plain sentinel, not a committed outcome and not an
	// outcome-uncertain transport failure — so the failure branch the report
	// quotes is the one taken (not the mutationOutcomeError branch).
	require.False(t, apiclient.IsMutationCommitted(alreadyArchivedErr),
		"ErrAlreadyArchived is not a committed (committed-with-follow-up) outcome")
	require.False(t, mutationOutcomeUnknown(alreadyArchivedErr),
		"ErrAlreadyArchived is a plain refusal, not an outcome-uncertain transport error")

	model, _ := h.handleInstanceArchived(instanceArchivedMsg{target: target, err: alreadyArchivedErr})
	h = model.(*home)

	// The recovery modal the report calls contradictory: the condition line says
	// "Cannot archive session" while its own detail says "already archived".
	require.NotNil(t, h.recovery, "ErrAlreadyArchived must surface the recovery modal")
	require.Equal(t, "Cannot archive session", h.recovery.condition)
	require.Contains(t, h.recovery.detail, "already archived",
		"the detail must carry the daemon's true-state prose")
	require.Contains(t, h.recovery.detail, "retained",
		"the detail must carry the 'retained' wording the report calls contradictory against the condition")
	// The failure branch reverts the optimistic OpArchiving, so the row does not
	// strand on an archiving fence while the daemon already has it Archived.
	require.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the failure branch's ClearOp must revert the optimistic OpArchiving")
}

// TestHandleArchive_ConfirmOnLiveRowStillEmitsStartArchive is the non-regression
// guard for the corresponding happy path: a confirm time with NO refused
// transition (the row is still live) must still emit startArchiveMsg and dispatch
// the archive RPC. This guards the narrowed fix against an over-broad guard that
// would suppress legitimate archive attempts; the LiveArchived-only suppression
// must not leak into the normal archive flow.
func TestHandleArchive_ConfirmOnLiveRowStillEmitsStartArchive(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleArchive()
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state)
	require.NotNil(t, h.confirmationOverlay)

	// No background snapshot intervenes; the row stays LiveReady/OpNone, so
	// BeginArchive is accepted.
	h.confirmationOverlay.OnConfirm()
	require.NotNil(t, h.pendingConfirmMsg, "confirming a live row must emit startArchiveMsg")
	_, ok := h.pendingConfirmMsg.(startArchiveMsg)
	require.True(t, ok, "the pending confirm message must be a startArchiveMsg")
	require.Equal(t, session.OpArchiving, inst.GetInFlightOp(),
		"the accepted transition must raise the optimistic OpArchiving fence")
}

// TestHandleArchive_ConfirmBusyOpFallsThroughAndEmitsStartArchive pins the
// narrowing: the fix suppresses ONLY the LiveArchived refusal (where the row is
// already where the user asked it to go). A refused BeginArchive on a row that is
// busy with another client's op — here op=OpArchiving from a concurrent archive,
// adopted via the snapshot reconcile path (daemon/archive.go reports
// InFlightOp=OpArchiving while the row is still LiveRunning) — must NOT be
// suppressed. The closure falls through and emits startArchiveMsg so the daemon
// authoritatively refuses with its accurate "busy; try again" modal, whose UX the
// report preserves unchanged. Guards against a future over-broad guard that would
// silently drop a valid archive intent with no feedback.
func TestHandleArchive_ConfirmBusyOpFallsThroughAndEmitsStartArchive(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// Mirror the main test: production leaves the panic-on-illegal-transition hook
	// nil, so a refused BeginArchive degrades to a soft error. Neutralize it so the
	// confirm-time refusal (op != OpNone) behaves as in production.
	restoreHook := session.SetIllegalTransitionHook(nil)
	t.Cleanup(restoreHook)

	model, _ := h.handleArchive()
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state)
	require.NotNil(t, h.confirmationOverlay)

	// Another surface is archiving the same session: the daemon snapshot reports
	// the row as LiveRunning with InFlightOp=OpArchiving. Driving it through the
	// real Update(snapshotFetchedMsg) reconcile path adopts that op onto the local
	// row via reconcileSnapshotOp→adoptSnapshotOp (BeginArchive on a LiveRunning,
	// OpNone row succeeds, raising OpArchiving) — the report's busy-op leg.
	busy := inst.ToInstanceData()
	busy.Liveness = session.LiveRunning
	busy.Status = session.Running
	busy.InFlightOp = session.OpArchiving
	model, _ = h.Update(snapshotFetchedMsg{repoID: h.repoID, data: []session.InstanceData{busy}})
	h = model.(*home)

	require.Equal(t, stateConfirm, h.state, "the snapshot reconcile must not dismiss the confirm overlay")
	require.Equal(t, session.OpArchiving, inst.GetInFlightOp(),
		"the reconcile must adopt the other client's OpArchiving onto the row")
	require.Equal(t, session.LiveRunning, inst.GetLiveness(),
		"the row stays LiveRunning while the other client's archive is in flight (not yet LiveArchived)")

	// Confirm. BeginArchive refuses on the op clause (s.op == OpNone is false),
	// but the row's liveness is NOT LiveArchived, so the fix's guard does NOT match
	// and the closure falls through to emit startArchiveMsg — dispatching the RPC
	// the daemon refuses with its accurate "busy; try again" modal.
	h.confirmationOverlay.OnConfirm()
	require.NotNil(t, h.pendingConfirmMsg, "a busy-op refusal must NOT be suppressed (the daemon's busy modal is accurate feedback)")
	emit, ok := h.pendingConfirmMsg.(startArchiveMsg)
	require.True(t, ok, "the pending confirm message must be a startArchiveMsg")
	require.Equal(t, inst.ID, emit.target.id, "the emit must target the same session")
	require.Equal(t, session.OpArchiving, inst.GetInFlightOp(),
		"the row keeps the adopted OpArchiving (the refused confirm-time transition does not change the op)")
}
