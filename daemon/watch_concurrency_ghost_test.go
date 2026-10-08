package daemon

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// The counter's universe (#1892).
//
// The transition table says what a task run does as its session MOVES. These
// cover a case that makes no transitions at all: a run that exists ON DISK and
// never becomes an in-memory Instance, because FromInstanceData failed — a
// vanished worktree, an unresolvable backend, a wedged tmux name.
//
// refreshDaemonInstances logs and skips such a row, so everything that walks
// m.instances behaves as though the session does not exist. The cap counted by
// walking that map, so a task whose sessions failed to load admitted replacements
// past max_concurrent_runs on every daemon restart — silently, and precisely
// against the restart-survival guarantee the cap is built on. A row we could not
// LOAD is not a run that stopped: its agent may still be up, and the persisted
// marker is the authority on whether the run is in flight.

// failLoadFor makes titles fail to materialize during refresh, exactly as a
// broken row does in production (see the "daemon skipping instance" branch).
// fromInstanceDataForRefresh is a package var for this purpose.
func failLoadFor(t *testing.T, titles ...string) {
	t.Helper()
	broken := make(map[string]bool, len(titles))
	for _, title := range titles {
		broken[title] = true
	}
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(data session.InstanceData) (*session.Instance, error) {
		if broken[data.Title] {
			return nil, errors.New("worktree path is empty")
		}
		return prev(data)
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
}

// TestGhostTaskRunIsCountedAfterRestart is the regression: a persisted, in-flight
// task run that cannot be materialized must still hold its slot, so the cap binds
// across a daemon restart instead of being bypassed by it.
func TestGhostTaskRunIsCountedAfterRestart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "ghost", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The run is in flight and persisted that way.
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()
	if !inst.TaskRunActive() {
		t.Fatal("precondition: a freshly created task session's run is in flight")
	}
	manager.persistInstance(repo.ID, inst)

	// The daemon restarts, and this row will not load.
	failLoadFor(t, data.Title)
	restarted, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if err := restarted.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}

	// It is genuinely invisible to the in-memory map — that is the whole premise.
	restarted.mu.Lock()
	_, live := restarted.instances[daemonInstanceKey(repo.ID, data.Title)]
	counted := restarted.countTaskRunsLocked(repo.ID, "task1")
	restarted.mu.Unlock()
	if live {
		t.Fatal("precondition: the row must have failed to materialize for this to test anything")
	}
	if counted != 1 {
		t.Fatalf("a persisted in-flight run that failed to load must still be counted; got %d "+
			"(the cap now admits replacements beyond max_concurrent_runs after every restart)", counted)
	}

	// The consequence that matters: the cap still binds.
	restarted.mu.Lock()
	err = restarted.admitTaskRunLocked(repo.ID, "task1", limit)
	restarted.mu.Unlock()
	if !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create while the task's only run is an unloadable row: want the at-limit refusal, got %v", err)
	}
}

// TestGhostTaskRunReleasesWhenItsRunIsOver: the ghost count follows the same
// persisted fact as everything else. A row whose run already FINISHED holds
// nothing, even if it fails to load — otherwise an unloadable but completed
// session would wedge a capped task forever, which is the failure mode this PR
// keeps having to avoid.
func TestGhostTaskRunReleasesWhenItsRunIsOver(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "done-ghost", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()

	// The run finishes, and that is what reaches disk.
	if err := inst.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("ready: %v", err)
	}
	manager.persistInstance(repo.ID, inst)

	failLoadFor(t, data.Title)
	restarted, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if err := restarted.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}

	restarted.mu.Lock()
	counted := restarted.countTaskRunsLocked(repo.ID, "task1")
	err = restarted.admitTaskRunLocked(repo.ID, "task1", limit)
	restarted.mu.Unlock()
	if counted != 0 {
		t.Fatalf("a finished run holds no slot, loadable or not; counted %d", counted)
	}
	if err != nil {
		t.Fatalf("a new event must land: an unloadable row whose run already finished must not wedge the task: %v", err)
	}
}

// TestStartupUnknownGhostDoesNotHoldTaskRunSlot covers contradictory rows from
// the rollout window: StartupStateUnknown is terminal even if an older writer
// left TaskRunActive set. Ghost accounting reads raw storage because the row did
// not load, so it must honor the terminal marker directly rather than wedging a
// task behind a bit no in-memory lifecycle transition can ever clear.
func TestStartupUnknownGhostDoesNotHoldTaskRunSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	const title = "startup-unknown-ghost"
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                  "startup-unknown-id",
		TaskID:              "task1",
		Title:               title,
		Path:                repoPath,
		Status:              session.Lost,
		Liveness:            session.LiveLost,
		TaskRunActive:       true,
		StartupStateUnknown: true,
		BackendType:         "local",
		Worktree:            session.GitWorktreeData{RepoPath: repoPath},
	}); err != nil {
		t.Fatalf("append startup-unknown row: %v", err)
	}
	failLoadFor(t, title)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	manager.mu.Lock()
	counted := manager.countTaskRunsLocked(repo.ID, "task1")
	admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
	manager.mu.Unlock()
	if counted != 0 {
		t.Fatalf("startup-unknown ghost consumed %d task slot(s); terminal startup outcomes must release the cap", counted)
	}
	if admitErr != nil {
		t.Fatalf("startup-unknown ghost blocked the next task event: %v", admitErr)
	}
}

// TestUserKilledGhostDoesNotHoldTaskRunSlot covers the kill tombstone (#1108) on
// the raw-row path. The loaded-instance half already frees this slot —
// holdsTaskRunSlot defers to canAutoRestoreLostSession, which refuses a UserKilled
// record ("finish-this-kill, never restore-this"). Ghost accounting reads storage
// directly, so it has to reach the same verdict on its own or the two halves
// disagree about the same session.
//
// Disagreeing here is unrecoverable rather than merely wrong. A tombstone is
// cleared by FINISHING the kill, and finishUserKill only ever runs against an
// instance in m.instances — the one place a ghost is by definition absent. So the
// bit that would release the slot can only be cleared by the path the row cannot
// reach, and the cap stays at its limit for as long as the row is unloadable:
// every later event for that task parks forever. That is the same
// wedged-cap failure TestGhostTaskRunReleasesWhenItsRunIsOver and
// TestStartupUnknownGhostDoesNotHoldTaskRunSlot exist to prevent, arrived at
// through the third terminal marker (#2418).
func TestUserKilledGhostDoesNotHoldTaskRunSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	const title = "tombstoned-ghost"
	// The wedge as it reaches disk: the user killed the session, so the tombstone
	// is committed, but the run marker was never cleared — the kill did not get to
	// finish. Nothing in the row can ever clear it once the row stops loading.
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:            "tombstoned-id",
		TaskID:        "task1",
		Title:         title,
		Path:          repoPath,
		Status:        session.Lost,
		Liveness:      session.LiveLost,
		TaskRunActive: true,
		UserKilled:    true,
		BackendType:   "local",
		Worktree:      session.GitWorktreeData{RepoPath: repoPath},
	}); err != nil {
		t.Fatalf("append tombstoned row: %v", err)
	}
	failLoadFor(t, title)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	manager.mu.Lock()
	_, live := manager.instances[daemonInstanceKey(repo.ID, title)]
	counted := manager.countTaskRunsLocked(repo.ID, "task1")
	admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
	manager.mu.Unlock()
	if live {
		t.Fatal("precondition: the row must have failed to materialize for this to test a ghost")
	}
	if counted != 0 {
		t.Fatalf("tombstoned ghost consumed %d task slot(s); an explicit kill releases the cap", counted)
	}
	if admitErr != nil {
		t.Fatalf("tombstoned ghost wedged the task's cap — no later event can ever land: %v", admitErr)
	}
}

// TestRestoreGaveUpGhostDoesNotHoldTaskRunSlot keeps raw-row accounting aligned
// with holdsTaskRunSlot. A terminal Lost restore releases the task cap when its
// Instance loads; it must do the same when startup cannot materialize the row,
// or no in-memory lifecycle edge can ever release the resulting ghost slot.
func TestRestoreGaveUpGhostDoesNotHoldTaskRunSlot(t *testing.T) {
	tests := []struct {
		name      string
		stableID  string
		breakLoad bool
	}{
		{name: "instance materialization fails", stableID: "restore-gave-up-id", breakLoad: true},
		{name: "legacy id persistence fails"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := setupControlRepo(t)
			repo, err := config.RepoFromPath(repoPath)
			if err != nil {
				t.Fatalf("RepoFromPath: %v", err)
			}
			const title = "restore-gave-up-ghost"
			if err := appendInstanceData(repo.ID, session.InstanceData{
				ID:            tc.stableID,
				TaskID:        "task1",
				Title:         title,
				Path:          repoPath,
				Status:        session.Lost,
				Liveness:      session.LiveLost,
				TaskRunActive: true,
				LostRestoreFailure: &session.LostRestoreFailure{
					Attempts: lostRestoreMaxAttempts,
					Error:    "agent exited at startup",
				},
				BackendType: "local",
				Worktree:    session.GitWorktreeData{RepoPath: repoPath},
			}); err != nil {
				t.Fatalf("append terminal restore row: %v", err)
			}
			if tc.breakLoad {
				failLoadFor(t, title)
			} else {
				previous := persistLegacyInstanceID
				persistLegacyInstanceID = func(string, session.InstanceData) error {
					return errors.New("stable id persistence failed")
				}
				t.Cleanup(func() { persistLegacyInstanceID = previous })
			}

			manager, err := NewManager(config.DefaultConfig())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			if err := manager.RestoreInstances(); err != nil {
				t.Fatalf("RestoreInstances: %v", err)
			}
			manager.mu.Lock()
			_, live := manager.instances[daemonInstanceKey(repo.ID, title)]
			counted := manager.countTaskRunsLocked(repo.ID, "task1")
			admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
			manager.mu.Unlock()
			if live {
				t.Fatal("precondition: terminal row unexpectedly materialized")
			}
			if counted != 0 {
				t.Fatalf("terminal restore ghost consumed %d task slot(s); give-up must release the cap", counted)
			}
			if admitErr != nil {
				t.Fatalf("terminal restore ghost blocked the next task event: %v", admitErr)
			}
		})
	}
}

// TestGhostTaskRunClearsWhenTheRowLoadsAgain: the ghost set is a projection,
// rebuilt every refresh — not bookkeeping. A row that starts loading again must
// stop being a ghost, or its slot would be held twice: once by the ghost and once
// by the instance it became.
func TestGhostTaskRunClearsWhenTheRowLoadsAgain(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "flaky", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()
	manager.persistInstance(repo.ID, inst)

	// A transient load failure: counted as a ghost.
	restore := fromInstanceDataForRefresh
	failLoadFor(t, data.Title)
	restarted, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if err := restarted.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	restarted.mu.Lock()
	ghosted := restarted.countTaskRunsLocked(repo.ID, "task1")
	restarted.mu.Unlock()
	if ghosted != 1 {
		t.Fatalf("the unloadable row must be counted once; got %d", ghosted)
	}

	// The row loads on the next refresh. It must be counted once — as an instance
	// now, not as an instance PLUS a stale ghost.
	fromInstanceDataForRefresh = restore
	restarted.mu.Lock()
	if err := restarted.refreshLocked(); err != nil {
		restarted.mu.Unlock()
		t.Fatalf("refresh: %v", err)
	}
	healed := restarted.countTaskRunsLocked(repo.ID, "task1")
	restarted.mu.Unlock()
	if healed != 1 {
		t.Fatalf("a row that loads again is counted once, not twice (ghost + instance); got %d", healed)
	}
}

// TestSandboxLostGhostDoesNotHoldTaskRunSlot covers the sandbox-backed LiveLost
// row on the raw-row path. The loaded-instance half already frees this slot:
// FromInstanceData loads a non-archived sandbox session inert (started stays
// false), so holdsTaskRunSlot defers to canAutoRestoreLostSession, which refuses
// it (ValidateRuntimeAction(RecoverLost) rejects a !Started session).
//
// Ghost accounting reads storage directly because the row did not load, so it
// must reach the same verdict on its own or the two halves disagree about the
// same session. Disagreeing here is unrecoverable rather than merely wrong: no
// in-memory Instance exists to run the lifecycle edge that would clear
// TaskRunActive, so the cap stays wedged at its limit for as long as the row is
// unloadable — every later event for that task parks forever. That is the same
// wedged-cap failure the StartupStateUnknown, UserKilled, and RestoreGaveUp
// guards exist to prevent, reached through a LiveLost row whose known
// materialized form is started=false by design.
//
// The row reaches disk the moment a running sandbox session is observed Lost
// (ObserveLiveness preserves TaskRunActive — its run ends on the Ready edge,
// which the Lost edge never fires); the bug bites only when that row then fails
// to load on the next daemon restart (a broken worktree path or an unresolvable
// relocation-recovery record) — the same load-failure seam every guard above
// uses. Each sandbox backend type is covered because LostSandboxRecord keys on
// the backend being any re-provisionable sandbox runtime.
func TestSandboxLostGhostDoesNotHoldTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := setupControlRepo(t)
			repo, err := config.RepoFromPath(repoPath)
			if err != nil {
				t.Fatalf("RepoFromPath: %v", err)
			}
			title := "lost-" + backend + "-ghost"
			// A sandbox-backed row persisted mid-run when its remote workspace died:
			//   - Liveness=LiveLost, TaskRunActive=true (ObserveLiveness preserves the
			//     marker — runEndsOnIdleEdge only fires on the Ready edge).
			//   - No LostRestoreFailure (the retry loop never reached give-up), no
			//     UserKilled, no StartupStateUnknown — none of the existing release
			//     sentinels fire.
			if err := appendInstanceData(repo.ID, session.InstanceData{
				ID:            "lost-" + backend + "-id",
				TaskID:        "task1",
				Title:         title,
				Path:          repoPath,
				Status:        session.Lost,
				Liveness:      session.LiveLost,
				TaskRunActive: true,
				BackendType:   backend,
				Worktree: session.GitWorktreeData{
					RepoPath:     repoPath,
					WorktreePath: "/dev/null",
					SessionName:  title,
					BranchName:   "af/" + title,
				},
			}); err != nil {
				t.Fatalf("append %s lost row: %v", backend, err)
			}
			failLoadFor(t, title)

			manager, err := NewManager(config.DefaultConfig())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			if err := manager.RestoreInstances(); err != nil {
				t.Fatalf("RestoreInstances: %v", err)
			}
			manager.mu.Lock()
			_, live := manager.instances[daemonInstanceKey(repo.ID, title)]
			counted := manager.countTaskRunsLocked(repo.ID, "task1")
			admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
			manager.mu.Unlock()
			if live {
				t.Fatal("precondition: the row must have failed to materialize for this to test a ghost")
			}
			if rawTaskRunHoldsSlot(session.InstanceData{
				TaskID:        "task1",
				TaskRunActive: true,
				Liveness:      session.LiveLost,
				BackendType:   backend,
			}) {
				t.Fatalf("rawTaskRunHoldsSlot still HOLDS for %s LiveLost; the fix must release a sandbox LiveLost row whose materialized form loads started=false", backend)
			}
			if counted != 0 {
				t.Fatalf("%s lost ghost consumed %d task slot(s); a sandbox LiveLost row's materialized form releases (started=false), so its ghost must too", backend, counted)
			}
			if admitErr != nil {
				t.Fatalf("%s lost ghost wedged the task's cap — no later event can ever land: %v", backend, admitErr)
			}
		})
	}
}

// TestLocalLostGhostStillHoldsTaskRunSlot confirms the sandbox release is scoped:
// a LOCAL-backed LiveLost row without LostRestoreFailure still holds. Its
// materialized form loads started=true (a local session restart does not tear its
// worktree down), so canAutoRestoreLostSession keeps retrying it and
// holdsTaskRunSlot HOLDS. rawTaskRunHoldsSlot must agree — LostSandboxRecord is
// false for a local backend, so the added release term is a no-op here. Without
// this agreement the cap would UNDERCOUNT a Lost session the restore loop is
// still actively reviving, letting a capped watcher admit a replacement that
// blows the cap the moment a retry lands.
func TestLocalLostGhostStillHoldsTaskRunSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	const title = "local-lost-ghost"
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:            "local-lost-id",
		TaskID:        "task1",
		Title:         title,
		Path:          repoPath,
		Status:        session.Lost,
		Liveness:      session.LiveLost,
		TaskRunActive: true,
		BackendType:   "local",
		Worktree:      session.GitWorktreeData{RepoPath: repoPath},
	}); err != nil {
		t.Fatalf("append local lost row: %v", err)
	}
	failLoadFor(t, title)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	manager.mu.Lock()
	_, live := manager.instances[daemonInstanceKey(repo.ID, title)]
	counted := manager.countTaskRunsLocked(repo.ID, "task1")
	admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
	manager.mu.Unlock()
	if live {
		t.Fatal("precondition: the row must have failed to materialize for this to test a ghost")
	}
	if counted != 1 {
		t.Fatalf("local lost ghost consumed %d task slot(s); a local LiveLost row whose restore loop can still revive it must hold the cap (the sandbox release must not over-release)", counted)
	}
	if admitErr == nil {
		t.Fatalf("local lost ghost must still refuse admission; the restore loop can revive it, so the cap must bind")
	}
}

// TestSandboxLostGhostWithPendingHandoffHoldsTaskRunSlot is the other half of the
// sandbox LiveLost release: a LiveLost row whose materialized form does NOT release
// must not be released by the raw arm either. A durable pending handoff
// (PromptNotDelivered) reconstructs OpReplacing on load, so the loaded instance's
// Activity is Pending and holdsTaskRunSlot HOLDS — even though the sandbox loaded
// inert (started=false), the run is still in flight through the handoff, not
// settled. Releasing the ghost here would let a replacement past
// max_concurrent_runs while the original task transaction is still pending, so the
// raw arm must mirror the live arm's Pending verdict rather than the inert-load
// release. ClassifyActivity is the raw-record reader's activity oracle, so the raw
// arm sees the same Pending a loaded Instance would.
func TestSandboxLostGhostWithPendingHandoffHoldsTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := setupControlRepo(t)
			repo, err := config.RepoFromPath(repoPath)
			if err != nil {
				t.Fatalf("RepoFromPath: %v", err)
			}
			title := "lost-" + backend + "-handoff-ghost"
			// A sandbox-backed LiveLost row carrying a durable pending handoff whose
			// delivery was never confirmed: FromInstanceData reconstructs OpReplacing
			// from the mission, so the loaded form holds the slot through ActivityPending
			// even though the sandbox loaded started=false. The raw arm must agree.
			if err := appendInstanceData(repo.ID, session.InstanceData{
				ID:                    "lost-" + backend + "-handoff-id",
				TaskID:                "task1",
				Title:                 title,
				Path:                  repoPath,
				Status:                session.Lost,
				Liveness:              session.LiveLost,
				TaskRunActive:         true,
				BackendType:           backend,
				PendingHandoffMission: "continue the exact inherited work",
				HandoffDeliveryStatus: session.PromptNotDelivered,
				Worktree: session.GitWorktreeData{
					RepoPath:     repoPath,
					WorktreePath: "/dev/null",
					SessionName:  title,
					BranchName:   "af/" + title,
				},
			}); err != nil {
				t.Fatalf("append %s lost handoff row: %v", backend, err)
			}
			failLoadFor(t, title)

			manager, err := NewManager(config.DefaultConfig())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			if err := manager.RestoreInstances(); err != nil {
				t.Fatalf("RestoreInstances: %v", err)
			}
			manager.mu.Lock()
			_, live := manager.instances[daemonInstanceKey(repo.ID, title)]
			counted := manager.countTaskRunsLocked(repo.ID, "task1")
			admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
			manager.mu.Unlock()
			if live {
				t.Fatal("precondition: the row must have failed to materialize for this to test a ghost")
			}
			if !rawTaskRunHoldsSlot(session.InstanceData{
				TaskID:                "task1",
				TaskRunActive:         true,
				Liveness:              session.LiveLost,
				BackendType:           backend,
				PendingHandoffMission: "continue the exact inherited work",
				HandoffDeliveryStatus: session.PromptNotDelivered,
			}) {
				t.Fatalf("rawTaskRunHoldsSlot released a %s LiveLost row whose materialized form reconstructs OpReplacing and holds the slot; only terminal sandbox ghosts release", backend)
			}
			if counted != 1 {
				t.Fatalf("%s lost handoff ghost consumed %d task slot(s); a pending-handoff LiveLost row's materialized form holds (ActivityPending), so its ghost must too", backend, counted)
			}
			if admitErr == nil {
				t.Fatalf("%s lost handoff ghost must still refuse admission; the pending handoff keeps the run in flight", backend)
			}
		})
	}
}

// TestLegacySandboxLostGhostReleasesTaskRunSlot covers a pre-#1195 sandbox row on
// the raw-row path: a record written before the liveness field carries only the
// legacy Status (Lost) and LivenessUnset. FromInstanceData rolls that status
// forward to LiveLost and loads a non-archived sandbox row inert, so the live arm
// releases its slot; the raw arm must reach the same verdict through the SAME
// rollforward (EffectiveLiveness), not a direct data.Liveness read that misses
// the legacy row and leaves its ghost wedging the task cap.
func TestLegacySandboxLostGhostReleasesTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			// A pre-liveness row: Liveness is the zero value, Status is Lost. The
			// loader rolls this forward to LiveLost; LostSandboxRecord must agree, or
			// a materialization-failure ghost of this row would hold a slot its
			// loaded form releases.
			legacy := session.InstanceData{
				TaskID:        "task1",
				Title:         "legacy-lost-" + backend,
				TaskRunActive: true,
				Status:        session.Lost,
				Liveness:      session.LivenessUnset,
				BackendType:   backend,
			}
			if !session.LostSandboxRecord(legacy) {
				t.Fatalf("LostSandboxRecord missed a legacy Status=Lost %s row (LivenessUnset); it must use the effective liveness the loader rolls forward to LiveLost", backend)
			}
			if rawTaskRunHoldsSlot(legacy) {
				t.Fatalf("rawTaskRunHoldsSlot held a legacy Status=Lost %s row whose rolled-forward liveness is LiveLost; the raw arm must release the same sandbox ghost the loader loads inert", backend)
			}
		})
	}
}

// TestSandboxLostGhostWithAmbiguousHandoffReleasesTaskRunSlot is the third leg of
// the sandbox LiveLost handoff scoping: a LiveLost row whose PendingHandoffMission
// carries an ambiguous delivery verdict — PromptCouldNotConfirm, or the missing
// evidence a legacy record carries (no HandoffDeliveryStatus at all) — does NOT
// reconstruct OpReplacing on load. FromInstanceData's fence reconstruction
// (pendingHandoffMissionNeedsFence) admits only PromptNotDelivered and
// PromptDelivered; an ambiguous verdict loads with InFlightOp=OpNone, so the
// loaded sandbox LiveLost row is terminal and holdsTaskRunSlot releases.
//
// ClassifyActivity on the raw record returns ActivityPending for the non-empty
// PendingHandoffMission regardless of the verdict, so without LoadedActivity the
// raw arm would HOLD while the live arm releases — the same wedged-cap mismatch
// the PR fixes. LoadedActivity mirrors LifecycleView.Activity() composed with
// FromInstanceData's InFlightOp reconstruction, so the raw arm sees the terminal
// verdict a loaded Instance would.
func TestSandboxLostGhostWithAmbiguousHandoffReleasesTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	cases := []struct {
		name    string
		mission string
		status  session.PromptDeliveryStatus
	}{
		{name: "missing-evidence", mission: "continue the inherited work", status: ""},
		{name: "could-not-confirm", mission: "continue the inherited work", status: session.PromptCouldNotConfirm},
		{name: "sent-unverified", mission: "continue the inherited work", status: session.PromptSentUnverified},
	}
	for _, backend := range sandboxBackends {
		for _, tc := range cases {
			t.Run(backend+"-"+tc.name, func(t *testing.T) {
				row := session.InstanceData{
					TaskID:                "task1",
					TaskRunActive:         true,
					Liveness:              session.LiveLost,
					BackendType:           backend,
					PendingHandoffMission: tc.mission,
					HandoffDeliveryStatus: tc.status,
				}
				if rawTaskRunHoldsSlot(row) {
					t.Fatalf("rawTaskRunHoldsSlot held a %s LiveLost row with ambiguous handoff (%s); the loader does not reconstruct OpReplacing, so its materialized form is terminal and the raw arm must release", backend, tc.name)
				}
			})
		}
	}
}

// TestSandboxRunningLostGhostReleasesTaskRunSlot covers a sandbox row persisted
// LiveRunning that then ghosts (a legacy-ID persistence failure or relocation
// metadata that prevents materialization). The stored liveness is NOT LiveLost,
// so LostSandboxRecord alone would hold it; but FromInstanceData's non-archived
// sandbox branch unconditionally rewrites every such row to inert LiveLost on
// load, after which holdsTaskRunSlot releases it. The raw arm must reach the same
// verdict through the loader's rewrite (session.LoadedActivity), not require the
// stored liveness to be lost already, or the common pre-restart running record
// wedges max_concurrent_runs indefinitely on the raw path.
func TestSandboxRunningLostGhostReleasesTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			row := session.InstanceData{
				TaskID:        "task1",
				TaskRunActive: true,
				Liveness:      session.LiveRunning,
				BackendType:   backend,
			}
			activity, _ := session.LoadedActivity(row)
			if activity != session.ActivityTerminal {
				t.Fatalf("LoadedActivity reported %v for a %s LiveRunning row; the loader rewrites a non-archived sandbox row to inert LiveLost, so its activity is terminal", activity, backend)
			}
			if rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot held a %s LiveRunning row whose loaded form is rewritten to inert LiveLost and releases; the raw arm must release the same pre-restart running ghost the loader loads inert", backend)
			}
		})
	}
}

// TestSandboxLostGhostWithPendingAccountSwapHoldsTaskRunSlot covers the account-swap
// half of the sandbox ghost scoping. ForStorage projects StartupStateUnknown=true
// onto a pending-account-swap row and stores the real value inside the swap
// record, so a raw reader that short-circuits on the projected StartupStateUnknown
// releases the slot while the loaded form restores the fence and HOLDS (its
// pending account swap is still in flight). The raw arm must delegate to
// releasesLostSandboxGhost, which restores the fence through LoadedActivity,
// instead of acting on the projected value before the restore runs.
func TestSandboxLostGhostWithPendingAccountSwapHoldsTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			originalStartup := false
			row := session.InstanceData{
				TaskID:              "task1",
				TaskRunActive:       true,
				Liveness:            session.LiveLost,
				BackendType:         backend,
				StartupStateUnknown: true,
				PendingAccountSwap:  &session.AccountSwapData{OriginalStartupStateUnknown: &originalStartup},
			}
			activity, _ := session.LoadedActivity(row)
			if activity != session.ActivityPending {
				t.Fatalf("LoadedActivity reported %v for a %s LiveLost row with a pending account swap; restoring the fence must surface the pending swap the loader holds on", activity, backend)
			}
			if !rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot released a %s LiveLost row whose materialized form restores the account-swap fence and holds; the raw projected StartupStateUnknown must not short-circuit before LoadedActivity restores it", backend)
			}
		})
	}
}

// TestSandboxLostGhostWithArchiveRollbackFenceHoldsTaskRunSlot covers an
// unloadable sandbox LiveLost row that carries an archive rollback fence together
// with a PromptNotDelivered handoff. The persisted StartupStateUnknown is the
// projected true, not the session's real value; FromInstanceData calls
// RestoreArchiveRollbackFence first, recovering false and reconstructing
// OpReplacing, so the loaded form holds. LoadedActivity must mirror that restore
// order or it would read the projected true, refuse to rebuild the fence, and
// report terminal — releasing a ghost whose handoff is still in flight.
func TestSandboxLostGhostWithArchiveRollbackFenceHoldsTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			row := session.InstanceData{
				TaskID:                "task1",
				TaskRunActive:         true,
				Liveness:              session.LiveLost,
				BackendType:           backend,
				StartupStateUnknown:   true,
				PendingHandoffMission: "continue the exact inherited work",
				HandoffDeliveryStatus: session.PromptNotDelivered,
				ArchiveReport: &git.ArchiveReport{
					RollbackFence: &git.ArchiveRollbackFence{OriginalStartupStateUnknown: false},
				},
			}
			activity, _ := session.LoadedActivity(row)
			if activity != session.ActivityPending {
				t.Fatalf("LoadedActivity reported %v for a %s LiveLost row whose archive fence restores StartupStateUnknown=false and reconstructs OpReplacing; it must hold like the loaded form", activity, backend)
			}
			if !rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot released a %s LiveLost row whose archive fence restores a PromptNotDelivered handoff; LoadedActivity must restore the archive fence before classifying, or the projected StartupStateUnknown drops a ghost whose handoff is still in flight", backend)
			}
		})
	}
}

// TestSandboxLostGhostWithUndecodableRelocationRecoveryHoldsTaskRunSlot covers a
// sandbox LiveLost row whose relocation-recovery metadata cannot be decoded. The
// loader itself would fail to materialize such a row, so a row that cannot load is
// not a row whose run has settled; LoadedActivity reports ActivityPending for it
// rather than guessing terminal, so the raw arm holds the slot conservatively
// instead of letting a replacement past the cap while the real verdict is unknown.
func TestSandboxLostGhostWithUndecodableRelocationRecoveryHoldsTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			row := session.InstanceData{
				TaskID:        "task1",
				TaskRunActive: true,
				Liveness:      session.LiveLost,
				BackendType:   backend,
				Worktree: session.GitWorktreeData{
					// A non-nil recovery whose original ownership fields are missing
					// is exactly the shape RestoreRelocationRecoveryOriginals refuses.
					RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{},
				},
			}
			activity, _ := session.LoadedActivity(row)
			if activity != session.ActivityPending {
				t.Fatalf("LoadedActivity reported %v for a %s row with undecodable relocation recovery; an unloadable row's run has not settled, so it must hold conservatively", activity, backend)
			}
			if !rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot released a %s row whose relocation recovery cannot be decoded; the loader cannot materialize it, so the raw arm must hold the slot rather than guess terminal", backend)
			}
		})
	}
}

// TestSandboxLostGhostWithUserKilledAndUndecodableRelocationReleasesTaskRunSlot
// covers the kill tombstone (#1108) on a sandbox row whose relocation-recovery
// metadata cannot be decoded. An undecodable relocation record is otherwise held
// conservatively (TestSandboxLostGhostWithUndecodableRelocationRecoveryHoldsTaskRunSlot),
// but a committed kill is a definitive terminal marker the loader honors before
// any fence: ClassifyActivity returns terminal for UserKilled first, and the
// sandbox arm of rawTaskRunHoldsSlot now routes through LoadedActivity instead of
// the raw UserKilled guard, so LoadedActivity must honor the tombstone before the
// relocation-error early return — or the tombstoned ghost holds the slot forever
// (it cannot materialize to run finishUserKill and clear the stale TaskRunActive).
func TestSandboxLostGhostWithUserKilledAndUndecodableRelocationReleasesTaskRunSlot(t *testing.T) {
	sandboxBackends := []string{"ssh", "docker", "sandbox", "remote"}
	for _, backend := range sandboxBackends {
		t.Run(backend, func(t *testing.T) {
			row := session.InstanceData{
				TaskID:        "task1",
				TaskRunActive: true,
				Liveness:      session.LiveLost,
				UserKilled:    true,
				BackendType:   backend,
				Worktree: session.GitWorktreeData{
					// A non-nil recovery whose original ownership fields are missing
					// is exactly the shape RestoreRelocationRecoveryOriginals refuses.
					RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{},
				},
			}
			activity, _ := session.LoadedActivity(row)
			if activity != session.ActivityTerminal {
				t.Fatalf("LoadedActivity reported %v for a %s row with a committed kill and undecodable relocation recovery; UserKilled is terminal before the relocation verdict, so the tombstone must release", activity, backend)
			}
			if rawTaskRunHoldsSlot(row) {
				t.Fatalf("rawTaskRunHoldsSlot held a %s row with a committed kill and undecodable relocation recovery; the kill tombstone is terminal, so the raw arm must release it", backend)
			}
		})
	}
}
