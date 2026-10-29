package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #5846 / Codex P1: the pre-drain checkpoint runs while a root replacement can
// still commit. It must never overwrite state newer than what it read, so a
// replacement that commits between the caller's snapshot and the write has to
// survive, and the replaced root must not come back.
func TestCheckpointLiveRows_NeverOverwritesNewerState(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	always := func(*Instance) bool { return true }

	for _, tc := range []struct {
		name      string
		disk      []InstanceData
		isCurrent func(*Instance) bool
		// wantBranch maps stable id -> expected branch after the checkpoint.
		wantBranch map[string]string
	}{
		{
			name: "unchanged row is refreshed from the live instance",
			disk: []InstanceData{
				{ID: "root-old", Title: "root", Path: repoPath, Branch: "stale"},
				{ID: "w1", Title: "worker", Path: repoPath, Branch: "stale"},
			},
			isCurrent:  always,
			wantBranch: map[string]string{"root-old": "live", "w1": "live"},
		},
		{
			name: "replacement committed between snapshot and write survives",
			disk: []InstanceData{
				{ID: "root-new", Title: "root", Path: repoPath, Branch: "replacement"},
				{ID: "w1", Title: "worker", Path: repoPath, Branch: "stale"},
			},
			isCurrent:  always,
			wantBranch: map[string]string{"root-new": "replacement", "w1": "live"},
		},
		{
			name: "replaced row deleted before the write is not resurrected",
			disk: []InstanceData{
				{ID: "w1", Title: "worker", Path: repoPath, Branch: "stale"},
			},
			isCurrent:  always,
			wantBranch: map[string]string{"w1": "live"},
		},
		{
			name: "instance no longer current is skipped, its row left as committed",
			disk: []InstanceData{
				{ID: "root-old", Title: "root", Path: repoPath, Branch: "committed"},
				{ID: "w1", Title: "worker", Path: repoPath, Branch: "stale"},
			},
			isCurrent:  func(i *Instance) bool { return i.Title != "root" },
			wantBranch: map[string]string{"root-old": "committed", "w1": "live"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			ms := newMockStorage()
			seedDisk(t, ms, repoPath, tc.disk)

			oldRoot := makeAliveInstance("root", repoPath)
			oldRoot.ID, oldRoot.Branch = "root-old", "live"
			worker := makeAliveInstance("worker", repoPath)
			worker.ID, worker.Branch = "w1", "live"

			storage, err := NewStorage(ms, "")
			require.NoError(t, err)
			require.NoError(t, storage.CheckpointLiveRows([]*Instance{oldRoot, worker}, tc.isCurrent, nil))

			got := map[string]string{}
			for _, row := range readDisk(t, ms, repoPath) {
				got[row.ID] = row.Branch
			}
			require.Equal(t, tc.wantBranch, got)
		})
	}
}

func TestCheckpointLiveRows_LeavesRowsWithoutALiveInstanceByteForByte(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	ms := newMockStorage()
	seedDisk(t, ms, repoPath, []InstanceData{
		{ID: "ghost", Title: "ghost", Path: repoPath, Branch: "ghost-branch"},
		{ID: "w1", Title: "worker", Path: repoPath, Branch: "stale"},
	})
	worker := makeAliveInstance("worker", repoPath)
	worker.ID, worker.Branch = "w1", "live"

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)
	require.NoError(t, storage.CheckpointLiveRows([]*Instance{worker}, nil, nil))

	got := map[string]string{}
	for _, row := range readDisk(t, ms, repoPath) {
		got[row.ID] = row.Branch
	}
	require.Equal(t, map[string]string{"ghost": "ghost-branch", "w1": "live"}, got)
}
