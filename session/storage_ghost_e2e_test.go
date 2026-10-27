package session

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSaveInstancesForShutdown_RealFilesystem_PreservesGhostAcrossRestart
// is the real-filesystem end-to-end smoke: it uses config.LoadState() (real
// on-disk instances.json + the production config.WithFileLock and atomic
// write), seeds a repo file with one loadable row and one unloadable (ghost)
// row, materializes via the real session.FromInstanceData exactly as
// refreshDaemonInstances does (skipping the ghost), then calls the production
// Storage.SaveInstancesForShutdown with only the materialized in-memory
// instance — exactly what Manager.SaveInstancesForShutdown passes. The ghost
// must survive on the REAL on-disk file across the shutdown checkpoint.
//
// This exercises the true production I/O path (file read, file lock, atomic
// write) rather than the in-memory mock storage the unit tests use, so it
// bounds the contract to the real filesystem layer too.
//
// The loadable "alive" row is a sandbox/remote row so FromInstanceData loads
// it INERT (started=false, LiveLost) without invoking tmux (no tmux server is
// available in this environment); the "ghost" row is a local row whose
// relocation-recovery record is missing its rollback-safety metadata, so
// FromInstanceData fails at the data-level RestoreRelocationRecoveryOriginals
// step (before any tmux Start) and the daemon leaves it on disk for repair.
func TestSaveInstancesForShutdown_RealFilesystem_PreservesGhostAcrossRestart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoPath := t.TempDir()
	repoID := config.RepoIDFromRoot(repoPath)

	// alive: a remote sandbox row that materializes INERT (no tmux needed).
	aliveData := InstanceData{
		ID: "alive-id", Title: "alive", Path: repoPath, Program: "claude",
		Status: Running, Liveness: LiveRunning, BackendType: "remote",
		Branch: "af/alive",
	}
	// ghost: a local row whose relocation-recovery record is missing its
	// rollback-safety Original* fields. FromInstanceData fails at the
	// data-level RestoreRelocationRecoveryOriginals step (before the backend
	// switch / tmux Start) with "rollback safety metadata is missing".
	ghostData := InstanceData{
		ID: "ghost-id", Title: "ghost", Path: repoPath, Program: "claude",
		Status: Running, Liveness: LiveRunning, BackendType: "local",
		Worktree: GitWorktreeData{
			RepoPath:     repoPath,
			WorktreePath: filepath.Join(repoPath, "ghost-wt"),
			BranchName:   "af/ghost",
			RelocationRecovery: &GitWorktreeRelocationRecoveryData{
				State: "relocation_stalled",
				// OriginalExternalWorktree / OriginalBranchCreatedByUs /
				// OriginalStartupStateUnknown intentionally nil so
				// RestoreRelocationRecoveryOriginals fails — a real
				// partially-written or version-skewed recovery record's
				// failure mode.
			},
		},
	}
	seed := []InstanceData{aliveData, ghostData}
	raw, err := json.Marshal(seed)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, raw))

	// Mirror refreshDaemonInstances: load each row, skip the one that fails
	// to materialize, keep only the loadable one in memory. The scoped
	// storage reads the REAL on-disk file.
	scoped, err := NewStorage(config.LoadState(), repoID)
	require.NoError(t, err)
	data, err := scoped.LoadInstanceData()
	require.NoError(t, err)
	var inMemory []*Instance
	for _, d := range data {
		inst, instErr := FromInstanceData(d)
		if instErr != nil {
			t.Logf("daemon skipped instance %q: %v (left on disk for repair)", d.Title, instErr)
			continue
		}
		inst.PinStorageRepoID(repoID)
		inMemory = append(inMemory, inst)
	}
	require.Len(t, inMemory, 1, "fixture: alive must materialize and ghost must not")
	require.Equal(t, "alive", inMemory[0].Title)

	// The production shutdown checkpoint: daemon mode (empty repoID) so
	// SaveInstances groups across all repos, exactly as
	// Manager.SaveInstancesForShutdown drives it. Uses the REAL file lock +
	// atomic write via config.LoadState().
	storage, err := NewStorage(config.LoadState(), "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstancesForShutdown(inMemory),
		"the shutdown checkpoint must succeed on the real filesystem")

	// Read the REAL on-disk file back (through the same config-layer read
	// the daemon uses) and assert the ghost survived verbatim. SaveRepoInstances
	// wraps the rows in a schema_version envelope, so read via the scoped
	// storage that unwraps it rather than parsing the raw bytes directly.
	scopedAfter, err := NewStorage(config.LoadState(), repoID)
	require.NoError(t, err)
	after, err := scopedAfter.LoadInstanceData()
	require.NoError(t, err)
	byTitle := make(map[string]InstanceData, len(after))
	for _, d := range after {
		byTitle[d.Title] = d
	}
	require.Contains(t, byTitle, "alive", "the materialized session must be saved")
	require.Contains(t, byTitle, "ghost",
		"END-TO-END: the unloadable ghost row must survive the shutdown checkpoint on the "+
			"real on-disk instances.json — it is the only durable handle to its worktree, branch, "+
			"and task-run slot (#4162/#1892/#3310)")
	assert.Equal(t, "ghost-id", byTitle["ghost"].ID, "the preserved ghost must keep its stable identity")
	assert.Equal(t, filepath.Join(repoPath, "ghost-wt"), byTitle["ghost"].Worktree.WorktreePath,
		"the preserved ghost must keep its worktree pointer for repair")
	assert.Equal(t, "af/ghost", byTitle["ghost"].Worktree.BranchName,
		"the preserved ghost must keep its branch pointer")
}
