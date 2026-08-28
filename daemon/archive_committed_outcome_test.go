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
	calls := 0
	archivePersist = func(m *Manager, rid string, inst *session.Instance) error {
		calls++
		if calls > 1 {
			// The disk heals for the helper's durable retry: only a LANDED
			// Archived row may claim committed (#3335 review), so the committed
			// case is a transient write failure plus an independent rollback
			// collision.
			return prev(m, rid, inst)
		}
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

// TestArchiveSession_PersistNeverHeals_StaysPlainFailure pins the durability
// half of the #3335 review: the committed claim must itself be durable before
// it is made. When the durable write keeps failing after the failed rollback,
// a restart would reload the pre-archive live row while the bytes sit under
// the archive — and a committed marker would let DeleteProject convert the
// failure to a warning and deregister the project on top of that stale row.
// The plain double-failure error is what stops that, exactly as before.
func TestArchiveSession_PersistNeverHeals_StaysPlainFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")

	prev := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error {
		// Same rollback collision as the committed sibling above, but the disk
		// never heals: every durable write fails.
		_ = os.WriteFile(srcPath, []byte("in the way"), 0644)
		return errors.New("forced persist failure")
	}
	t.Cleanup(func() { archivePersist = prev })

	_, ch := manager.events.subscribe()
	archivedPath, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be written durably",
		"the second write failure must be named alongside the double failure")
	assert.False(t, isMutationCommitted(err),
		"an undurable archive must not claim committed: DeleteProject would deregister over a stale live row")
	assert.Empty(t, archivedPath)
	assert.Empty(t, archived.ID)
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionArchived {
				t.Fatal("no archived event may be published for an archive whose durable claim never landed")
			}
			continue
		default:
		}
		break
	}
}

// TestArchiveRemoteSession_PersistNeverHeals_StaysPlainFailure is the remote
// half of the durability rule (#3335 review): ArchiveSandbox records the pushed
// branch only in memory, so if no durable write ever lands, a restart loads a
// row whose branch is empty or stale — the Lost re-provision then clones the
// default branch and strands the pushed work — and a committed marker would let
// DeleteProject deregister the project over that row. The plain error (whose
// message still names the branch) is what stops both.
func TestArchiveRemoteSession_PersistNeverHeals_StaysPlainFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	_, _, _ = registerStartedRemoteWithReap(t, manager, repoID, repoPath, "remote-undurable", srv.url, session.Running)

	prev := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error {
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { archivePersist = prev })

	_, ch := manager.events.subscribe()
	archivedPath, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "remote-undurable", RepoID: repoID})
	require.Error(t, err)
	assert.False(t, isMutationCommitted(err),
		"an undurable remote archive must not claim committed: the branch exists only in memory and DeleteProject would deregister over the stale row")
	assert.Contains(t, err.Error(), "af/pushed-branch",
		"the plain shape must still name the pushed branch for manual recovery")
	assert.Empty(t, archivedPath)
	assert.Empty(t, archived.ID)
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionArchived {
				t.Fatal("no archived event may be published for a remote archive whose durable claim never landed")
			}
			continue
		default:
		}
		break
	}
}

// TestKeepUnrollableArchiveCommitted_RefusesWhenBytesLeftTheArchive pins the
// #3335 review guard: a rollback error is not proof the archive stayed put.
// The move home can land the bytes and then fail registration repair — which
// commits the pre-archive location while still returning an error
// (session/git's TestMoveWorktree_RepairFailureStillCommitsLocation) — so when
// the instance's worktree path no longer equals the archived path, the helper
// must keep the prior plain double-failure shape instead of publishing an
// Archived projection pointing at a vacated directory.
func TestKeepUnrollableArchiveCommitted_RefusesWhenBytesLeftTheArchive(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	require.NoError(t, inst.Transition(session.BeginArchive()))
	require.NoError(t, inst.Transition(session.CommitArchive()))

	dest, derr := archivedWorktreePath(repoID, "worker")
	require.NoError(t, derr)
	require.NotEqual(t, dest, inst.GetWorktreePath(),
		"precondition: the bytes are at the pre-archive path, not the archive")
	require.Equal(t, srcPath, inst.GetWorktreePath())

	_, ch := manager.events.subscribe()
	cause := errors.New("registration repair failed after the bytes moved home")
	archivedPath, archived, err := manager.keepUnrollableArchiveCommitted(repoID, dest, inst, nil, cause)

	require.ErrorIs(t, err, cause)
	assert.False(t, isMutationCommitted(err),
		"the committed-archive claim must not be made when the worktree is no longer at the archived path")
	assert.Empty(t, archivedPath, "no location may be claimed when the real one is not the archive")
	assert.Empty(t, archived.ID, "no Archived projection may be published for a vacated archive path")
	select {
	case ev := <-ch:
		t.Fatalf("no event may be published on the refused path, got %s", ev.Type)
	default:
	}
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
	calls := 0
	archivePersist = func(m *Manager, rid string, inst *session.Instance) error {
		calls++
		if calls > 1 {
			// The disk heals for the durable retry: only a LANDED Archived row —
			// which is what makes the pushed branch durable at all — may claim
			// committed (#3335 review).
			return prev(m, rid, inst)
		}
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
