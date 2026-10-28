package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchivedWorkspaceRoundTripsThroughStorage(t *testing.T) {
	data := InstanceData{
		ID: "s1", Title: "t", Path: t.TempDir(), Status: Archived, BackendType: "local",
		Host:              "buildbox",
		Worktree:          GitWorktreeData{RepoPath: "/r", WorktreePath: "/r/wt"},
		ArchivedWorkspace: &ArchivedWorkspaceData{Host: "buildbox", Dir: "/home/u/.af-sessions/s1"},
	}
	inst, err := FromInstanceData(data)
	require.NoError(t, err)
	got := inst.ToInstanceData().ArchivedWorkspace
	require.NotNil(t, got)
	assert.Equal(t, *data.ArchivedWorkspace, *got)
	// The accessor hands out a copy: mutating it must not rewrite the row.
	inst.ArchivedWorkspace().Dir = "elsewhere"
	assert.Equal(t, "/home/u/.af-sessions/s1", inst.ArchivedWorkspace().Dir)
}

func TestRestoreRefusedForPreservedWorkspace(t *testing.T) {
	build := func(w *ArchivedWorkspaceData) *Instance {
		inst, err := FromInstanceData(InstanceData{
			ID: "s1", Title: "t", Path: t.TempDir(), Status: Archived, BackendType: "local", ArchivedWorkspace: w,
			Worktree: GitWorktreeData{RepoPath: "/r", WorktreePath: "/r/wt"},
		})
		require.NoError(t, err)
		return inst
	}
	t.Run("an unpushed preserved workspace is refused and named", func(t *testing.T) {
		err := build(&ArchivedWorkspaceData{Host: "box", Dir: "/d"}).RestoreRefusedForPreservedWorkspace()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "box:/d")
		assert.Contains(t, err.Error(), "W8.6")
	})
	t.Run("a pushed archive restores as before", func(t *testing.T) {
		assert.NoError(t, build(&ArchivedWorkspaceData{Pushed: true, Branch: "b"}).RestoreRefusedForPreservedWorkspace())
	})
	t.Run("a row archived before the field restores as before", func(t *testing.T) {
		assert.NoError(t, build(nil).RestoreRefusedForPreservedWorkspace())
	})
}
