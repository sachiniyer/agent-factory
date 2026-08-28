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

func archivedInstanceComplete(t *testing.T) *session.Instance {
	t.Helper()
	instance, err := session.FromInstanceData(session.InstanceData{
		ID: "session-id", Title: "worker", Status: session.Archived, Liveness: session.LiveArchived,
		Worktree: session.GitWorktreeData{
			RepoPath: "/repos/project", WorktreePath: "/archives/worker",
			SessionName: "worker", BranchName: "af/worker",
		},
		// No ArchiveReport: a complete archive (GetArchiveReport().Empty()).
	})
	require.NoError(t, err)
	return instance
}

func TestFailedRestoredArchiveResultJoinsRespawnFailureAndReport(t *testing.T) {
	spawnErr := errors.New("agent spawn failed")
	path, err := failedRestoredArchiveResult(archivedInstanceWithReport(t), "/worktrees/worker", spawnErr)
	require.Equal(t, "/worktrees/worker", path, "the committed relocate path must survive the partial restore")
	require.True(t, isMutationCommitted(err), "retrying the archived restore would repeat a landed move: %v", err)
	require.ErrorIs(t, err, spawnErr, "the report must not hide the respawn failure")
	require.Contains(t, err.Error(), "incomplete archive")
}

// TestFailedRestoredArchiveResultCompleteArchiveReturnsCommittedPath pins the
// regression this bug report is about: a restore whose worktree relocate already
// landed must not look failed-nothing-committed for a COMPLETE archive. The
// function is reachable only after RestoreArchivedWorktreeWithClaim succeeded —
// the worktree has moved, so a plain error and an empty path would (a) lose the
// relocated location the caller needs and (b) let a transport treat a landed
// move as safely retryable, which repeats the move and fails the source-exists
// guard. A complete archive carries no skipped-file warning, so the committed
// marker wraps only the failure — the same shape keepUnrollableArchiveCommitted
// established for the archive-side durable mutation (#3235).
func TestFailedRestoredArchiveResultCompleteArchiveReturnsCommittedPath(t *testing.T) {
	instance := archivedInstanceComplete(t)
	require.True(t, instance.GetArchiveReport().Empty(),
		"precondition: the archive is complete, with no retained/skipped content")

	spawnErr := errors.New("agent spawn failed")
	path, err := failedRestoredArchiveResult(instance, "/worktrees/worker", spawnErr)

	require.Equal(t, "/worktrees/worker", path,
		"the committed relocate path must be returned so the caller can find the moved worktree")
	require.True(t, isMutationCommitted(err),
		"a landed relocate must not read as a retryable failure: %v", err)
	require.ErrorIs(t, err, spawnErr, "the committed marker must not hide the failure")
	require.NotContains(t, err.Error(), "incomplete archive",
		"a complete archive must not surface an incomplete-archive warning")
	require.Contains(t, err.Error(), spawnErr.Error(),
		"the underlying failure must remain the warning's text")
}

func TestFailedArchiveErrorJoinsRepairFailureAndReport(t *testing.T) {
	repairErr := errors.New("registration repair failed")
	err := failedArchiveError(archivedInstanceWithReport(t), repairErr)
	require.False(t, isMutationCommitted(err), "the session is Lost, not durably Archived")
	require.ErrorIs(t, err, repairErr, "the report must not hide the repair failure")
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
	worktree, err := instance.GetGitWorktree()
	require.NoError(t, err)

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
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

// TestRestoreArchivedRespawnFailureReturnsCommittedPathForCompleteArchive is the
// complete-archive twin of the test above: the same re-spawn-after-relocate
// failure, but for an archive with NO retained/skipped content. Before the fix
// failedRestoredArchiveResult returned ("", plain error) here, so the RestoreArchived
// control handler hit its `if !resp.record(err) { return err }` branch and the
// client saw a bare error with no path and no committed marker — indistinguishable
// from a restore that failed before moving anything, even though the worktree had
// already landed. A complete archive surfaces no skipped-file warning, but the
// relocate landed, so the response must still carry the committed marker and the
// relocated path, and the Lost loop must recover the session in place.
func TestRestoreArchivedRespawnFailureReturnsCommittedPathForCompleteArchive(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	instance, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	spawnErr := errors.New("agent spawn failed")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: spawnErr}
	instance.SetBackend(backend)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	// No RestoreArchiveReport install: the archive is complete, so
	// GetArchiveReport().Empty() is the case the bug mishandled.

	partialPath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.True(t, isMutationCommitted(err),
		"the worktree move landed before respawn failed, so it must not read as retryable: %v", err)
	require.ErrorIs(t, err, spawnErr, "the committed marker must not hide the respawn failure")
	require.NotContains(t, err.Error(), "incomplete archive",
		"a complete archive carries no skipped-file warning")
	require.Equal(t, instance.GetWorktreePath(), partialPath,
		"the relocated worktree path must be returned, not discarded")
	require.Equal(t, session.Lost, instance.GetStatus(),
		"a failed re-spawn leaves the session Lost for the recovery loop")

	// The Lost retry must still heal a complete archive: re-spawn succeeds, the
	// session returns to Running, and — with no retained report — the restore
	// completes cleanly rather than reporting a committed warning.
	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	recoveredPath, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err, "a complete archive has no warning to surface once the re-spawn succeeds")
	require.Equal(t, partialPath, recoveredPath,
		"recovery re-spawns in place; the worktree location must be unchanged")
	require.Equal(t, session.Running, instance.GetStatus())
	require.Empty(t, instance.ToInstanceData().ArchiveWarning,
		"a complete archive must leave no committed warning on the recovered row")
	require.Nil(t, instance.ToInstanceData().ArchiveReport)
}

func TestArchiveCommitWarningJoinsReportAndHookFailure(t *testing.T) {
	hookErr := errors.New("hook failed")
	err := archiveCommitWarning(archivedInstanceWithReport(t), hookErr)
	require.True(t, isMutationCommitted(err))
	require.Contains(t, err.Error(), "incomplete archive")
	require.ErrorIs(t, err, hookErr, "joining the report must not hide the hook failure")
}
