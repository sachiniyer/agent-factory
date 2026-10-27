package session

import (
	"encoding/json"
	"fmt"
	"io/fs"
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

// TestSaveInstances_NewerSchemaInstancesJsonRefusedNotOverwritten is the
// version-skewed companion to the corrupt-file carve-out. A newer af's
// instances.json carries a schema_version this binary cannot read, and
// config.GetInstances returns *config.UnsupportedSchemaVersionError. The raw
// file remains readable, so without an explicit refusal the corrupt-but-
// readable overwrite branch would clobber it with this binary's older
// snapshot — discarding fields and ghost rows the newer af wrote and
// regressing the previous archive-reconciliation path, which propagated every
// read error (#4783). The checkpoint must propagate the error and leave the
// file untouched; the remedy is to upgrade, not to repair.
func TestSaveInstances_NewerSchemaInstancesJsonRefusedNotOverwritten(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	repoID := config.RepoIDFromRoot(repoPath)

	// Plant a newer-schema instances.json on the real filesystem. The
	// envelope carries a schema_version this binary does not understand, so
	// config.LoadRepoInstances returns *config.UnsupportedSchemaVersionError
	// while the raw bytes remain perfectly readable.
	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	newerSchema := []byte(`{"schema_version": 99, "instances": []}`)
	require.NoError(t, os.WriteFile(path, newerSchema, 0o644))

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(config.LoadState(), "")
	require.NoError(t, err)
	err = storage.SaveInstances([]*Instance{alive})
	require.Error(t, err, "SaveInstances must refuse to overwrite a newer-schema "+
		"instances.json rather than clobbering it with this binary's older snapshot (#4783)")

	// The newer-schema file must survive the refused checkpoint verbatim.
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr, "the newer-schema file must not have been touched")
	assert.JSONEq(t, string(newerSchema), string(got),
		"the newer-schema file must be left untouched when the checkpoint refuses it")
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
// plus the empty-on-disk fast path and the dedupe-handoff. It returns the
// verbatim raw bytes of the ghost rows to preserve (a newer binary's additive
// fields survive because the helper never re-encodes them, #6); the parallel
// onDiskRaw is built by marshalling each on-disk row, mirroring what
// SaveInstances decodes from the read bytes.
func TestPreserveUnmaterializedGhostRows_Unit(t *testing.T) {
	rawFor := func(rows []InstanceData) []json.RawMessage {
		out := make([]json.RawMessage, len(rows))
		for i := range rows {
			b, err := json.Marshal(rows[i])
			require.NoError(t, err)
			out[i] = b
		}
		return out
	}
	titlesOf := func(raws []json.RawMessage) map[string]struct{} {
		out := make(map[string]struct{}, len(raws))
		for _, r := range raws {
			var d InstanceData
			require.NoError(t, json.Unmarshal(r, &d))
			out[d.Title] = struct{}{}
		}
		return out
	}

	// Empty on-disk: no ghosts preserved.
	in := []InstanceData{{Title: "alive"}}
	got := preserveUnmaterializedGhostRows(nil, nil, map[string]struct{}{"alive": {}})
	require.Empty(t, got, "empty on-disk must preserve no ghosts")
	_ = in

	// Represented on-disk row (title in inMemoryTitles): skipped, NOT preserved.
	onDisk := []InstanceData{
		{Title: "alive", ID: "alive-id"}, // same title as in-memory
		{Title: "ghost"},                 // not represented, not disposable
	}
	got = preserveUnmaterializedGhostRows(onDisk, rawFor(onDisk), map[string]struct{}{"alive": {}})
	require.Len(t, got, 1, "only the non-represented ghost must be preserved")
	titles := titlesOf(got)
	require.Contains(t, titles, "ghost", "a non-represented non-disposable row must be preserved")
	_, alivePresent := titles["alive"]
	require.False(t, alivePresent, "the represented disk row must NOT be preserved again")

	// Disposable legacy ghost (Loading, no markers): skipped, NOT preserved.
	onDisk = []InstanceData{
		{Title: "legacy-ghost", Status: Loading},
	}
	got = preserveUnmaterializedGhostRows(onDisk, rawFor(onDisk), map[string]struct{}{"alive": {}})
	require.Empty(t, got, "a disposable legacy transient ghost must be reaped, not preserved")

	// Disposable-but-durable (Loading + PendingHandoffMission): preserved.
	onDisk = []InstanceData{
		{Title: "loading-owed", Status: Loading, PendingHandoffMission: "owed"},
	}
	got = preserveUnmaterializedGhostRows(onDisk, rawFor(onDisk), map[string]struct{}{"alive": {}})
	require.Len(t, got, 1, "a Loading row with a durable marker must be preserved, not reaped")

	// Disposable-but-durable (Loading + RelocationRecovery): preserved.
	// A stranded/version-skewed Loading row carrying an unresolved relocation
	// recovery is one of the unloadable ghost classes this change is meant to
	// preserve; the carve-out must not reap it.
	onDisk = []InstanceData{
		{Title: "loading-reloc", Status: Loading, Worktree: GitWorktreeData{
			RelocationRecovery: &GitWorktreeRelocationRecoveryData{},
		}},
	}
	got = preserveUnmaterializedGhostRows(onDisk, rawFor(onDisk), map[string]struct{}{"alive": {}})
	require.Len(t, got, 1, "a Loading row with an unresolved relocation recovery must be preserved, not reaped")

	// Disposable-but-durable (Loading + non-empty ArchiveReport): preserved.
	onDisk = []InstanceData{
		{Title: "loading-archive", Status: Loading, ArchiveReport: &sessiongit.ArchiveReport{
			RetainedTrees: []sessiongit.ArchiveRetainedTree{{Path: "/retained"}},
		}},
	}
	got = preserveUnmaterializedGhostRows(onDisk, rawFor(onDisk), map[string]struct{}{"alive": {}})
	require.Len(t, got, 1, "a Loading row with a non-empty archive report must be preserved, not reaped")

	// Disposable legacy ghost (Loading + EMPTY ArchiveReport): reaped, NOT
	// preserved. A non-nil but empty report has no retained trees to protect
	// and matches the in-memory snapshot's archiveReportPending = !Empty()
	// durable-retention rule, so it is the same disposable transient ghost as
	// one with no report (#7).
	onDisk = []InstanceData{
		{Title: "loading-empty-report", Status: Loading, ArchiveReport: &sessiongit.ArchiveReport{}},
	}
	got = preserveUnmaterializedGhostRows(onDisk, rawFor(onDisk), map[string]struct{}{"alive": {}})
	require.Empty(t, got, "a Loading row with an EMPTY archive report must be reaped, not preserved")

	// Verbatim preservation: an additive field a newer binary wrote under the
	// same schema version survives the checkpoint because the helper carries
	// the original bytes through without re-encoding (#6).
	onDisk = []InstanceData{
		{Title: "version-skewed-ghost"},
	}
	// Plant an additive field this binary's InstanceData does not know on the
	// raw bytes; the decoded form drops it but the preserved raw form keeps it.
	decodedRaw := rawFor(onDisk)
	var probed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decodedRaw[0], &probed))
	probed["future_recovery_handle"] = json.RawMessage(`"abc-123"`)
	verbatim, err := json.Marshal(probed)
	require.NoError(t, err)
	decodedRaw[0] = verbatim
	got = preserveUnmaterializedGhostRows(onDisk, decodedRaw, map[string]struct{}{"alive": {}})
	require.Len(t, got, 1, "the version-skewed ghost must be preserved")
	assert.JSONEq(t, string(verbatim), string(got[0]),
		"the preserved ghost must carry its original bytes verbatim, including additive fields this binary does not know (#6)")

	// A transient row with an unrecognized field fails closed: a Loading/Deleting
	// row whose raw form carries a field this binary's InstanceData does not decode
	// (a newer same-schema durable marker) is preserved, not reaped, because the
	// decode dropped the marker and isLegacyTransientGhostRow saw none of its known
	// markers. The raw form the checkpoint preserves still carries the field (#9f7).
	for _, status := range []Status{Loading, Deleting} {
		onDisk = []InstanceData{
			{Title: "loading-unknown-marker", Status: status},
		}
		rawWithUnknown := rawFor(onDisk)
		var probed2 map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rawWithUnknown[0], &probed2))
		probed2["future_durable_marker"] = json.RawMessage(`"owed"`)
		unknownRaw, err := json.Marshal(probed2)
		require.NoError(t, err)
		rawWithUnknown[0] = unknownRaw
		got = preserveUnmaterializedGhostRows(onDisk, rawWithUnknown, map[string]struct{}{"alive": {}})
		require.Len(t, got, 1, "a %s row with an unrecognized field must be preserved, not reaped: the field may be a newer binary's durable marker", statusName(status))
		assert.JSONEq(t, string(unknownRaw), string(got[0]),
			"the preserved row must carry its unrecognized field verbatim so a newer binary can read it")
	}
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

// ioErrorGetInstancesStorage wraps a config.InstanceStorage and returns a real
// *fs.PathError from GetInstances for one repo, simulating a transient I/O read
// failure from the production LoadRepoInstances path. Unlike
// failOnGetInstancesMockStorage (which returns a plain fmt.Errorf against a
// mock with no on-disk file), the underlying state here is the real
// config.LoadState, so the repo's instances.json IS present on disk — an
// immediate os.ReadFile retry reads it. The checkpoint must still refuse to
// overwrite from the stale in-memory snapshot when its own GetInstances read
// failed with an I/O error, so disk-only ghost rows are not erased under the
// intermittent-read scenario the read-failure contract exists to protect (#4).
type ioErrorGetInstancesStorage struct {
	config.InstanceStorage
	failRepoID string
}

func (s *ioErrorGetInstancesStorage) GetInstances(repoID string) (json.RawMessage, error) {
	if repoID == s.failRepoID {
		// Mirror LoadRepoInstances' wrapping of an os.ReadFile *fs.PathError so
		// checkpointReadError's I/O classification matches the production path.
		path, err := config.RepoInstancesPath(repoID)
		if err != nil {
			return nil, err
		}
		ioErr := &fs.PathError{Op: "read", Path: path, Err: fs.ErrInvalid}
		return nil, fmt.Errorf("failed to read repo instances: %w", ioErr)
	}
	return s.InstanceStorage.GetInstances(repoID)
}

// TestSaveInstances_TransientReadErrorPropagatedEvenIfFileReadable is the
// regression for the intermittent-read contract (#4): when GetInstances fails
// with a genuine I/O error but the file is still readable on an immediate
// os.ReadFile retry, the checkpoint must NOT proceed to overwrite from the
// stale in-memory snapshot — doing so would erase disk-only ghost rows the
// checkpoint never reconciled. The original I/O error must propagate so the
// file is left for the next start.
func TestSaveInstances_TransientReadErrorPropagatedEvenIfFileReadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	repoID := config.RepoIDFromRoot(repoPath)

	// Plant a real on-disk instances.json carrying one unloadable ghost row,
	// exactly as a prior daemon start left it for repair.
	ghost := unloadableRelocationRecoveryRow(t, "ghost", repoPath)
	seed := []InstanceData{ghost}
	raw, err := json.Marshal(seed)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, raw))

	// The retry os.ReadFile inside checkpointReadError will read this file
	// successfully, so the only thing standing between the ghost and erasure is
	// checkpointReadError refusing on a genuine I/O error.
	state := &ioErrorGetInstancesStorage{InstanceStorage: config.LoadState(), failRepoID: repoID}

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	err = storage.SaveInstances([]*Instance{alive})
	require.Error(t, err, "SaveInstances must propagate the transient I/O read failure rather "+
		"than overwrite disk-only ghost rows from the stale in-memory snapshot (#4)")

	// The ghost must survive untouched: the checkpoint refused to clobber a
	// file it failed to read.
	scoped, err := NewStorage(config.LoadState(), repoID)
	require.NoError(t, err)
	after, err := scoped.LoadInstanceData()
	require.NoError(t, err)
	byTitle := make(map[string]InstanceData, len(after))
	for _, d := range after {
		byTitle[d.Title] = d
	}
	require.Contains(t, byTitle, "ghost",
		"the unloadable ghost must survive a transient read failure: the checkpoint must not "+
			"erase rows it could not reconcile")
	require.NotContains(t, byTitle, "alive",
		"the checkpoint must not have written the in-memory snapshot over the unread file")
}

// TestSaveInstances_UninterpretableSchemaVersionRefusedNotOverwritten is the
// companion to TestSaveInstances_NewerSchemaInstancesJsonRefusedNotOverwritten
// for the present-but-UNINTERPRETABLE case (#5): a readable envelope whose
// schema_version field is present but cannot be parsed as a version integer
// (null, a string, a negative, an overflowing number) is NOT returned as
// *config.UnsupportedSchemaVersionError by LoadRepoInstances — it surfaces as
// a generic schema-content error — so without an explicit refusal the
// corrupt-but-readable overwrite branch would clobber a possibly-newer file
// this binary cannot read. The checkpoint must refuse to overwrite it and
// leave the file untouched, mirroring refuseStateDowngrade's
// present-but-uninterpretable rule.
func TestSaveInstances_UninterpretableSchemaVersionRefusedNotOverwritten(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"null", `{"schema_version": null, "instances": []}`},
		{"string", `{"schema_version": "2", "instances": []}`},
		{"negative", `{"schema_version": -1, "instances": []}`},
		{"overflow", `{"schema_version": 99999999999999999999, "instances": []}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			repoID := config.RepoIDFromRoot(repoPath)

			path, err := config.RepoInstancesPath(repoID)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o644))

			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(config.LoadState(), "")
			require.NoError(t, err)
			err = storage.SaveInstances([]*Instance{alive})
			require.Error(t, err, "SaveInstances must refuse to overwrite an instances.json with "+
				"a present-but-uninterpretable schema_version rather than clobbering a possibly-newer "+
				"file this binary cannot read (#5)")

			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr, "the uninterpretable-schema file must not have been touched")
			assert.JSONEq(t, tc.content, string(got),
				"the file must be left untouched when the checkpoint refuses it")
		})
	}
}

// TestSaveInstances_LegacyTransientGhostWithEmptyArchiveReportReaped pins the
// empty-ArchiveReport carve-out (#7): a Loading/Deleting row whose archive
// report is a non-nil but EMPTY pointer (no RetainedTrees) has no retained
// source trees to protect, so it is the same disposable transient ghost as one
// with no report at all and must be reaped by the checkpoint. A Deleting
// variant that survives would otherwise keep reserving its title in
// findTitleConflictLocked across every checkpoint, blocking recreation of
// that session indefinitely.
func TestSaveInstances_LegacyTransientGhostWithEmptyArchiveReportReaped(t *testing.T) {
	for _, status := range []Status{Loading, Deleting} {
		t.Run(statusName(status), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			emptyReportGhost := InstanceData{
				ID: "empty-report-id", Title: "empty-report", Path: repoPath, Program: "claude",
				Status:        status,
				ArchiveReport: &sessiongit.ArchiveReport{}, // non-nil but EMPTY
			}
			require.True(t, isLegacyTransientGhostRow(emptyReportGhost),
				"fixture: a %s row with an EMPTY archive report must be a disposable legacy ghost", statusName(status))

			seedDisk(t, state, repoPath, []InstanceData{emptyReportGhost})
			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive}))

			got := rowsByTitle(t, state, repoPath)
			require.Contains(t, got, "alive")
			require.NotContains(t, got, "empty-report",
				"a %s row with an EMPTY archive report must be reaped, not preserved: it has no "+
					"retained trees to protect and a Deleting variant would block recreating its "+
					"title across every checkpoint (#7)", statusName(status))
		})
	}
}

// TestSaveInstances_TransientRowWithUnrecognizedFieldPreserved is the
// version-skew carve-out (#9f7): a Loading/Deleting row whose raw form carries
// a field this binary's InstanceData does not decode — an additive durable
// marker a newer same-schema binary wrote — must be preserved across the
// checkpoint rather than reaped as a disposable transient ghost. The decode
// drops the unknown field, so isLegacyTransientGhostRow sees none of its known
// markers and would reap the row, erasing the only recovery record a rollback
// shutdown had on disk. The save path fails closed by inspecting the raw form:
// a transient row with an unrecognized field is kept verbatim so the next
// start's newer binary can read it.
func TestSaveInstances_TransientRowWithUnrecognizedFieldPreserved(t *testing.T) {
	for _, status := range []Status{Loading, Deleting} {
		t.Run(statusName(status), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			repoID := config.RepoIDFromRoot(repoPath)
			state := newMockStorage()

			// A bare Loading/Deleting row has no known durable markers, so it is a
			// disposable transient ghost — except that its raw form carries an
			// unrecognized field this binary's InstanceData does not decode, which
			// might be a newer binary's durable marker. Plant that field directly on
			// the raw bytes; seedDisk marshals InstanceData, which drops it.
			row := InstanceData{
				ID: "loading-unknown-id", Title: "loading-unknown", Path: repoPath, Program: "claude",
				Status: status,
			}
			require.True(t, isLegacyTransientGhostRow(row),
				"fixture: the decoded row has no known durable markers and would be reaped")
			raw, err := json.Marshal([]InstanceData{row})
			require.NoError(t, err)
			var arr []map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &arr))
			arr[0]["future_durable_marker"] = json.RawMessage(`"owed-to-the-next-start"`)
			raw, err = json.Marshal(arr)
			require.NoError(t, err)
			state.data[repoID] = raw

			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive}))

			// The row must survive the checkpoint verbatim — the next start's newer
			// binary can read the marker this one cannot.
			got := readDisk(t, state, repoPath)
			var found bool
			for _, d := range got {
				if d.Title == "loading-unknown" {
					found = true
				}
			}
			require.True(t, found, "a %s row with an unrecognized field must be preserved, not reaped: "+
				"the field may be a newer binary's durable marker and the checkpoint must fail closed (#9f7)", statusName(status))
			// The unrecognized field must survive verbatim on the raw bytes the
			// checkpoint wrote.
			var afterArr []map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(state.data[repoID], &afterArr))
			for _, r := range afterArr {
				if string(r["title"]) == `"loading-unknown"` {
					require.Contains(t, r, "future_durable_marker",
						"the unrecognized field must survive the checkpoint verbatim on the raw bytes (#6/#9f7)")
				}
			}
		})
	}
}

// TestSaveInstances_LegacyTransientGhostWithRemovedPrInfoKeyReaped pins the
// removed-wire-key carve-out for the unrecognized-field detector (#El8): pr_info
// was removed (see the InstanceData.BackendType comment) and old records may still
// carry it. encoding/json ignores it on read, and the release notes say it must
// not be written back, so it is not a newer binary's additive durable marker.
// rowHasUnrecognizedInstanceFields treats it as known, so a Loading/Deleting row
// whose only "extra" field is the legacy pr_info is still the disposable
// transient ghost it always was and is reaped by the checkpoint — it does not
// get preserved indefinitely as if pr_info were a newer marker. For a Deleting
// variant, preserving it would also keep its title reserved in
// findTitleConflictLocked across every checkpoint, blocking recreation.
func TestSaveInstances_LegacyTransientGhostWithRemovedPrInfoKeyReaped(t *testing.T) {
	for _, status := range []Status{Loading, Deleting} {
		t.Run(statusName(status), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			repoID := config.RepoIDFromRoot(repoPath)
			state := newMockStorage()

			// A bare Loading/Deleting row has no known durable markers, so it is a
			// disposable transient ghost — except that its raw form carries the
			// REMOVED wire key pr_info, which is in the recognized set (a
			// historical field this binary no longer decodes, not a newer binary's
			// additive marker). Plant pr_info directly on the raw bytes; seedDisk
			// marshals InstanceData, which drops it.
			row := InstanceData{
				ID: "loading-pr-info-id", Title: "loading-pr-info", Path: repoPath, Program: "claude",
				Status: status,
			}
			require.True(t, isLegacyTransientGhostRow(row),
				"fixture: the decoded row has no known durable markers and would be reaped")
			raw, err := json.Marshal([]InstanceData{row})
			require.NoError(t, err)
			var arr []map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &arr))
			arr[0]["pr_info"] = json.RawMessage(`"removed-legacy-key"`)
			raw, err = json.Marshal(arr)
			require.NoError(t, err)
			state.data[repoID] = raw

			// The detector must not treat the removed key as an unrecognized
			// durable marker; it is in the recognized set (a historical field
			// this binary no longer decodes, not a newer binary's additive one).
			rowBytes, mErr := json.Marshal(arr[0])
			require.NoError(t, mErr)
			require.False(t, rowHasUnrecognizedInstanceFields(rowBytes),
				"pr_info is a removed wire key in the recognized set; the row must not be flagged as carrying a newer marker")

			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive}))

			got := rowsByTitle(t, state, repoPath)
			require.NotContains(t, got, "loading-pr-info",
				"a %s row whose only extra field is the removed pr_info key must be reaped, "+
					"not preserved as if it were a newer binary's durable marker (#El8)", statusName(status))
		})
	}
}

// TestSaveInstances_TransientRowWithNestedUnrecognizedFieldPreserved is the
// nested-field carve-out (#El9): an additive durable marker a newer same-schema
// binary writes INSIDE a recognized object (archive_report, worktree) is dropped
// by decoding into this binary's InstanceData, so isLegacyTransientGhostRow sees
// no known marker and would reap a Loading/Deleting row carrying it. The
// top-level-only detector misses it because the nested object's key is known
// (archive_report/worktree); the detector walks the nested object so the save
// path fails closed and preserves the row verbatim for the next start's newer
// binary to read.
func TestSaveInstances_TransientRowWithNestedUnrecognizedFieldPreserved(t *testing.T) {
	cases := []struct {
		name  string
		field string // the top-level recognized container key
		plant func(obj map[string]json.RawMessage)
	}{
		{
			name:  "archive_report",
			field: "archive_report",
			plant: func(obj map[string]json.RawMessage) {
				obj["archive_report"] = json.RawMessage(`{"retained_trees":[],"future_recovery":"owed-to-the-next-start"}`)
			},
		},
		{
			name:  "worktree",
			field: "worktree",
			plant: func(obj map[string]json.RawMessage) {
				obj["worktree"] = json.RawMessage(`{"repo_path":"p","worktree_path":"p/w","session_name":"s","branch_name":"af/s","base_commit_sha":"","future_recovery":"owed-to-the-next-start"}`)
			},
		},
	}
	for _, status := range []Status{Loading, Deleting} {
		for _, tc := range cases {
			t.Run(statusName(status)+"/"+tc.name, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				repoPath := t.TempDir()
				repoID := config.RepoIDFromRoot(repoPath)
				state := newMockStorage()

				// A bare Loading/Deleting row has no known durable markers, so it
				// is a disposable transient ghost — except that a newer binary
				// wrote an additive marker inside a recognized container object
				// this binary still decodes. isLegacyTransientGhostRow sees no
				// known marker (the nested field is dropped on decode), so the
				// detector must walk the nested object and preserve the row.
				row := InstanceData{
					ID: "loading-nested-id", Title: "loading-nested", Path: repoPath, Program: "claude",
					Status: status,
				}
				require.True(t, isLegacyTransientGhostRow(row),
					"fixture: the decoded row has no known durable markers and would be reaped")
				raw, err := json.Marshal([]InstanceData{row})
				require.NoError(t, err)
				var arr []map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &arr))
				tc.plant(arr[0])
				raw, err = json.Marshal(arr)
				require.NoError(t, err)
				state.data[repoID] = raw

				// The detector must flag the nested additive field.
				var selfObj map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(arr[0][tc.field], &selfObj))
				require.Contains(t, selfObj, "future_recovery",
					"fixture: the additive nested field must be present")
				// Re-marshal the whole row to feed the detector.
				rowBytes, mErr := json.Marshal(arr[0])
				require.NoError(t, mErr)
				require.True(t, rowHasUnrecognizedInstanceFields(rowBytes),
					"a nested additive field inside %s must be detected as unrecognized", tc.field)

				alive := makeAliveInstance("alive", repoPath)
				storage, err := NewStorage(state, "")
				require.NoError(t, err)
				require.NoError(t, storage.SaveInstances([]*Instance{alive}))

				got := rowsByTitle(t, state, repoPath)
				require.Contains(t, got, "loading-nested",
					"a %s row with a newer binary's additive marker inside %s must be preserved, "+
						"not reaped: the marker is dropped on decode and isLegacyTransientGhostRow sees no marker",
					statusName(status), tc.field)
				// The nested additive field must survive the checkpoint verbatim
				// on the raw bytes the checkpoint wrote.
				var afterArr []map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(state.data[repoID], &afterArr))
				for _, r := range afterArr {
					if string(r["title"]) == `"loading-nested"` {
						var nested map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(r[tc.field], &nested))
						require.Contains(t, nested, "future_recovery",
							"the nested additive field must survive the checkpoint verbatim so a newer binary can read it (#6/#El9)")
					}
				}
			})
		}
	}
}
