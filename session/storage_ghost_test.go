package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unloadableRelocationRecoveryRow builds a local session row whose persisted
// worktree relocation recovery record is missing its rollback-safety metadata,
// so FromInstanceData deterministically fails with "rollback safety metadata
// is missing" — the exact failure shape a partially-written or version-skewed
// recovery record produces at load. This is one "ghost" row class the daemon
// deliberately leaves on disk for repair (#4162/#3310) and that the shutdown
// checkpoint erased before this fix.
func unloadableRelocationRecoveryRow(t *testing.T, title, repoPath string) InstanceData {
	t.Helper()
	return InstanceData{
		ID: title + "-id", Title: title, Path: repoPath, Program: "claude",
		Status: Running, Liveness: LiveRunning, BackendType: "local",
		Worktree: GitWorktreeData{
			RepoPath:     repoPath,
			WorktreePath: filepath.Join(repoPath, title+"-wt"),
			BranchName:   "af/" + title,
			RelocationRecovery: &GitWorktreeRelocationRecoveryData{
				State: sessiongit.RelocationRecoveryStalled,
				// OriginalExternalWorktree / OriginalBranchCreatedByUs /
				// OriginalStartupStateUnknown intentionally nil so
				// RestoreRelocationRecoveryOriginals fails with "rollback
				// safety metadata is missing" — a real failure mode a
				// partially-written or version-skewed recovery record hits.
			},
		},
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// loadMaterializedLikeDaemon mirrors refreshDaemonInstances' "skip rows that
// fail to materialize" contract for the SaveInstances tests. It reads every
// row the storage backs, attempts FromInstanceData on each exactly as the
// daemon's fromInstanceDataForRefresh does, and returns ONLY the materialized
// Instances. Rows that fail (the "ghost" rows the daemon leaves on disk) are
// skipped here exactly as the daemon skips them out of m.instances — their
// durable records stay on the mock for the save path to preserve (or, before
// the fix, to erase). The returned slice is what Manager.SaveInstancesForShutdown
// would pass: InstancesSnapshot() walks m.instances, which never contained the
// ghost.
func loadMaterializedLikeDaemon(t *testing.T, state *mockInstanceStorage) []*Instance {
	t.Helper()
	all, err := state.GetAllInstances()
	require.NoError(t, err)
	var out []*Instance
	for repoID, raw := range all {
		if raw == nil || string(raw) == "[]" || string(raw) == "null" {
			continue
		}
		var rows []InstanceData
		require.NoError(t, json.Unmarshal(raw, &rows))
		for _, data := range dedupeInstanceData(rows) {
			inst, err := FromInstanceData(data)
			if err != nil {
				// Mirror refreshDaemonInstances: skip out of memory, leave the
				// row on disk for repair or manual cleanup. The save path
				// under test must preserve this row.
				t.Logf("loader skipped instance %q (repo %s): %v", data.Title, repoID, err)
				continue
			}
			inst.PinStorageRepoID(repoID)
			out = append(out, inst)
		}
	}
	return out
}

// rowsByTitle reads a repo's on-disk rows and indexes them by title.
func rowsByTitle(t *testing.T, state *mockInstanceStorage, repoPath string) map[string]InstanceData {
	t.Helper()
	out := make(map[string]InstanceData, len(readDisk(t, state, repoPath)))
	for _, r := range readDisk(t, state, repoPath) {
		out[r.Title] = r
	}
	return out
}

// mustFailToMaterialize asserts that a row fails to load via FromInstanceData,
// establishing the "ghost" precondition: the daemon's loader would skip it out
// of memory while leaving it on disk.
func mustFailToMaterialize(t *testing.T, data InstanceData) {
	t.Helper()
	_, err := FromInstanceData(data)
	require.Error(t, err, "fixture must produce an unloadable row (ghost): %q", data.Title)
}

// mustMaterialize asserts that a row loads via FromInstanceData, establishing
// the "loadable sibling" precondition.
func mustMaterialize(t *testing.T, data InstanceData) *Instance {
	t.Helper()
	inst, err := FromInstanceData(data)
	require.NoError(t, err, "fixture must produce a loadable row: %q", data.Title)
	return inst
}

// TestSaveInstances_PreservesUnloadableGhostAlongsideStartedSibling is the
// core regression for the ghost-erase bug. A repo with one normally-loadable
// in-memory session and one unloadable (ghost) on-disk row must keep BOTH rows
// on disk after the shutdown checkpoint. Before the fix, the wholesale per-repo
// rewrite marshaled only the in-memory instances and silently erased the ghost —
// the only durable record for its worktree, its branch, and its task-run slot.
//
// The started sibling is load-bearing: without an in-memory instance for the
// repo, grouped[rid] is empty and the checkpoint is a no-op, so the bug never
// fires. This test deliberately exercises the mixed-repo path the bug report
// calls out.
func TestSaveInstances_PreservesUnloadableGhostAlongsideStartedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	ghost := unloadableRelocationRecoveryRow(t, "ghost", repoPath)
	seedDisk(t, state, repoPath, []InstanceData{ghost})
	mustFailToMaterialize(t, ghost)

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{alive}))

	got := rowsByTitle(t, state, repoPath)
	require.Contains(t, got, "alive", "the materialized session must be saved")
	require.Contains(t, got, "ghost",
		"the unloadable ghost row must survive the checkpoint: it is the only durable "+
			"handle to its worktree, branch, and task-run slot (#4162/#1892/#3310)")
	assert.Equal(t, ghost.Worktree.WorktreePath, got["ghost"].Worktree.WorktreePath,
		"the preserved ghost row must keep its worktree pointer for repair")
	assert.Equal(t, ghost.Worktree.BranchName, got["ghost"].Worktree.BranchName,
		"the preserved ghost row must keep its branch pointer")
	assert.Equal(t, ghost.ID, got["ghost"].ID,
		"the preserved ghost row must keep its stable identity")
}

// TestSaveInstancesForShutdown_PreservesUnloadableGhost verifies the
// production shutdown path (Manager.SaveInstancesForShutdown) preserves the
// ghost too: SaveInstancesForShutdown is the only production full-save caller,
// invoked on SIGTERM and on every daemon-owned upgrade hand-off.
func TestSaveInstancesForShutdown_PreservesUnloadableGhost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	ghost := unloadableRelocationRecoveryRow(t, "ghost", repoPath)
	seedDisk(t, state, repoPath, []InstanceData{ghost})

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstancesForShutdown([]*Instance{alive}))

	got := rowsByTitle(t, state, repoPath)
	require.Contains(t, got, "ghost",
		"the shutdown checkpoint must preserve the ghost row across a daemon "+
			"restart or upgrade hand-off (#4162/#1892/#3310)")
	require.Contains(t, got, "alive")
}

// TestSaveInstances_PreservesGhostAcrossLoadAndShutdown mirrors the daemon's
// actual load→save flow end to end. It seeds a repo with one loadable row and
// one unloadable row, runs the actual loader (FromInstanceData) per row exactly
// as refreshDaemonInstances does, then calls SaveInstancesForShutdown with
// only the materialized instance — exactly what Manager.SaveInstancesForShutdown
// passes. The ghost must survive on disk.
//
// This is the test shape described in the bug report's Evidence #1.
func TestSaveInstances_PreservesGhostAcrossLoadAndShutdown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	// A docker sandbox row materializes via FromInstanceData as an inert Lost
	// session (the loader's documented contract for a non-archived sandbox),
	// so it serves as the loadable sibling a mixed repo holds alongside the
	// ghost. No real git worktree is required: the sandbox branch of
	// FromInstanceData rebuilds only an inert backend.
	siblingSeed := InstanceData{
		ID: "sibling-id", Title: "sibling", Path: repoPath, Program: "claude",
		Status: Running, Liveness: LiveRunning, BackendType: "docker",
		Branch: "af/sibling",
	}
	ghostSeed := unloadableRelocationRecoveryRow(t, "ghost", repoPath)
	seedDisk(t, state, repoPath, []InstanceData{siblingSeed, ghostSeed})
	mustMaterialize(t, siblingSeed)
	mustFailToMaterialize(t, ghostSeed)

	// Mirror the daemon: load each row, skip the one that fails, save only the
	// materialized in-memory instance — what InstancesSnapshot() returns.
	materialized := loadMaterializedLikeDaemon(t, state)
	require.Len(t, materialized, 1, "fixture: the sibling must materialize and the ghost must not")
	require.Equal(t, "sibling", materialized[0].Title)

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstancesForShutdown(materialized))

	got := rowsByTitle(t, state, repoPath)
	require.Contains(t, got, "sibling", "the materialized sibling must be saved")
	require.Contains(t, got, "ghost",
		"the unloadable ghost row must survive the daemon's load→shutdown flow: "+
			"the loader left it on disk for repair and the checkpoint must not undo that")
	require.Equal(t, ghostSeed.Worktree.WorktreePath, got["ghost"].Worktree.WorktreePath,
		"the preserved ghost must keep its worktree pointer intact for repair")
}

// TestSaveInstances_PreservesMultipleGhostsInSameRepo verifies the fix
// preserves every ghost in a mixed repo, not just one.
func TestSaveInstances_PreservesMultipleGhostsInSameRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	ghost1 := unloadableRelocationRecoveryRow(t, "ghost-1", repoPath)
	ghost2 := unloadableRelocationRecoveryRow(t, "ghost-2", repoPath)
	ghost3 := unloadableRelocationRecoveryRow(t, "ghost-3", repoPath)
	seedDisk(t, state, repoPath, []InstanceData{ghost1, ghost2, ghost3})

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{alive}))

	got := rowsByTitle(t, state, repoPath)
	require.Contains(t, got, "alive")
	for _, title := range []string{"ghost-1", "ghost-2", "ghost-3"} {
		require.Contains(t, got, title,
			"every ghost in a mixed repo must survive the checkpoint, not just one")
	}
}

// TestSaveInstances_AllGhostsRepoLeftUntouched is the carve-out: a repo the
// daemon holds NO loadable in-memory instance for must NOT be rewritten by the
// checkpoint — its on-disk ghosts survive untouched simply because the repo is
// absent from the grouped map. This invariant already held before the fix for
// the all-ghosts case (the bug is the MIXED-repo case); this test pins it as a
// regression guard so the new disk-read path cannot accidentally start writing
// empty files for repos the daemon holds nothing for.
func TestSaveInstances_AllGhostsRepoLeftUntouched(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	ghost := unloadableRelocationRecoveryRow(t, "ghost", repoPath)
	seedDisk(t, state, repoPath, []InstanceData{ghost})
	original := readDisk(t, state, repoPath)
	require.Len(t, original, 1)

	// Save nothing for this repo: the daemon holds no in-memory instance for
	// it, exactly the all-ghosts case.
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances(nil))
	require.NoError(t, storage.SaveInstancesForShutdown(nil))

	after := readDisk(t, state, repoPath)
	require.Equal(t, original, after,
		"an all-ghosts repo must be left untouched: the checkpoint must not open, "+
			"empty, or rewrite a repo file the daemon holds no in-memory instance for")
}

// TestSaveInstances_PreservesGhostCarryingEachDurableMarker is a table over the
// durable retention markers the bug report enumerates. Each row is unloadable
// (a relocation-recovery ghost) AND carries one durable marker; the checkpoint
// must preserve the row verbatim so the marker survives to the next daemon
// start for repair. These markers exist precisely to persist across restarts,
// and before the fix the shutdown checkpoint was the only writer that erased
// them.
func TestSaveInstances_PreservesGhostCarryingEachDurableMarker(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d *InstanceData)
		check  func(t *testing.T, d InstanceData)
		marker string
	}{
		{
			name:   "PendingOnComplete",
			marker: "#4162 on_complete teardown obligation",
			mutate: func(d *InstanceData) {
				d.PendingOnComplete = &PendingOnCompleteData{
					TaskID:  "task-owed",
					FiledAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				}
			},
			check: func(t *testing.T, d InstanceData) {
				require.NotNil(t, d.PendingOnComplete, "the preserved ghost lost its PendingOnComplete obligation")
				assert.Equal(t, "task-owed", d.PendingOnComplete.TaskID)
				assert.Equal(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), d.PendingOnComplete.FiledAt)
			},
		},
		{
			name:   "UserKilled tombstone",
			marker: "#1108 kill-intent tombstone whose kill can then never be finished",
			mutate: func(d *InstanceData) {
				d.UserKilled = true
			},
			check: func(t *testing.T, d InstanceData) {
				assert.True(t, d.UserKilled, "the preserved ghost lost its UserKilled tombstone")
			},
		},
		{
			name:   "LostRestoreWait",
			marker: "#5295 armed restore retry record",
			mutate: func(d *InstanceData) {
				d.LostRestoreWait = &LostRestoreWait{Attempts: 3, Error: "no space left on device"}
			},
			check: func(t *testing.T, d InstanceData) {
				require.NotNil(t, d.LostRestoreWait, "the preserved ghost lost its armed LostRestoreWait retry record")
				assert.Equal(t, 3, d.LostRestoreWait.Attempts)
				assert.Equal(t, "no space left on device", d.LostRestoreWait.Error)
			},
		},
		{
			name:   "LostRestoreFailure",
			marker: "#3310 persistent restore-gave-up record",
			mutate: func(d *InstanceData) {
				d.LostRestoreFailure = &LostRestoreFailure{Attempts: 3, Error: "exhausted retries"}
			},
			check: func(t *testing.T, d InstanceData) {
				require.NotNil(t, d.LostRestoreFailure, "the preserved ghost lost its LostRestoreFailure record")
				assert.Equal(t, 3, d.LostRestoreFailure.Attempts)
			},
		},
		{
			name:   "RuntimeCleanupStateUnknown",
			marker: "armed sandbox teardown the next restore must retry",
			mutate: func(d *InstanceData) {
				d.RuntimeCleanupStateUnknown = true
			},
			check: func(t *testing.T, d InstanceData) {
				assert.True(t, d.RuntimeCleanupStateUnknown, "the preserved ghost lost its RuntimeCleanupStateUnknown marker")
			},
		},
		{
			name:   "PendingHandoffMission",
			marker: "pending runtime swap the daemon still owes",
			mutate: func(d *InstanceData) {
				d.PendingHandoffMission = "continue the inherited work"
			},
			check: func(t *testing.T, d InstanceData) {
				assert.Equal(t, "continue the inherited work", d.PendingHandoffMission,
					"the preserved ghost lost its PendingHandoffMission")
			},
		},
		{
			name:   "TaskRunActive slot contribution",
			marker: "#1892 task-run slot the cap counts against",
			mutate: func(d *InstanceData) {
				d.TaskID = "watch-task-1"
				d.TaskRunActive = true
			},
			check: func(t *testing.T, d InstanceData) {
				assert.Equal(t, "watch-task-1", d.TaskID, "the preserved ghost lost its TaskID")
				assert.True(t, d.TaskRunActive, "the preserved ghost lost its TaskRunActive marker — "+
					"the next start must count this run against max_concurrent_runs (#1892)")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			ghost := unloadableRelocationRecoveryRow(t, "ghost", repoPath)
			tc.mutate(&ghost)
			seedDisk(t, state, repoPath, []InstanceData{ghost})
			mustFailToMaterialize(t, ghost)

			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive}))

			got := rowsByTitle(t, state, repoPath)
			require.Contains(t, got, "ghost",
				"the %s ghost must survive the checkpoint — %s", tc.name, tc.marker)
			tc.check(t, got["ghost"])
		})
	}
}

// TestSaveInstances_LegacyTransientGhostStillReaped pins the carve-out: an
// older binary's stranded Loading/Deleting row with NO durable retention
// markers is disposable (#551) and must still be reaped by the checkpoint,
// even with the new ghost-preservation read. This is the existing
// TestRepoSaveReapsLegacyLoadingGhost / TestDaemonSaveReapsLegacyLoadingGhost
// contract restated against the new disk-merge path so the carve-out cannot
// regress.
func TestSaveInstances_LegacyTransientGhostStillReaped(t *testing.T) {
	for _, status := range []Status{Loading, Deleting} {
		t.Run(statusName(status), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			seedDisk(t, state, repoPath, []InstanceData{
				{Title: "legacy-ghost", Path: repoPath, Status: status},
				{Title: "alive", Path: repoPath, Status: Running},
			})

			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive}))

			got := rowsByTitle(t, state, repoPath)
			require.Contains(t, got, "alive")
			require.NotContains(t, got, "legacy-ghost",
				"a disposable legacy %s ghost with no durable markers must be reaped, "+
					"not preserved (#551)", statusName(status))
		})
	}
}

// TestSaveInstances_LegacyTransientGhostWithDurableMarkerPreserved verifies
// the carve-out respects durable retention markers: a Loading/Deleting row
// that carries a durable marker is NOT disposable (#551 carve-out is bounded
// by isLegacyTransientGhostRow), so the checkpoint must preserve it for repair.
// The markers that lift the carve-out are PendingHandoffMission,
// RuntimeCleanupStateUnknown, an unresolved Worktree.RelocationRecovery, and a
// non-empty ArchiveReport — the same durable-retention markers
// snapshotInstancesForCheckpoint uses for in-memory rows.
func TestSaveInstances_LegacyTransientGhostWithDurableMarkerPreserved(t *testing.T) {
	cases := []struct {
		name   string
		marker string
		ghost  InstanceData
		check  func(t *testing.T, d InstanceData)
	}{
		{
			name:   "PendingHandoffMission",
			marker: "a pending handoff the daemon still owes",
			ghost: InstanceData{
				ID: "loading-owed-id", Title: "loading-owed", Path: "", Program: "claude",
				Status: Loading, PendingHandoffMission: "continue the inherited work",
			},
			check: func(t *testing.T, d InstanceData) {
				assert.Equal(t, "continue the inherited work", d.PendingHandoffMission,
					"the preserved ghost lost its PendingHandoffMission")
			},
		},
		{
			name:   "RuntimeCleanupStateUnknown",
			marker: "an armed sandbox teardown the next restore must retry",
			ghost: InstanceData{
				ID: "loading-cleanup-id", Title: "loading-cleanup", Path: "", Program: "claude",
				Status: Loading, RuntimeCleanupStateUnknown: true,
			},
			check: func(t *testing.T, d InstanceData) {
				assert.True(t, d.RuntimeCleanupStateUnknown,
					"the preserved ghost lost its RuntimeCleanupStateUnknown")
			},
		},
		{
			name:   "RelocationRecovery",
			marker: "an unresolved worktree relocation recovery record",
			ghost: InstanceData{
				ID: "loading-reloc-id", Title: "loading-reloc", Path: "", Program: "claude",
				Status: Loading,
				Worktree: GitWorktreeData{
					RelocationRecovery: &GitWorktreeRelocationRecoveryData{
						State: sessiongit.RelocationRecoveryStalled,
					},
				},
			},
			check: func(t *testing.T, d InstanceData) {
				require.NotNil(t, d.Worktree.RelocationRecovery,
					"the preserved ghost lost its RelocationRecovery")
				assert.Equal(t, sessiongit.RelocationRecoveryStalled, d.Worktree.RelocationRecovery.State,
					"the preserved ghost lost its RelocationRecovery state")
			},
		},
		{
			name:   "ArchiveReport",
			marker: "a non-empty archive report with retained source trees",
			ghost: InstanceData{
				ID: "loading-archive-id", Title: "loading-archive", Path: "", Program: "claude",
				Status: Loading,
				ArchiveReport: &sessiongit.ArchiveReport{
					RetainedTrees: []sessiongit.ArchiveRetainedTree{{Path: "/retained"}},
				},
			},
			check: func(t *testing.T, d InstanceData) {
				require.NotNil(t, d.ArchiveReport,
					"the preserved ghost lost its ArchiveReport")
				require.False(t, d.ArchiveReport.Empty(),
					"the preserved ghost's ArchiveReport must be non-empty")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			tc.ghost.Path = repoPath
			require.False(t, isLegacyTransientGhostRow(tc.ghost),
				"fixture: a Loading row with %s must NOT be a disposable legacy ghost", tc.marker)

			seedDisk(t, state, repoPath, []InstanceData{tc.ghost})
			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive}))

			got := rowsByTitle(t, state, repoPath)
			require.Contains(t, got, tc.ghost.Title,
				"a Loading row with %s must be preserved, not reaped", tc.marker)
			tc.check(t, got[tc.ghost.Title])
		})
	}
}

// TestSaveInstances_KilledInMemoryInstanceNotResurrectedAsGhost is the
// regression guard for the "represented" check. A killed in-memory instance
// is dropped by the snapshot (dead & killed), and its on-disk row must NOT be
// re-persisted as a ghost — the in-memory row's title marks it "represented"
// even though the snapshot dropped it. This is the TestDaemonSaveRemovesKilledInstances
// contract restated against the new disk-merge path: a kill the daemon knows
// about must still take effect on disk.
func TestSaveInstances_KilledInMemoryInstanceNotResurrectedAsGhost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	// Disk starts with a loadable A and a loadable B; both are in the daemon's
	// memory, but B was killed (started=false) and the snapshot drops it.
	seedDisk(t, state, repoPath, []InstanceData{
		{Title: "instance-A", Path: repoPath, Status: Running},
		{Title: "instance-B", Path: repoPath, Status: Running},
	})
	instanceA := makeInstance("instance-A", repoPath, true)
	instanceB := makeInstance("instance-B", repoPath, false) // killed

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{instanceA, instanceB}))

	got := rowsByTitle(t, state, repoPath)
	require.Contains(t, got, "instance-A", "the started instance should be saved")
	require.NotContains(t, got, "instance-B",
		"a killed in-memory instance must NOT be resurrected as a ghost: its title "+
			"marks it represented and the snapshot legitimately dropped it (#819/#844)")
}

// TestSaveInstances_PreservesGhostAlongsideArchiveReconcile verifies the new
// ghost-preservation logic composes correctly with the existing archive
// reconciliation path: a repo holding a mid-archive sandbox row AND an
// unloadable ghost must keep BOTH after the checkpoint — the mid-archive row
// retained by the pendingArchiveSandbox claim and the ghost preserved by the
// new merge. needArchiveReconcile is true for the mid-archive row, so the read
// also feeds reconcilePendingArchiveRows / mergeCommittedArchiveRows; the
// ghost must survive that path too.
func TestSaveInstances_PreservesGhostAlongsideArchiveReconcile(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			alive := makeAliveInstance("alive", repoPath)
			// A mid-archive sandbox row retained by the pendingArchiveSandbox
			// claim. The snapshot keeps it; needsArchiveReconcile is true.
			midArchive := &Instance{
				ID: "mid-archive-id", Title: "mid-archive", Path: repoPath,
				Branch: "af/mid-archive", Program: "claude",
				started: true, liveness: LiveRunning, inFlightOp: OpArchiving,
				archivePushCompleted: true,
				backend:              newInertSandboxBackend(backendType),
			}
			// An unloadable ghost row in the same repo, NOT involved in the
			// archive. It must survive the checkpoint alongside the mid-archive
			// row.
			ghost := unloadableRelocationRecoveryRow(t, "ghost", repoPath)
			seedDisk(t, state, repoPath, []InstanceData{ghost})

			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive, midArchive}))

			got := rowsByTitle(t, state, repoPath)
			require.Contains(t, got, "alive")
			require.Contains(t, got, "mid-archive",
				"the mid-archive sandbox row must be retained by its pendingArchiveSandbox claim")
			require.Contains(t, got, "ghost",
				"the unloadable ghost must survive alongside the archive-reconcile path: "+
					"needsArchiveReconcile must not gate ghost preservation")
			assert.Equal(t, "af/mid-archive", got["mid-archive"].Branch,
				"the mid-archive row's pushed-branch pointer must survive")
			assert.Equal(t, ghost.ID, got["ghost"].ID,
				"the ghost's stable identity must survive verbatim")
		})
	}
}

// TestSaveInstances_GhostReadFailurePropagated verifies that a disk-read
// failure during the ghost-preservation read propagates as an error rather
// than silently proceeding to overwrite the on-disk ghosts. This mirrors the
// established TestSaveInstances_ReconcileReadFailureIsPropagated contract for
// the archive path: a silent overwrite would clobber the ghost rows with the
// stale in-memory snapshot, which is the exact data loss the fix exists to
// prevent.
func TestSaveInstances_GhostReadFailurePropagated(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	ms := newMockStorage()
	rid := config.RepoIDFromRoot(repoPath)
	failingState := &failOnGetInstancesMockStorage{
		mockInstanceStorage: ms,
		failRepoID:          rid,
	}

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(failingState, "")
	require.NoError(t, err)
	err = storage.SaveInstances([]*Instance{alive})
	require.Error(t, err, "SaveInstances must return an error when GetInstances fails during the "+
		"ghost-preservation read: silently proceeding would overwrite the ghost rows with the "+
		"stale in-memory snapshot")
}

// TestSaveInstances_CorruptInstancesJsonStillOverwritten is the regression
// guard for the corrupt-file carve-out: the daemon is the sole writer, so a
// corrupt instances.json must be overwritten with the authoritative in-memory
// state (#938) rather than abort the save. The new disk-read path must NOT
// abort on a corrupt file; it must treat the unreadable bytes as "no resolvable
// rows" and proceed to overwrite.
//
// This mock-based test exercises the json.Unmarshal fallback (corrupt bytes
// returned with a nil error from a mock that does not unwrap the envelope). The
// real-filesystem companion below (TestSaveInstances_CorruptInstancesJsonStillOverwritten_RealState)
// exercises the production GetInstances path, where LoadRepoInstances returns
// an error for corrupt content and the raw-file probe distinguishes corrupt
// content (overwrite) from I/O failure (propagate).
func TestSaveInstances_CorruptInstancesJsonStillOverwritten(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()
	ms.data[repoID] = json.RawMessage(`{not valid json`)

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(ms, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{alive}),
		"the daemon must overwrite a corrupt instances.json rather than abort the save (#938)")

	rows := readDisk(t, ms, repoPath)
	require.Len(t, rows, 1)
	assert.Equal(t, "alive", rows[0].Title)
}

// TestSaveInstances_CorruptInstancesJsonStillOverwritten_RealState is the
// production-path companion to the mock-based corrupt-file test. The production
// State.GetInstances calls LoadRepoInstances, which unwraps the instances
// envelope and returns an error for corrupt content — so the json.Unmarshal
// fallback in SaveInstances is unreachable in production for a corrupt file.
// The raw-file probe must distinguish a corrupt-but-readable file (overwrite
// from authoritative memory, the #938 sole-writer contract) from a genuine
// I/O failure (propagate, the #766 read-failure contract). A corrupt file on
// the real filesystem must be overwritten, not abort the checkpoint.
func TestSaveInstances_CorruptInstancesJsonStillOverwritten_RealState(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	repoID := config.RepoIDFromRoot(repoPath)

	// Plant a corrupt instances.json on the real filesystem. config.SaveRepoInstances
	// would validate the JSON, so write the file directly with garbage bytes.
	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{not valid json"), 0o644))

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(config.LoadState(), "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{alive}),
		"the daemon must overwrite a corrupt instances.json rather than abort the save (#938)")

	// Read the real on-disk file back through the production config layer and
	// confirm the authoritative in-memory state replaced the corrupt content.
	scoped, err := NewStorage(config.LoadState(), repoID)
	require.NoError(t, err)
	data, err := scoped.LoadInstanceData()
	require.NoError(t, err, "the corrupt file must have been overwritten, not left unreadable")
	require.Len(t, data, 1, "the in-memory session must be the only row on disk")
	assert.Equal(t, "alive", data[0].Title)
}

// TestSaveInstances_GhostInSeparateRepoLeftUntouched verifies cross-repo
// isolation: a ghost in a repo the daemon holds no in-memory instance for is
// left untouched (the repo is absent from grouped and never iterated), while a
// ghost in the same repo as a live in-memory instance IS preserved. This pins
// the boundary the bug report draws between "all-ghosts repo untouched" and
// "mixed-repo ghosts preserved".
func TestSaveInstances_GhostInSeparateRepoLeftUntouched(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	aliveRepo := t.TempDir()
	ghostRepo := t.TempDir()
	state := newMockStorage()

	ghostInAliveRepo := unloadableRelocationRecoveryRow(t, "mixed-ghost", aliveRepo)
	ghostInGhostRepo := unloadableRelocationRecoveryRow(t, "only-ghost", ghostRepo)
	seedDisk(t, state, aliveRepo, []InstanceData{ghostInAliveRepo})
	seedDisk(t, state, ghostRepo, []InstanceData{ghostInGhostRepo})

	alive := makeAliveInstance("alive", aliveRepo)
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{alive}))

	// The mixed repo's ghost must be preserved by the new merge.
	require.Contains(t, rowsByTitle(t, state, aliveRepo), "mixed-ghost",
		"a ghost in a mixed repo must be preserved")
	// The all-ghosts repo must be left untouched (not rewritten at all).
	require.Contains(t, rowsByTitle(t, state, ghostRepo), "only-ghost",
		"an all-ghosts repo must be left untouched — the checkpoint never opens its file")
}

// TestSaveInstances_GhostSameTitleAsDifferentRepoSibling verifies the per-repo
// scoping of the "represented" check. A ghost in repo B whose title coincides
// with a live in-memory session in repo A (titles are unique per repo, not
// globally) must STILL be preserved when repo B has its own in-memory
// sibling: repo A's title must not silence repo B's ghost. Before the per-repo
// scoping fix, a global inMemoryTitles map would have erased it.
func TestSaveInstances_GhostSameTitleAsDifferentRepoSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoA := t.TempDir()
	repoB := t.TempDir()
	require.NotEqual(t, config.RepoIDFromRoot(repoA), config.RepoIDFromRoot(repoB),
		"fixture: repos must hash to distinct IDs")

	state := newMockStorage()
	// The ghost in repo B shares its title with the live session in repo A.
	ghostInB := unloadableRelocationRecoveryRow(t, "shared-title", repoB)
	seedDisk(t, state, repoB, []InstanceData{ghostInB})

	// Repo A holds the live session "shared-title"; repo B holds its own
	// live sibling "sibling-b" plus the unloadable ghost "shared-title".
	aliveA := makeAliveInstance("shared-title", repoA)
	siblingB := makeAliveInstance("sibling-b", repoB)

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{aliveA, siblingB}))

	// Repo A: only the live session, no ghost (none was seeded).
	require.Contains(t, rowsByTitle(t, state, repoA), "shared-title",
		"repo A's live session must be saved")
	// Repo B: the sibling AND the ghost must both survive. The ghost's title
	// matching repo A's live session must NOT silence it (titles are per-repo).
	gotB := rowsByTitle(t, state, repoB)
	require.Contains(t, gotB, "sibling-b", "repo B's live sibling must be saved")
	require.Contains(t, gotB, "shared-title",
		"a ghost whose title matches a DIFFERENT repo's live session must still be "+
			"preserved: title uniqueness is per-repo, not global")
	assert.Equal(t, ghostInB.ID, gotB["shared-title"].ID,
		"the ghost must survive verbatim")
}

// TestIsLegacyTransientGhostRow_MatchesDaemonPredicate is a parity guard for
// the PendingHandoffMission and RuntimeCleanupStateUnknown markers: the
// storage-side isLegacyTransientGhostRow must agree with the daemon-side
// daemon.isLegacyTransientGhost (daemon/refresh_support.go) for those markers
// so the save and load paths reap the same rows for the ID-backfill carve-out.
// If the daemon widens or narrows its carve-out for those markers, this test
// forces the storage mirror to be revisited.
//
// The storage-side is STRICTER than the loader-side for the remaining
// durable-retention markers (Worktree.RelocationRecovery and ArchiveReport):
// the loader-side isLegacyTransientGhost only gates the legacy ID-backfill reap
// (it never deletes on-disk rows), so preserving these rows at save time does
// not resurrect rows the loader reaps — a stranded/version-skewed Loading or
// Deleting row carrying an unresolved relocation recovery or a non-empty
// archive report is one of the unloadable ghost classes this change is meant
// to preserve (snapshotInstancesForCheckpoint treats both as durable
// retention), and the save-side carve-out must not reap them.
func TestIsLegacyTransientGhostRow_MatchesDaemonPredicate(t *testing.T) {
	// Disposable: Loading/Deleting with no durable markers.
	for _, status := range []Status{Loading, Deleting} {
		assert.True(t, isLegacyTransientGhostRow(InstanceData{Status: status}),
			"a bare %s row should be a disposable legacy ghost", statusName(status))
	}
	// Durable markers lift the carve-out.
	for _, status := range []Status{Loading, Deleting} {
		assert.False(t, isLegacyTransientGhostRow(InstanceData{
			Status: status, PendingHandoffMission: "owed",
		}), "a %s row with a pending handoff is NOT disposable", statusName(status))
		assert.False(t, isLegacyTransientGhostRow(InstanceData{
			Status: status, RuntimeCleanupStateUnknown: true,
		}), "a %s row with an unknown cleanup is NOT disposable", statusName(status))
		// The storage-side carve-out is stricter than the loader-side for the
		// relocation-recovery and archive-report markers: a stranded/version-
		// skewed Loading or Deleting row carrying either is a durable ghost the
		// save path must preserve, not reap.
		assert.False(t, isLegacyTransientGhostRow(InstanceData{
			Status:   status,
			Worktree: GitWorktreeData{RelocationRecovery: &GitWorktreeRelocationRecoveryData{}},
		}), "a %s row with an unresolved relocation recovery is NOT disposable", statusName(status))
		assert.False(t, isLegacyTransientGhostRow(InstanceData{
			Status: status, ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{Path: "/retained"}}},
		}), "a %s row with a non-empty archive report is NOT disposable", statusName(status))
	}
	// Non-transient statuses are never disposable, regardless of markers.
	for _, status := range []Status{Running, Ready, Dead, Lost, Archived} {
		assert.False(t, isLegacyTransientGhostRow(InstanceData{Status: status}),
			"a %s row is never a disposable legacy ghost", statusName(status))
	}
}

// TestPreserveUnmaterializedGhostRows_Unit is a direct unit test for the merge
// helper, covering the four quadrants of the (represented, disposable) grid
// plus the empty-on-disk fast path and the dedupe-handoff.
func TestPreserveUnmaterializedGhostRows_Unit(t *testing.T) {
	// Empty on-disk: passthrough.
	in := []InstanceData{{Title: "alive"}}
	got := preserveUnmaterializedGhostRows(in, nil, map[string]struct{}{"alive": {}})
	require.Equal(t, in, got, "empty on-disk must be a passthrough")

	// Represented on-disk row (title in inMemoryTitles): skipped, NOT appended.
	in = []InstanceData{{Title: "alive"}}
	onDisk := []InstanceData{
		{Title: "alive", ID: "alive-id"}, // same title as in-memory
		{Title: "ghost"},                 // not represented, not disposable
	}
	got = preserveUnmaterializedGhostRows(in, onDisk, map[string]struct{}{"alive": {}})
	titles := make(map[string]struct{}, len(got))
	for _, d := range got {
		titles[d.Title] = struct{}{}
	}
	require.Contains(t, titles, "alive")
	require.Contains(t, titles, "ghost", "a non-represented non-disposable row must be preserved")
	require.Len(t, got, 2, "the represented disk row must NOT be appended again")

	// Disposable legacy ghost (Loading, no markers): skipped, NOT appended.
	in = []InstanceData{{Title: "alive"}}
	onDisk = []InstanceData{
		{Title: "legacy-ghost", Status: Loading},
	}
	got = preserveUnmaterializedGhostRows(in, onDisk, map[string]struct{}{"alive": {}})
	require.Len(t, got, 1, "a disposable legacy transient ghost must be reaped, not preserved")
	assert.Equal(t, "alive", got[0].Title)

	// Disposable-but-durable (Loading + PendingHandoffMission): preserved.
	in = []InstanceData{{Title: "alive"}}
	onDisk = []InstanceData{
		{Title: "loading-owed", Status: Loading, PendingHandoffMission: "owed"},
	}
	got = preserveUnmaterializedGhostRows(in, onDisk, map[string]struct{}{"alive": {}})
	require.Len(t, got, 2, "a Loading row with a durable marker must be preserved, not reaped")

	// Disposable-but-durable (Loading + RelocationRecovery): preserved.
	// A stranded/version-skewed Loading row carrying an unresolved relocation
	// recovery is one of the unloadable ghost classes this change is meant to
	// preserve; the carve-out must not reap it.
	in = []InstanceData{{Title: "alive"}}
	onDisk = []InstanceData{
		{Title: "loading-reloc", Status: Loading, Worktree: GitWorktreeData{
			RelocationRecovery: &GitWorktreeRelocationRecoveryData{},
		}},
	}
	got = preserveUnmaterializedGhostRows(in, onDisk, map[string]struct{}{"alive": {}})
	require.Len(t, got, 2, "a Loading row with an unresolved relocation recovery must be preserved, not reaped")

	// Disposable-but-durable (Loading + ArchiveReport): preserved.
	in = []InstanceData{{Title: "alive"}}
	onDisk = []InstanceData{
		{Title: "loading-archive", Status: Loading, ArchiveReport: &sessiongit.ArchiveReport{
			RetainedTrees: []sessiongit.ArchiveRetainedTree{{Path: "/retained"}},
		}},
	}
	got = preserveUnmaterializedGhostRows(in, onDisk, map[string]struct{}{"alive": {}})
	require.Len(t, got, 2, "a Loading row with a non-empty archive report must be preserved, not reaped")
}

// statusName returns a human-readable name for a Status, used in subtests and
// assertion messages so a failure names the status that surprised it.
func statusName(s Status) string {
	switch s {
	case Running:
		return "Running"
	case Ready:
		return "Ready"
	case Loading:
		return "Loading"
	case Deleting:
		return "Deleting"
	case Dead:
		return "Dead"
	case Lost:
		return "Lost"
	case Archived:
		return "Archived"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}
