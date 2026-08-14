package daemon

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// TestArchiveSession_PersistAndRollbackFailure_ReportsCommittedArchive is
// #3235's local arm: the worktree has moved and CommitArchive has run; when the
// durable write fails AND the rollback home fails, the code deliberately keeps
// the committed archive — so the caller must receive the archived location, the
// resolved projection, and the committed marker, exactly as the sibling
// keepIncompleteArchiveCommitted path reports. A plain error with an empty path
// here tells every transport failed-nothing-committed about an archive that IS
// committed and kept.
func TestArchiveSession_PersistAndRollbackFailure_ReportsCommittedArchive(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")

	dest, derr := archivedWorktreePath(repoID, "worker")
	require.NoError(t, derr)

	prev := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error {
		// Block the road home while failing the durable write: the worktree has
		// already moved to dest, so a file squatting on the vacated source makes
		// the rollback's move-home refuse ("destination already exists") — the
		// reachable double failure this regression is about.
		require.NoError(t, os.WriteFile(srcPath, []byte("in the way"), 0644))
		return errors.New("forced persist failure")
	}
	t.Cleanup(func() { archivePersist = prev })

	_, ch := manager.events.subscribe()
	archivedPath, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not roll it back",
		"precondition: the double-failure branch must be the one that ran")
	require.True(t, isMutationCommitted(err),
		"the archive is committed and kept, so the error must carry the committed marker: %v", err)
	assert.Equal(t, dest, archivedPath, "the archived location must be preserved for the caller")
	assert.Equal(t, session.Archived, archived.Status, "the resolved projection must be the committed Archived state")
	assert.Equal(t, inst.ID, archived.ID)
	assert.True(t, exists(dest), "the kept archive must actually be at the reported path")

	archEv := drainNextSessionEvent(t, ch, agentproto.EventSessionArchived)
	assert.Equal(t, inst.ID, archEv.ID,
		"a kept-committed archive must publish session.archived like its keepIncompleteArchiveCommitted sibling")
}

// TestArchiveRemoteSession_PersistFailure_ReportsCommittedArchiveWithBranch is
// #3235's remote arm: ArchiveSandbox pushed the branch and reaped the sandbox,
// and CommitArchive has run — there is nothing left to roll back. A later
// instances-record write failure must therefore report committed and preserve
// the pushed branch (the remote archive's location) and the resolved projection,
// instead of an empty branch and a plain error the caller then also drops.
func TestArchiveRemoteSession_PersistFailure_ReportsCommittedArchiveWithBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	inst, _, _ := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "remote-committed", srv.url, session.Running)

	prev := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error {
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { archivePersist = prev })

	_, ch := manager.events.subscribe()
	archivedPath, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "remote-committed", RepoID: repoID})
	require.Error(t, err)
	require.True(t, isMutationCommitted(err),
		"branch pushed and sandbox reaped: the archive is committed and must not read as untouched: %v", err)
	assert.Equal(t, "af/pushed-branch", archivedPath,
		"the pushed branch is the remote archive's location and must be preserved")
	assert.Equal(t, session.Archived, archived.Status)
	assert.Equal(t, inst.ID, archived.ID)

	archEv := drainNextSessionEvent(t, ch, agentproto.EventSessionArchived)
	assert.Equal(t, inst.ID, archEv.ID,
		"the committed remote archive must publish session.archived so clients reconcile the completed transition")
}
