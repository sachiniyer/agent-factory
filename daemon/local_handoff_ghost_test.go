package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The pending-handoff local ghost (#1892), the sibling the
// pending-account-swap ghost fix (c2c7dc84) left uncovered.
//
// rawTaskRunHoldsSlot is the storage-only mirror of holdsTaskRunSlot: it tells
// refreshDaemonInstances whether a row that failed to materialize (a "ghost")
// still occupies a max_concurrent_runs slot for its task. ForStorage projects
// StartupStateUnknown=true onto ANY row whose PendingHandoffMission carries an
// ambiguous delivery verdict (HandoffDeliveryUnresolved: PromptCouldNotConfirm,
// PromptSentUnverified, or the missing evidence a legacy record carries, which
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
