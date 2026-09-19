package daemon

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteProject_ReservedLegacyRowIsPreservedNotRestorable pins Option 2 of
// the #4407 change request. Project deletion archives a pre-#3732 local "ro ot"
// row through allowReserved so its linked worktree is preserved, but restore
// refuses the row: starting it again would claim the root's af_root tmux name.
// The delete therefore must not report that row as restorable. ArchivedCount
// is the "restorable" number every client prints, so the row is counted in
// UnrestorableCount instead, and the refused restore names where the work is.
func TestDeleteProject_ReservedLegacyRowIsPreservedNotRestorable(t *testing.T) {
	const legacyTitle = "ro ot"
	manager, repoID, repoPath := newStatusTestManager(t)
	_, _ = registerArchivable(t, manager, repoID, repoPath, "alpha")
	legacy, legacySource := registerArchivable(t, manager, repoID, repoPath, legacyTitle)
	require.True(t, session.IsReservedRecordTitle(legacyTitle, legacy.BackendType()),
		"premise: a local %q record claims the root's tmux name", legacyTitle)
	require.NoError(t, manager.storage.SaveInstances(manager.InstancesSnapshot()))

	server := &controlServer{manager: manager}
	var resp DeleteProjectResponse
	require.NoError(t, server.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath}, &resp))
	assert.Equal(t, 1, resp.ArchivedCount, "only alpha can be restored, so only alpha is reported as archived")
	assert.Equal(t, 1, resp.UnrestorableCount, "the legacy row is preserved but restore refuses it")
	assert.Zero(t, resp.KilledCount, "neither session is in-place")

	// Preserved: archived like any other row, and the uncommitted work moved
	// with the worktree rather than being destroyed.
	assert.Equal(t, session.Archived, legacy.GetStatus())
	assert.False(t, exists(legacySource), "the worktree was relocated to the archive")
	archivedWorktree := legacy.GetWorktreePath()
	require.NotEmpty(t, archivedWorktree)
	assert.True(t, exists(filepath.Join(archivedWorktree, "dirty.txt")), "uncommitted work survives the archive")

	// Not restorable, and the refusal says where the work is.
	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: legacyTitle, RepoID: repoID})
	require.Error(t, err, "restore must refuse a row whose title claims the reserved root name")
	assert.Contains(t, err.Error(), legacyTitle)
	assert.Contains(t, err.Error(), "reserved")
	assert.Contains(t, err.Error(), archivedWorktree, "the refusal must name the preserved worktree")
	assert.Contains(t, err.Error(), manager.branchForTitle(legacyTitle), "the refusal must name the preserved branch")
	assert.Equal(t, session.Archived, legacy.GetStatus(), "a refused restore leaves the row archived")
	assert.True(t, exists(filepath.Join(archivedWorktree, "dirty.txt")), "a refused restore leaves the work in place")
}
