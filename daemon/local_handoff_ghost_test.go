package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The pending-handoff local ghost (#1892), the sibling the
// pending-account-swap ghost fix (c2c7dc84) left uncovered.
//
// rawTaskRunHoldsSlot is the storage-only mirror of holdsTaskRunSlot: it tells
// refreshDaemonInstances whether a row that failed to materialize (a "ghost")
// still occupies a max_concurrent_runs slot for its task. ForStorage projects
// StartupStateUnknown=true onto ANY row whose PendingHandoffMission carries an
// ambiguous delivery verdict (PromptCouldNotConfirm, PromptSentUnverified, or
// the missing evidence a legacy record carries, which
// restoreMissingHandoffMissionEvidence normalizes to PromptCouldNotConfirm before
// the projection runs) — no backend gate
// (projectPendingHandoffForPreviousRelease) — so a local task session
// mid-handoff is persisted with StartupStateUnknown=true for as long as the
// mission keeps the run's idle edge held. A current reader restores the fence
// (RestoreHandoffRollbackFence) before deciding, so the materialized form
// (holdsTaskRunSlot via LifecycleView.Activity) keeps the slot: a local row does
// NOT get the inert LiveLost rewrite the sandbox loader applies, so a stored
// LiveRunning classifies ActivityPending and a recoverable LiveLost is held by
// canAutoRestoreLostSession. The raw arm must agree, or a local ghost
// mid-handoff is released from the cap and admitTaskRunLocked admits a
// replacement beyond max_concurrent_runs.
//
// The local backend runs agent handoffs (Capabilities().Handoff is true), so the
// projection reaches local rows mid-mission, unlike the account-swap projection
// which only a local backend can commit. The undercount never self-corrects:
// refreshDaemonInstances rebuilds ghostTaskRuns from scratch on every call and
// re-evaluates rawTaskRunHoldsSlot against the same on-disk row, so the slot is
// leaked for as long as the row remains unloadable — across daemon restarts and
// across every poll tick, exactly the accepted premise of the sibling fix.

// TestLocalPendingHandoffGhostReleasesRawArm is the raw-level divergence: a local
// row carrying a pending handoff mission whose delivery verdict is ambiguous (with
// the ForStorage-projected StartupStateUnknown=true) has a materialized form that
// HOLDS — LoadedActivity mirrors LifecycleView.Activity composed with the loader's
// fence restoration, so restoring the handoff fence surfaces the held idle edge the
// loaded local row keeps, yielding ActivityPending. rawTaskRunHoldsSlot must return
// true (must not short-circuit on the projected StartupStateUnknown before the
// handoffFence term runs), or the raw arm and the materialized form disagree about
// the same row and the cap undercounts the ghost. Each case is written through
// ForStorage so the row matches the exact on-disk shape a ghost carries, including
// the missing-evidence normalization ("") to PromptCouldNotConfirm.
func TestLocalPendingHandoffGhostReleasesRawArm(t *testing.T) {
	cases := []struct {
		name   string
		status session.PromptDeliveryStatus
	}{
		{name: "could-not-confirm", status: session.PromptCouldNotConfirm},
		{name: "sent-unverified", status: session.PromptSentUnverified},
		{name: "missing-evidence", status: ""}, // ForStorage normalizes to PromptCouldNotConfirm
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The in-memory shape before a storage write: the run is in flight,
			// the local agent is running the inherited mission, and the delivery
			// verdict is ambiguous. StartupStateUnknown is the real false.
			before := session.InstanceData{
				TaskID:                "task1",
				TaskRunActive:         true,
				Liveness:              session.LiveRunning,
				BackendType:           "local",
				PendingHandoffMission: "continue the inherited work",
				HandoffDeliveryStatus: tc.status,
				StartupStateUnknown:   false,
			}
			// ForStorage applies the handoff rollback projection: it normalizes a
			// missing verdict to PromptCouldNotConfirm and sets StartupStateUnknown=true,
			// capturing the original false in HandoffOriginalStartupStateUnknown. This
			// is the exact on-disk shape refreshDaemonInstances hands to the raw arm.
			row := before.ForStorage()
			if row.HandoffOriginalStartupStateUnknown == nil {
				t.Fatalf("precondition: ForStorage did not project the handoff fence for %q (verdict %q); the projection must fire for every ambiguous mission", tc.name, tc.status)
			}
			if row.StartupStateUnknown != true {
				t.Fatalf("precondition: ForStorage left StartupStateUnknown=%v for %q; the projected value is what the raw arm must not short-circuit on", row.StartupStateUnknown, tc.name)
			}
			// The materialized oracle: the loader restores the handoff fence
			// (recovering the original StartupStateUnknown=false) and the local
			// backend keeps the persisted LiveRunning, so the loaded form is Pending
			// — exactly the verdict holdsTaskRunSlot reaches for a live instance.
			activity, _ := session.LoadedActivity(row)
			if activity != session.ActivityPending {
				t.Fatalf("LoadedActivity reported %v for a local LiveRunning row with a pending handoff (%s); restoring the fence must surface the held idle edge the loaded form holds on", activity, tc.name)
			}
			if !rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot released a local row whose materialized form HOLDS (Pending) for %s; the projected StartupStateUnknown short-circuits before the handoffFence term and undercounts the ghost", tc.name)
			}
		})
	}
}

// TestLocalPendingHandoffGhostReleasesRawArmLiveLost mirrors the above for a
// recoverable LiveLost local row, the second shape the bug report establishes
// diverges. The loaded form releases on the Activity axis (ClassifyActivity
// calls LiveLost terminal), but holdsTaskRunSlot still HOLDS through
// canAutoRestoreLostSession — the restore loop can revive a local Lost session
// with no give-up marker — so the raw arm must hold the same on-disk shape.
// This is the shape the sandbox arm deliberately releases (the sandbox loader
// rewrites a non-archived row to inert LiveLost, so its loaded form is terminal
// with no recover path); only the local arm diverges from its materialized form.
func TestLocalPendingHandoffGhostReleasesRawArmLiveLost(t *testing.T) {
	before := session.InstanceData{
		TaskID:                "task1",
		TaskRunActive:         true,
		Liveness:              session.LiveLost,
		BackendType:           "local",
		PendingHandoffMission: "continue the inherited work",
		HandoffDeliveryStatus: session.PromptCouldNotConfirm,
		StartupStateUnknown:   false,
	}
	// The on-disk shape after the handoff projection: StartupStateUnknown=true
	// with the original false captured. No LostRestoreFailure, so the restore
	// loop has not given up.
	row := before.ForStorage()
	if row.HandoffOriginalStartupStateUnknown == nil || row.StartupStateUnknown != true {
		t.Fatalf("precondition: ForStorage did not project the handoff fence onto the LiveLost row; got marker=%v startup=%v", row.HandoffOriginalStartupStateUnknown, row.StartupStateUnknown)
	}
	// The loaded form's Activity is Terminal for LiveLost, but the run's slot is
	// held through canAutoRestoreLostSession (recoverable local Lost, no give-up).
	// LoadedActivity reports the Activity axis only; it deliberately does NOT
	// compose the cap's canAutoRestoreLostSession arm, because that arm needs the
	// backend's Recover capability a raw record cannot reconstruct. So the oracle
	// here is the Agreement that the raw arm must not release before the trailing
	// IdleReasonRestoreGaveUp guard: a LiveLost row without a give-up marker is
	// exactly the row the restore loop is still actively reviving.
	if got := session.IdleReasonFor(row); got == session.IdleReasonRestoreGaveUp {
		t.Fatalf("precondition: a LiveLost row without LostRestoreFailure must not read as RestoreGaveUp; got %v", got)
	}
	if !rawTaskRunHoldsSlot(row) {
		t.Fatal("rawTaskRunHoldsSlot released a recoverable local LiveLost row whose materialized form HOLDS through canAutoRestoreLostSession; the cap undercounts the ghost the restore loop is still reviving")
	}
}

// TestLocalPendingHandoffGhostUndercountEndToEnd is the end-to-end restart
// regression. Mirroring TestLocalPendingAccountSwapGhostUndercountEndToEnd and
// TestLocalLostGhostStillHoldsTaskRunSlot, it writes a local row carrying a
// pending handoff mission through appendInstanceData (which applies ForStorage,
// normalizing the missing evidence to PromptCouldNotConfirm and projecting
// StartupStateUnknown=true — exactly the on-disk shape the daemon persists while
// a handoff mission is outstanding), breaks loading via failLoadFor, restarts
// through NewManager + RestoreInstances, and asserts the ghost is still counted
// against the task's cap and a replacement is refused. Without the handoffFence
// term in the raw arm, countTaskRunsLocked returns 0 and admitTaskRunLocked
// returns nil — the cap is violated end-to-end for as long as the row remains a
// ghost.
func TestLocalPendingHandoffGhostUndercountEndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	const title = "local-handoff-ghost"
	// The on-disk shape of a local task session mid-handoff: TaskRunActive=true
	// (the mission holds the run's idle edge open), a pending handoff mission
	// whose delivery carries the missing-evidence verdict (""), and a
	// StartupStateUnknown that appendInstanceData's ForStorage projection will
	// normalize to PromptCouldNotConfirm and set to true — exactly the shape the
	// daemon persists from the swap path through every save while the mission is
	// outstanding.
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                    "local-handoff-ghost-id",
		TaskID:                "task1",
		Title:                 title,
		Path:                  repoPath,
		Status:                session.Lost,
		Liveness:              session.LiveLost,
		TaskRunActive:         true,
		BackendType:           "local",
		PendingHandoffMission: "continue the inherited work",
		HandoffDeliveryStatus: "", // ForStorage normalizes to PromptCouldNotConfirm
		Worktree:              session.GitWorktreeData{RepoPath: repoPath},
	}); err != nil {
		t.Fatalf("append local handoff row: %v", err)
	}
	failLoadFor(t, title)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}

	// The row is genuinely invisible to the in-memory map — that is the ghost
	// premise, and the only thing that could count it is m.ghostTaskRuns.
	manager.mu.Lock()
	_, live := manager.instances[daemonInstanceKey(repo.ID, title)]
	counted := manager.countTaskRunsLocked(repo.ID, "task1")
	admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
	manager.mu.Unlock()
	if live {
		t.Fatal("precondition: the row must have failed to materialize for this to test a ghost")
	}
	if counted != 1 {
		t.Fatalf("local pending-handoff ghost consumed %d task slot(s); the materialized form holds (the mission keeps the run in flight) but rawTaskRunHoldsSlot read the projected StartupStateUnknown and released the ghost, undercounting the cap", counted)
	}
	if !isAtConcurrencyLimitErr(admitErr) {
		t.Fatalf("a local ghost mid-handoff must still refuse admission; the pending mission keeps its run in flight, so the cap must bind: got %v", admitErr)
	}
}

// TestLocalPendingHandoffGhostStartupAlreadyUnknownReleases is the finding the
// handoffFence must not re-raise a hold the loaded form releases: when the
// startup state was already unknown BEFORE the handoff (MarkStartupStateUnknown
// ran before the ambiguous delivery), ForStorage captures the original true in
// HandoffOriginalStartupStateUnknown and leaves StartupStateUnknown=true. A
// current reader restores the fence to true, and ClassifyActivity returns
// ActivityTerminal for a StartupStateUnknown row before it ever consults
// liveness — so the materialized form releases its slot (holdsTaskRunSlot:
// ActivityTerminal, and canAutoRestoreLostSession is false because
// ValidateRuntimeAction refuses a StartupStateUnknown row). An unconditional
// handoffFence would hold the ghost solely because the mission is ambiguous,
// and with no Instance to run the lifecycle edge that clears TaskRunActive the
// capped task wedges forever. The marker gate (HandoffOriginalStartupStateUnknown
// proving the original was false) is exactly what separates this shape from the
// mid-handoff ghost the fence is for: the projection did not raise
// StartupStateUnknown here, so the loaded form is terminal and the raw arm must
// agree.
func TestLocalPendingHandoffGhostStartupAlreadyUnknownReleases(t *testing.T) {
	cases := []struct {
		name     string
		liveness session.Liveness
	}{
		// ClassifyActivity returns ActivityTerminal for StartupStateUnknown
		// before the liveness switch, so the liveness does not change the
		// verdict — a LiveRunning row restores to the same terminal the loaded
		// form would settle on for any liveness.
		{name: "live-running", liveness: session.LiveRunning},
		{name: "live-lost", liveness: session.LiveLost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The in-memory shape before a storage write whose startup was
			// already marked unknown when the ambiguous handoff landed:
			// MarkStartupStateUnknown ran before the delivery verdict, so
			// StartupStateUnknown is the real true, and the run is still in
			// flight. ForStorage captures that true in the rollback marker and
			// leaves StartupStateUnknown=true — the on-disk shape a ghost
			// carries while the mission is outstanding.
			before := session.InstanceData{
				TaskID:                "task1",
				TaskRunActive:         true,
				Liveness:              tc.liveness,
				BackendType:           "local",
				PendingHandoffMission: "continue the inherited work",
				HandoffDeliveryStatus: session.PromptCouldNotConfirm,
				StartupStateUnknown:   true, // the original state, already unknown
			}
			row := before.ForStorage()
			if row.HandoffOriginalStartupStateUnknown == nil || *row.HandoffOriginalStartupStateUnknown != true {
				t.Fatalf("precondition: ForStorage did not capture the original true startup state for %s; the marker gate reads this value, got %v", tc.name, row.HandoffOriginalStartupStateUnknown)
			}
			if row.StartupStateUnknown != true {
				t.Fatalf("precondition: ForStorage left StartupStateUnknown=%v for %s; the projection of an already-unknown state must keep it true", row.StartupStateUnknown, tc.name)
			}
			// The materialized oracle: restoring the fence recovers the original
			// true, so the loaded form is terminal (ClassifyActivity returns
			// ActivityTerminal for StartupStateUnknown before the liveness switch),
			// and canAutoRestoreLostSession is false for a StartupStateUnknown row
			// (ValidateRuntimeAction refuses it). The raw arm must not re-raise the
			// hold a terminal loaded form releases, or the cap wedges on a ghost
			// no lifecycle edge can clear.
			if activity, _ := session.LoadedActivity(row); activity != session.ActivityTerminal {
				t.Fatalf("LoadedActivity reported %v for a StartupStateUnknown=true %s row; restoring the fence must surface the terminal the loaded form settles on, not the held idle edge a false-original row keeps", activity, tc.name)
			}
			if rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot held a local %v row whose startup was already unknown before the handoff for %s; the marker proves the original state was true, so the loaded form is terminal and the fence must not re-raise the hold (a ghost that never materializes would wedge the cap forever)", tc.liveness, tc.name)
			}
		})
	}
}

// TestLocalPendingHandoffGhostReleasesOnIdleOrTerminalLiveness is the second
// shape the handoffFence must not re-raise: a row whose loaded form RELEASES after
// the fence is restored. The projection raised StartupStateUnknown from false to
// true (the marker proves the original was false), so a blind handoffFence would
// hold the row — but the loaded form restores the fence, reconstructs no op (an
// ambiguous verdict never rebuilds OpReplacing), and the local backend does not
// rewrite liveness, so ClassifyActivity reaches the liveness switch and the
// materialized form releases:
//   - LiveReady: the run went idle (no op, the mission is not an in-flight
//     obligation), so ActivityIdle releases the slot.
//   - LiveArchived: ActivityTerminal with no restore path (canAutoRestoreLostSession
//     is for Lost only), so the slot frees.
//
// A blind handoffFence would hold the same row if it failed to materialize, and
// no in-memory lifecycle edge can clear TaskRunActive for a ghost, so the capped
// task wedges forever. The fence mirrors the loaded form's activity
// (handoffFenceLoadedFormHolds replays the fence restoration through LoadedActivity
// and only re-raises a hold the loaded form would keep), so the raw arm releases
// the same row the materialized form releases.
func TestLocalPendingHandoffGhostReleasesOnIdleOrTerminalLiveness(t *testing.T) {
	cases := []struct {
		name     string
		liveness session.Liveness
		activity session.Activity
	}{
		{name: "live-ready", liveness: session.LiveReady, activity: session.ActivityIdle},
		{name: "live-archived", liveness: session.LiveArchived, activity: session.ActivityTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The in-memory shape before a storage write: the run is in flight,
			// the local agent is mid-handoff with an ambiguous verdict, and the
			// startup state is the real false (the projection will raise it).
			// ForStorage normalizes the verdict to PromptCouldNotConfirm and
			// projects StartupStateUnknown=true, capturing the original false in
			// the rollback marker — exactly the on-disk shape a ghost carries.
			before := session.InstanceData{
				TaskID:                "task1",
				TaskRunActive:         true,
				Liveness:              tc.liveness,
				BackendType:           "local",
				PendingHandoffMission: "continue the inherited work",
				HandoffDeliveryStatus: session.PromptCouldNotConfirm,
				StartupStateUnknown:   false,
			}
			row := before.ForStorage()
			if row.HandoffOriginalStartupStateUnknown == nil || *row.HandoffOriginalStartupStateUnknown != false {
				t.Fatalf("precondition: ForStorage did not project the handoff fence for %s; the marker must capture the original false so the fence restores a loadable state, got %v", tc.name, row.HandoffOriginalStartupStateUnknown)
			}
			if row.StartupStateUnknown != true {
				t.Fatalf("precondition: ForStorage left StartupStateUnknown=%v for %s; the projected value is what the raw arm must not short-circuit on", row.StartupStateUnknown, tc.name)
			}
			// The materialized oracle: restoring the fence recovers the original
			// false, the ambiguous verdict reconstructs no op, and the local
			// backend does not rewrite liveness, so LoadedActivity classifies the
			// row by its liveness — ActivityIdle for a LiveReady row whose mission
			// went idle, ActivityTerminal for a LiveArchived row with no restore
			// path. holdsTaskRunSlot releases both (ActivityIdle is not Pending,
			// and canAutoRestoreLostSession is for Lost only), so the raw arm must
			// not re-raise a hold the loaded form releases.
			if activity, _ := session.LoadedActivity(row); activity != tc.activity {
				t.Fatalf("LoadedActivity reported %v for a local %v row with a pending handoff (%s); restoring the fence must surface the %v the loaded form settles on, not the held idle edge a LiveRunning row keeps", activity, tc.liveness, tc.name, tc.activity)
			}
			if rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot held a local %v row whose loaded form releases (%v) for %s; the handoffFence must mirror the loaded form's activity and not re-raise a hold a %v row releases, or a ghost that never materializes wedges the cap forever", tc.liveness, tc.activity, tc.name, tc.liveness)
			}
		})
	}
}

// TestLocalPendingHandoffGhostHonorsFutureVerdictMarker is the version-skew gate.
// A newer release may add another ambiguous delivery verdict this binary has no
// constant for; its writer still projects the handoff fence (its
// ambiguousHandoffDelivery list includes the new verdict), setting
// StartupStateUnknown=true and capturing the original false in
// HandoffOriginalStartupStateUnknown. This older reader does not recognize the
// verdict, but the marker proves a compatible writer projected the fence, and
// LoadedActivity re-raises the mission for an unrecognized nonempty verdict
// (ClassifyActivity returns ActivityPending for any non-empty
// PendingHandoffMission), so the loaded form HOLDS. The raw arm must honor the
// marker without requiring the current binary to recognize the verdict, or a
// local running ghost persisted with a future verdict is released from the cap
// and admitTaskRunLocked admits a replacement beyond max_concurrent_runs after a
// rollback to this older binary.
//
// The row is constructed directly in its on-disk shape (rather than through
// ForStorage) because the projection this binary ships does not yet know the
// future verdict; a future writer's projection is what the marker stands in for.
func TestLocalPendingHandoffGhostHonorsFutureVerdictMarker(t *testing.T) {
	originalFalse := false
	row := session.InstanceData{
		TaskID:                             "task1",
		TaskRunActive:                      true,
		Liveness:                           session.LiveRunning,
		BackendType:                        "local",
		PendingHandoffMission:              "continue the inherited work",
		HandoffDeliveryStatus:              session.PromptDeliveryStatus("future-ambiguous"),
		StartupStateUnknown:                true,
		HandoffOriginalStartupStateUnknown: &originalFalse,
	}
	// The materialized oracle: the loader restores the fence (recovering the
	// original false), does not recognize the verdict, and re-raises the
	// mission for an unrecognized nonempty verdict, so ClassifyActivity returns
	// ActivityPending — the held idle edge the loaded local row keeps.
	activity, _ := session.LoadedActivity(row)
	if activity != session.ActivityPending {
		t.Fatalf("LoadedActivity reported %v for a local LiveRunning row with a future-ambiguous handoff verdict; an unrecognized nonempty verdict must re-raise the mission and keep the loaded form pending", activity)
	}
	if !rawTaskRunHoldsSlot(row) {
		t.Fatalf("rawTaskRunHoldsSlot released a local row whose marker proves a compatible writer projected the handoff fence for a future verdict; the fence must honor the marker without requiring the current binary to recognize the verdict, or the cap undercounts a future-verdict ghost")
	}
}

// TestLocalPendingHandoffGhostReleasesInertRelocationLiveLost is the second
// shape the handoffFence must not re-raise: a local LiveLost row whose
// relocation-recovery record decodes but still represents a pending relocation.
// FromInstanceData keeps such a row inert (Started=false) — the persisted
// worktree path is not authoritative while a relocation is pending — so
// ValidateRuntimeAction(RuntimeActionRecoverLost) refuses it and
// canAutoRestoreLostSession returns false: the loaded form RELEASES its slot.
// LoadedActivity reports ActivityTerminal for LiveLost (the local backend does
// not rewrite liveness), so the handoffFenceLoadedFormHolds LiveLost fallback
// is the only thing that could re-raise the hold. Equating every LiveLost row
// with the restore loop holds this inert row, and with no Instance to clear
// TaskRunActive the capped task wedges forever. The fallback gates on the
// loader's relocation eligibility (RestoreRelocationRecoveryOriginals leaves a
// real pending relocation in place), so the raw arm releases the same row the
// loaded form releases.
//
// The row carries a decodable relocation-recovery record (a real pending
// relocation, not the worktree-missing rollback fence the restore clears), so
// the restored record still has RelocationRecovery != nil — the one shape the
// loader keeps inert.
func TestLocalPendingHandoffGhostReleasesInertRelocationLiveLost(t *testing.T) {
	originalFalse := false
	branchTrue := true
	externalFalse := false
	row := session.InstanceData{
		TaskID:                             "task1",
		TaskRunActive:                      true,
		Liveness:                           session.LiveLost,
		BackendType:                        "local",
		PendingHandoffMission:              "continue the inherited work",
		HandoffDeliveryStatus:              session.PromptCouldNotConfirm,
		StartupStateUnknown:                true,
		HandoffOriginalStartupStateUnknown: &originalFalse,
		Worktree: session.GitWorktreeData{
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State:                       sessiongit.RelocationRecoveryStalled,
				OriginalExternalWorktree:    &externalFalse,
				OriginalBranchCreatedByUs:   &branchTrue,
				OriginalStartupStateUnknown: &originalFalse,
			},
		},
	}
	// Precondition: the relocation record decodes and remains a pending
	// relocation (not the worktree-missing fence, which the restore clears),
	// so the loader keeps this row inert (Started=false).
	restored, err := row.RestoreRelocationRecoveryOriginals()
	if err != nil {
		t.Fatalf("precondition: the relocation record must decode for this to be the inert shape; got %v", err)
	}
	if restored.Worktree.RelocationRecovery == nil {
		t.Fatalf("precondition: the restored record must still carry the pending relocation; a nil record is the worktree-missing fence shape the loader starts, not the inert relocation shape this test is for")
	}
	// The materialized oracle: the loaded form is terminal (LiveLost, no inert
	// rewrite for the local backend) and the row loads inert (Started=false),
	// so canAutoRestoreLostSession refuses it and holdsTaskRunSlot releases.
	activity, _ := session.LoadedActivity(row)
	if activity != session.ActivityTerminal {
		t.Fatalf("LoadedActivity reported %v for a local LiveLost row; ClassifyActivity calls LiveLost terminal, and the local backend does not rewrite liveness, so the loaded form's Activity is terminal", activity)
	}
	if rawTaskRunHoldsSlot(row) {
		t.Fatalf("rawTaskRunHoldsSlot held a local LiveLost row whose relocation-recovery record keeps the loader inert (Started=false); canAutoRestoreLostSession refuses it and the loaded form releases, so the raw arm must release too or the ghost wedges max_concurrent_runs forever")
	}
}

// TestLocalPendingHandoffGhostReleasesUndecodableRelocation is the third shape
// the handoffFenceLoadedFormHolds relocation gate must not re-raise: a local row
// whose relocation-recovery record cannot be decoded (here, missing rollback-safety
// originals — RestoreRelocationRecoveryOriginals errors with "rollback safety
// metadata is missing"). FromInstanceData rejects such a row at the same
// RestoreRelocationRecoveryOriginals boundary, so no Instance or restore loop can
// ever materialize it and clear its TaskRunActive. LoadedActivity returns
// ActivityPending for a non-sandbox row it cannot read a loaded form for, so the
// LoadedActivityPending early return in handoffFenceLoadedFormHolds would hold the
// slot — and with no in-memory lifecycle edge to release it, the capped task
// wedges forever. The undecodable guard releases the unmaterializable shape
// before LoadedActivity is consulted, mirroring the loader's rejection.
//
// The row carries the handoff fence (HandoffOriginalStartupStateUnknown proving
// the original state was false) so the marker gate passes and a LiveRunning
// liveness so the only thing that could re-raise the hold is the
// LoadedActivityPending early return; the undecodable relocation recovery is what
// makes it unmaterializable.
func TestLocalPendingHandoffGhostReleasesUndecodableRelocation(t *testing.T) {
	originalFalse := false
	row := session.InstanceData{
		TaskID:                             "task1",
		TaskRunActive:                      true,
		Liveness:                           session.LiveRunning,
		BackendType:                        "local",
		PendingHandoffMission:              "continue the inherited work",
		HandoffDeliveryStatus:              session.PromptCouldNotConfirm,
		StartupStateUnknown:                true,
		HandoffOriginalStartupStateUnknown: &originalFalse,
		Worktree: session.GitWorktreeData{
			// A recovery record with a real stall state but none of the
			// rollback-safety originals (OriginalExternalWorktree/
			// OriginalBranchCreatedByUs/OriginalStartupStateUnknown) the
			// restore path requires — RestoreRelocationRecoveryOriginals
			// errors with "rollback safety metadata is missing". This is the
			// shape the loader rejects outright (FromInstanceData fails at the
			// same boundary), so no Instance or restore loop can clear
			// TaskRunActive.
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State: sessiongit.RelocationRecoveryStalled,
			},
		},
	}
	// Precondition: the relocation record is undecodable, so FromInstanceData
	// rejects this row and no Instance or restore loop can materialize it.
	if _, err := row.RestoreRelocationRecoveryOriginals(); err == nil {
		t.Fatalf("precondition: the relocation record must be undecodable for this to be the unmaterializable shape; got no error")
	}
	// Precondition: LoadedActivity reports ActivityPending for the non-sandbox
	// row it cannot read a loaded form for — the early return the
	// undecodable guard must bypass.
	activity, _ := session.LoadedActivity(row)
	if activity != session.ActivityPending {
		t.Fatalf("LoadedActivity reported %v for a local row with an undecodable relocation record; the non-sandbox err branch must report pending for the unmaterializable shape the fence must not re-raise", activity)
	}
	if rawTaskRunHoldsSlot(row) {
		t.Fatalf("rawTaskRunHoldsSlot held a local row whose relocation-recovery record is undecodable (FromInstanceData rejects it, so no Instance or restore loop can clear TaskRunActive); LoadedActivity returns ActivityPending for the non-sandbox shape, but the loaded form never exists, so holding it wedges max_concurrent_runs forever on a ghost no in-memory lifecycle edge can release")
	}
}

// TestLocalPendingHandoffGhostArchiveFenceRestoresRelocation is the first shape
// the archive-rollback fence must be restored before the relocation checks for:
// a local LiveLost row whose ambiguous handoff also has a non-empty archive report.
// ForStorage replaces Worktree.RelocationRecovery with the synthetic archive kill
// fence (archiveReportKillFence, a ClaimStale with the retained tree's identity)
// and preserves the real recovery only in ArchiveReport.RollbackFence; for a row
// whose original recovery was nil, the real recovery is nil, so
// RestoreArchiveRollbackFence recovers nil. handoffFenceLoadedFormHolds must
// restore the archive fence before inspecting relocation — the same order
// FromInstanceData and LoadedActivity use — or the LiveLost fallback sees the
// synthetic fence (which isWorktreeMissingRollbackFence does not recognize,
// because it carries the retained tree's non-zero identity) as a real pending
// relocation and releases the row. The loaded form counts it: the original
// recovery is nil so the loader starts it and canAutoRestoreLostSession holds the
// recoverable LiveLost row, and the restore loop revives it. Releasing it
// undercounts the cap; the loader's restore loop revives the row and holds its
// slot, so a replacement admit exceeds max_concurrent_runs.
//
// The row is written through ForStorage so it carries the exact on-disk shape a
// ghost carries: the synthetic archive kill fence in Worktree.RelocationRecovery
// and the real (nil) recovery in ArchiveReport.RollbackFence.
func TestLocalPendingHandoffGhostArchiveFenceRestoresRelocation(t *testing.T) {
	// The in-memory shape before a storage write: a local LiveLost row mid-handoff
	// with a non-empty archive report and no real relocation recovery. ForStorage
	// projects the handoff fence (StartupStateUnknown=true with the original false
	// captured) and the archive kill fence (Worktree.RelocationRecovery replaced
	// by archiveReportKillFence, the real nil recovery preserved only in
	// ArchiveReport.RollbackFence with RelocationRecoveryProjected=true).
	before := session.InstanceData{
		TaskID:                "task1",
		TaskRunActive:         true,
		Liveness:              session.LiveLost,
		BackendType:           "local",
		PendingHandoffMission: "continue the inherited work",
		HandoffDeliveryStatus: session.PromptCouldNotConfirm,
		StartupStateUnknown:   false,
		ArchiveReport: &sessiongit.ArchiveReport{
			RetainedTrees: []sessiongit.ArchiveRetainedTree{{
				Path:          "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
				IdentityKnown: true, Device: 1, Inode: 2, FileType: 0o040000,
			}},
			// RollbackFence deliberately nil: the disk-reload recapture branch of
			// ForStorage captures it from the row before the pending projections,
			// preserving the original (no relocation recovery) and the original
			// startup state.
		},
	}
	row := before.ForStorage()
	if row.ArchiveReport == nil || row.ArchiveReport.RollbackFence == nil {
		t.Fatalf("precondition: ForStorage did not project the archive rollback fence for the LiveLost row; the archive fence must carry the original recovery for the test to exercise the shape")
	}
	if !row.ArchiveReport.RollbackFence.RelocationRecoveryProjected {
		t.Fatalf("precondition: the archive fence must project a relocation recovery so RestoreArchiveRollbackFence recovers the real (nil) recovery")
	}
	if row.ArchiveReport.RollbackFence.OriginalRelocationRecovery != nil {
		t.Fatalf("precondition: the original recovery must be nil for this to be the synthetic-fence shape the loaded form counts; got %v", row.ArchiveReport.RollbackFence.OriginalRelocationRecovery)
	}
	if row.Worktree.RelocationRecovery == nil {
		t.Fatalf("precondition: ForStorage must publish the synthetic archive kill fence in Worktree.RelocationRecovery; without it the row does not exercise the bug (the synthetic fence is what the raw arm sees instead of the real nil recovery)")
	}
	if row.HandoffOriginalStartupStateUnknown == nil || *row.HandoffOriginalStartupStateUnknown != false {
		t.Fatalf("precondition: ForStorage must project the handoff fence with the original false startup state for the LiveLost row; got %v", row.HandoffOriginalStartupStateUnknown)
	}
	// The materialized oracle: RestoreArchiveRollbackFence recovers the real
	// (nil) recovery, so the loaded form's LiveLost row has no relocation the
	// loader keeps inert — it starts the row and canAutoRestoreLostSession holds
	// it through the restore loop. A non-empty ArchiveReport does not change
	// that: the loader restores the archive fence before any relocation decision,
	// so the real nil recovery is what it sees.
	restored := row.RestoreArchiveRollbackFence()
	if restored.Worktree.RelocationRecovery != nil {
		t.Fatalf("precondition: RestoreArchiveRollbackFence must recover the real (nil) recovery; the synthetic fence survives in Worktree.RelocationRecovery and the test no longer exercises the bug (got %v)", restored.Worktree.RelocationRecovery)
	}
	// Precondition: the loaded form holds this row. RestoreArchiveRollbackFence
	// recovers the real (nil) recovery, so the local LiveLost row has no
	// relocation the loader keeps inert — it starts the row and
	// canAutoRestoreLostSession holds it through the restore loop. ClassifyActivity
	// calls LiveLost terminal, so LoadedActivity reports ActivityTerminal (the
	// local backend does not rewrite liveness), and the handoffFence's LiveLost
	// fallback is what holds it — exactly the on-disk shape the loaded form holds
	// and the raw arm must agree on.
	activity, _ := session.LoadedActivity(row)
	if activity != session.ActivityTerminal {
		t.Fatalf("precondition: LoadedActivity must report ActivityTerminal for the local LiveLost row whose archive fence recovers a nil recovery; got %v (the LiveLost fallback is the only hold the fence re-raises)", activity)
	}
	if !rawTaskRunHoldsSlot(row) {
		t.Fatalf("rawTaskRunHoldsSlot released a local LiveLost row whose archive fence recovers a nil relocation recovery (the loader starts the row and canAutoRestoreLostSession holds it through the restore loop); without restoring the archive fence first, the LiveLost fallback sees the synthetic archive kill fence as a real pending relocation and releases the ghost, undercounting the cap")
	}
}

// TestLocalPendingHandoffGhostArchiveFenceUndecodableOriginal is the second
// shape the archive-rollback fence must be restored before the relocation
// checks for: a local row whose ArchiveReport.RollbackFence carries a malformed
// original recovery (its rollback-safety originals are missing), so the real
// recovery is undecodable. ForStorage publishes the synthetic archive kill fence
// (a valid ClaimStale with the retained tree's identity) in
// Worktree.RelocationRecovery, so the raw undecodable guard — which runs
// RestoreRelocationRecoveryOriginals on the synthetic fence without restoring
// the archive fence first — sees a decodable record and accepts it. LoadedActivity
// restores the archive fence internally and reports ActivityPending for the
// non-sandbox err branch, so the LoadedActivityPending early return holds an
// unmaterializable ghost forever: the loader restores the same malformed original
// and rejects it at RestoreRelocationRecoveryOriginals, so no Instance or restore
// loop can clear TaskRunActive. Restoring the archive fence first lets the
// undecodable guard see the malformed original the loader sees and release the
// row the loaded form can never materialize.
//
// The row carries a LiveRunning liveness so LoadedActivity's non-sandbox err
// branch reports ActivityPending (the verdict the early return would hold on),
// and the handoff fence marker so handoffFenceLoadedFormHolds is the gate.
func TestLocalPendingHandoffGhostArchiveFenceUndecodableOriginal(t *testing.T) {
	branchTrue := true
	// The on-disk shape ForStorage produces for a row whose archive report's
	// rollback fence preserves a malformed real recovery (a Stalled record with
	// none of the rollback-safety originals RestoreRelocationRecoveryOriginals
	// requires) and a synthetic archive kill fence in Worktree.RelocationRecovery.
	// The retained tree's identity makes the synthetic fence a ClaimStale with a
	// non-zero identity, so isWorktreeMissingRollbackFence does not recognize it
	// and RestoreRelocationRecoveryOriginals decodes it — the shape the raw guard
	// must not accept after the archive fence is restored.
	row := session.InstanceData{
		TaskID:                             "task1",
		TaskRunActive:                      true,
		Liveness:                           session.LiveRunning,
		BackendType:                        "local",
		PendingHandoffMission:              "continue the inherited work",
		HandoffDeliveryStatus:              session.PromptCouldNotConfirm,
		StartupStateUnknown:                true,
		HandoffOriginalStartupStateUnknown: func() *bool { b := false; return &b }(),
		ArchiveReport: &sessiongit.ArchiveReport{
			RetainedTrees: []sessiongit.ArchiveRetainedTree{{
				Path:          "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
				IdentityKnown: true, Device: 1, Inode: 2, FileType: 0o040000,
			}},
			RollbackFence: &sessiongit.ArchiveRollbackFence{
				OriginalStartupStateUnknown: false,
				OriginalExternalWorktree:    false,
				OriginalBranchCreatedByUs:   &branchTrue,
				RelocationRecoveryProjected: true,
				// A real but malformed recovery: a Stalled state with none of the
				// rollback-safety originals RestoreRelocationRecoveryOriginals
				// requires, so the loader rejects it at the same boundary the
				// raw guard must restore the archive fence to reach.
				OriginalRelocationRecovery: &sessiongit.ArchiveRollbackRelocationRecovery{
					State: sessiongit.RelocationRecoveryStalled,
				},
			},
		},
	}
	// Precondition: the synthetic archive kill fence is a decodable ClaimStale
	// the raw undecodable guard would accept without the archive fence restored,
	// but the real recovery it preserves is undecodable.
	if _, err := row.RestoreRelocationRecoveryOriginals(); err != nil {
		t.Fatalf("precondition: the synthetic archive kill fence must decode for the raw guard to accept it without the archive fence; got %v", err)
	}
	restored := row.RestoreArchiveRollbackFence()
	if _, err := restored.RestoreRelocationRecoveryOriginals(); err == nil {
		t.Fatalf("precondition: the real recovery the archive fence recovers must be undecodable (the loader rejects it at RestoreRelocationRecoveryOriginals); got no error")
	}
	// The materialized oracle: LoadedActivity restores the archive fence
	// internally, so it sees the malformed original and reports ActivityPending
	// for the non-sandbox err branch — the verdict the early return would hold
	// on a row whose loaded form never materializes.
	activity, _ := session.LoadedActivity(row)
	if activity != session.ActivityPending {
		t.Fatalf("precondition: LoadedActivity must report ActivityPending for the non-sandbox row with an undecodable relocation the loader rejects; got %v (the LoadedActivityPending early return is the hold the fence must not re-raise)", activity)
	}
	if rawTaskRunHoldsSlot(row) {
		t.Fatalf("rawTaskRunHoldsSlot held a local row whose archive fence recovers a malformed relocation recovery the loader rejects at RestoreRelocationRecoveryOriginals; no Instance or restore loop can clear TaskRunActive, so the cap wedges forever on the unmaterializable ghost")
	}
}

// TestLocalPendingHandoffGhostRuntimeInvalidRelocation is the fourth shape the
// handoffFenceLoadedFormHolds relocation gate must not re-raise: a local
// LiveRunning row whose relocation-recovery record decodes (passes
// RestoreRelocationRecoveryOriginals) but is runtime-invalid at the loader's
// GitWorktree.RestoreRelocationRecovery — a move_unknown with an empty
// alternate path. FromInstanceData installs the recovery through
// GitWorktree.RestoreRelocationRecovery, which rejects the empty alternate
// path, so the row never materializes and no lifecycle edge can clear
// TaskRunActive. LoadedActivity only runs RestoreRelocationRecoveryOriginals,
// which decodes the rollback metadata but not the runtime shape, so it reports
// ActivityPending for the local LiveRunning row and the LoadedActivityPending
// early return holds the slot. The runtime-installable gate mirrors
// GitWorktree.RestoreRelocationRecovery's state-shape validation on the
// restored record, so the fence releases the same unmaterializable shape the
// loader rejects instead of wedging the cap on a ghost no Instance can clear.
//
// The row carries the handoff fence marker so handoffFenceLoadedFormHolds is the
// gate, and a move_unknown with an empty alternate path and the rollback-safety
// originals so RestoreRelocationRecoveryOriginals decodes it (the loader rejects
// it at the later GitWorktree.RestoreRelocationRecovery boundary).
func TestLocalPendingHandoffGhostRuntimeInvalidRelocation(t *testing.T) {
	originalFalse := false
	branchTrue := true
	externalFalse := false
	row := session.InstanceData{
		TaskID:                             "task1",
		TaskRunActive:                      true,
		Liveness:                           session.LiveRunning,
		BackendType:                        "local",
		PendingHandoffMission:              "continue the inherited work",
		HandoffDeliveryStatus:              session.PromptCouldNotConfirm,
		StartupStateUnknown:                true,
		HandoffOriginalStartupStateUnknown: &originalFalse,
		Worktree: session.GitWorktreeData{
			// A move_unknown with an empty alternate path: the rollback-safety
			// originals decode (so RestoreRelocationRecoveryOriginals succeeds),
			// but GitWorktree.RestoreRelocationRecovery rejects it for the empty
			// alternate path, so FromInstanceData never materializes an Instance.
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State:                       sessiongit.RelocationRecoveryMoveUnknown,
				IdentityKnown:               true,
				OriginalExternalWorktree:    &externalFalse,
				OriginalBranchCreatedByUs:   &branchTrue,
				OriginalStartupStateUnknown: &originalFalse,
			},
		},
	}
	// Precondition: the relocation record decodes (the rollback-safety
	// originals are present), so the undecodable guard does not release it.
	if _, err := row.RestoreRelocationRecoveryOriginals(); err != nil {
		t.Fatalf("precondition: the relocation record must decode for the runtime-invalid shape (the rollback-safety originals are present); got %v", err)
	}
	restored, _ := row.RestoreRelocationRecoveryOriginals()
	if restored.Worktree.RelocationRecovery == nil {
		t.Fatalf("precondition: the restored record must still carry the pending relocation (a nil record is the worktree-missing fence shape, not the runtime-invalid move_unknown this test is for)")
	}
	if restored.Worktree.RelocationRecovery.AlternatePath != "" {
		t.Fatalf("precondition: the move_unknown record must have an empty alternate path so GitWorktree.RestoreRelocationRecovery rejects it; got %q", restored.Worktree.RelocationRecovery.AlternatePath)
	}
	// The materialized oracle: LoadedActivity reports ActivityPending for the
	// local LiveRunning row (the local backend does not rewrite liveness, and
	// the handoff fence is not up because the row has no InFlightOp). The loader
	// would reject the row at GitWorktree.RestoreRelocationRecovery, so no
	// Instance materializes — the fence must not hold a slot no lifecycle edge
	// can clear.
	activity, _ := session.LoadedActivity(row)
	if activity != session.ActivityPending {
		t.Fatalf("precondition: LoadedActivity must report ActivityPending for the local LiveRunning row (the verdict the LoadedActivityPending early return would hold); got %v", activity)
	}
	if rawTaskRunHoldsSlot(row) {
		t.Fatalf("rawTaskRunHoldsSlot held a local row whose relocation record decodes but is runtime-invalid (a move_unknown with an empty alternate path GitWorktree.RestoreRelocationRecovery rejects); no Instance materializes to clear TaskRunActive, so the cap wedges forever on the unmaterializable ghost")
	}
}
