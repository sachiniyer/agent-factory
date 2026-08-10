package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

func archivedInstanceWithReport(t *testing.T) *session.Instance {
	t.Helper()
	instance, err := session.FromInstanceData(session.InstanceData{
		ID: "session-id", Title: "worker", Status: session.Archived, Liveness: session.LiveArchived,
		Worktree: session.GitWorktreeData{
			RepoPath: "/repos/project", WorktreePath: "/archives/worker",
			SessionName: "worker", BranchName: "af/worker",
		},
		ArchiveReport: &sessiongit.ArchiveReport{
			RetainedTrees: []sessiongit.ArchiveRetainedTree{{
				Path: "/repos/.af-source-0123456789abcdef0123456789abcdef", IdentityKnown: true,
				Device: 1, Inode: 2, FileType: 0o040000,
				Skipped: []sessiongit.ArchiveSkippedEntry{{
					Path: "private/locked\ncredential", Reason: sessiongit.ArchiveSkipPermissionDenied,
				}},
			}},
		},
	})
	require.NoError(t, err)
	return instance
}

func TestRestoredArchiveResultSurfacesDurableReportAsCommitted(t *testing.T) {
	path, err := restoredArchiveResult(archivedInstanceWithReport(t), "/worktrees/worker")
	require.Equal(t, "/worktrees/worker", path)
	require.True(t, isMutationCommitted(err), "the completed restore must not look retryable: %v", err)
	require.Contains(t, err.Error(), `"private/locked\ncredential"`)
	require.NotContains(t, err.Error(), "private/locked\ncredential\n",
		"the filename's newline must remain quoted rather than injecting output")
}

func TestFailedRestoredArchiveResultJoinsRespawnFailureAndReport(t *testing.T) {
	spawnErr := errors.New("agent spawn failed")
	path, err := failedRestoredArchiveResult(archivedInstanceWithReport(t), "/worktrees/worker", spawnErr)
	require.Equal(t, "/worktrees/worker", path, "the committed relocate path must survive the partial restore")
	require.True(t, isMutationCommitted(err), "retrying the archived restore would repeat a landed move: %v", err)
	require.ErrorIs(t, err, spawnErr, "the report must not hide the respawn failure")
	require.Contains(t, err.Error(), "incomplete archive")
}

func TestGhostCleanupReadsThroughArchiveRollbackFence(t *testing.T) {
	stored := archivedInstanceWithReport(t).ToInstanceData().ForStorage()
	require.True(t, stored.Worktree.ExternalWorktree, "precondition: storage projects the old-reader fence")
	require.True(t, ghostWorktreeRemovable(&stored),
		"the current daemon must remove its compatibility fence before deciding cleanup ownership")
}

func TestRestoreArchivedRespawnFailurePreservesReportThroughLostRetry(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	instance, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	spawnErr := errors.New("agent spawn failed")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: spawnErr}
	instance.SetBackend(backend)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	worktree, err := instance.GetGitWorktree()
	require.NoError(t, err)
	worktree.RestoreArchiveReport(sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: "/repos/.af-source-0123456789abcdef0123456789abcdef", IdentityKnown: true,
		Device: 1, Inode: 2, FileType: 0o040000,
		Skipped: []sessiongit.ArchiveSkippedEntry{{
			Path: "private/locked\ncredential", Reason: sessiongit.ArchiveSkipPermissionDenied,
		}},
	}}})

	partialPath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.True(t, isMutationCommitted(err), "the worktree move landed before respawn failed: %v", err)
	require.ErrorIs(t, err, spawnErr)
	require.Contains(t, err.Error(), "incomplete archive")
	require.Equal(t, instance.GetWorktreePath(), partialPath)
	require.Equal(t, session.Lost, instance.GetStatus())

	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	recoveredPath, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "worker", RepoID: repoID})
	require.True(t, isMutationCommitted(err), "the Lost retry must surface the retained report: %v", err)
	require.Contains(t, err.Error(), "incomplete archive")
	require.Equal(t, partialPath, recoveredPath)
	require.Equal(t, session.Running, instance.GetStatus())
	require.Contains(t, instance.ToInstanceData().ArchiveWarning, "private/locked")
	require.Nil(t, instance.ToInstanceData().ArchiveReport, "the full report must stay out of the recovered live snapshot")
}

func TestArchiveCommitWarningJoinsReportAndHookFailure(t *testing.T) {
	hookErr := errors.New("hook failed")
	err := archiveCommitWarning(archivedInstanceWithReport(t), hookErr)
	require.True(t, isMutationCommitted(err))
	require.Contains(t, err.Error(), "incomplete archive")
	require.ErrorIs(t, err, hookErr, "joining the report must not hide the hook failure")
}
