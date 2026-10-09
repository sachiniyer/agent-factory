package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The pending-account-swap local ghost (#1892).
//
// rawTaskRunHoldsSlot is the storage-only mirror of holdsTaskRunSlot: it tells
// refreshDaemonInstances whether a row that failed to materialize (a "ghost")
// still occupies a max_concurrent_runs slot for its task. ForStorage projects
// StartupStateUnknown=true onto ANY row with a pending account swap — no backend
// gate (projectPendingAccountSwapForPreviousRelease) — so a local task session
// mid-swap is persisted with StartupStateUnknown=true for the whole swap window
// (persistSettlement at account_swap.go:512 through the final checkpoint at
// limit.go:907). ClassifyActivity checks PendingAccountSwap BEFORE
// StartupStateUnknown, so the materialized form (holdsTaskRunSlot via
// LifecycleView.Activity) is ActivityPending and HOLDS the slot while the swap is
// in flight. The raw arm must agree, or a local ghost mid-swap is released from the
// cap and admitTaskRunLocked admits a replacement beyond max_concurrent_runs.
//
// The local backend is the only one that can commit an account swap
// (SupportsAutomaticAccountSwap gates on backend.Type() == "local"), so this is
// the arm the cap leaks on. The undercount never self-corrects:
// refreshDaemonInstances rebuilds ghostTaskRuns from scratch on every call and
// re-evaluates rawTaskRunHoldsSlot against the same on-disk row, so the slot is
// leaked for as long as the row remains unloadable — across daemon restarts and
// across every poll tick.

// TestLocalPendingAccountSwapGhostReleasesRawArm is the raw-level divergence: a
// local row carrying a pending account swap (with the ForStorage-projected
// StartupStateUnknown=true) has a materialized form that HOLDS — LoadedActivity
// mirrors LifecycleView.Activity composed with the loader's fence restoration, so
// restoring the account-swap fence surfaces the pending swap ClassifyActivity sees
// BEFORE StartupStateUnknown, yielding ActivityPending. rawTaskRunHoldsSlot must
// return true (must not short-circuit on the projected StartupStateUnknown before
// the PendingAccountSwap term runs), or the raw arm and the materialized form
// disagree about the same row and the cap undercounts the ghost.
func TestLocalPendingAccountSwapGhostReleasesRawArm(t *testing.T) {
	originalStartup := false
	row := session.InstanceData{
		TaskID:              "task1",
		TaskRunActive:       true,
		Liveness:            session.LiveLost,
		BackendType:         "local",
		StartupStateUnknown: true, // the ForStorage-projected value on disk
		PendingAccountSwap:  &session.AccountSwapData{OriginalStartupStateUnknown: &originalStartup},
	}
	// The materialized oracle: the loader restores the account-swap fence
	// (recovering the original StartupStateUnknown=false) and ClassifyActivity
	// sees the pending swap before the restored StartupStateUnknown, so the
	// loaded form is Pending — exactly the verdict holdsTaskRunSlot reaches for a
	// live instance.
	activity, _ := session.LoadedActivity(row)
	if activity != session.ActivityPending {
		t.Fatalf("LoadedActivity reported %v for a local LiveLost row with a pending account swap; restoring the fence must surface the pending swap the loaded form holds on", activity)
	}
	if !rawTaskRunHoldsSlot(row) {
		t.Fatal("rawTaskRunHoldsSlot released a local row whose materialized form HOLDS (Pending); the cap undercounts the ghost")
	}
}

// TestLocalPendingAccountSwapGhostUndercountEndToEnd is the end-to-end restart
// regression. Mirroring TestGhostTaskRunIsCountedAfterRestart and
// TestLocalLostGhostStillHoldsTaskRunSlot, it writes a local row carrying a
// pending account swap through appendInstanceData (which applies ForStorage,
// projecting StartupStateUnknown=true — exactly the on-disk shape
// persistSettlement produces for the whole swap window), breaks loading via
// failLoadFor, restarts through NewManager + RestoreInstances, and asserts the
// ghost is still counted against the task's cap and a replacement is refused.
// Without the PendingAccountSwap term in the raw arm, countTaskRunsLocked returns
// 0 and admitTaskRunLocked returns nil — the cap is violated end-to-end for as
// long as the row remains a ghost.
func TestLocalPendingAccountSwapGhostUndercountEndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	const title = "local-swap-ghost"
	// The on-disk shape of a local task session mid-account-swap: TaskRunActive=true
	// (the run is still in flight), a pending account swap, and a StartupStateUnknown
	// that appendInstanceData's ForStorage projection will set to true (capturing the
	// original false inside the swap record) — exactly the shape persistSettlement
	// writes at account_swap.go:512 and that persists until ClearPendingAccountSwap
	// at limit.go:875.
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                 "local-swap-ghost-id",
		TaskID:             "task1",
		Title:              title,
		Path:               repoPath,
		Status:             session.Lost,
		Liveness:           session.LiveLost,
		TaskRunActive:      true,
		BackendType:        "local",
		PendingAccountSwap: &session.AccountSwapData{},
		Worktree:           session.GitWorktreeData{RepoPath: repoPath},
	}); err != nil {
		t.Fatalf("append local swap row: %v", err)
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
		t.Fatalf("local pending-account-swap ghost consumed %d task slot(s); the materialized form holds (PendingAccountSwap is in flight) but rawTaskRunHoldsSlot read the projected StartupStateUnknown and released the ghost, undercounting the cap", counted)
	}
	if !isAtConcurrencyLimitErr(admitErr) {
		t.Fatalf("a local ghost mid-account-swap must still refuse admission; the pending swap keeps its run in flight, so the cap must bind: got %v", admitErr)
	}
}
