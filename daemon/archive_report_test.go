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

func TestArchiveCommitWarningJoinsReportAndHookFailure(t *testing.T) {
	hookErr := errors.New("hook failed")
	err := archiveCommitWarning(archivedInstanceWithReport(t), hookErr)
	require.True(t, isMutationCommitted(err))
	require.Contains(t, err.Error(), "incomplete archive")
	require.ErrorIs(t, err, hookErr, "joining the report must not hide the hook failure")
}
