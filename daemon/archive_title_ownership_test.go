package daemon

import (
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

func TestArchiveTitleClaimsProjectedOwnership(t *testing.T) {
	for _, projection := range []string{"archive report", "relocation", "cleanup", "report and relocation"} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external=%t", projection, external), func(t *testing.T) {
				m, repoID, repoPath := newStatusTestManager(t)
				ownedBranch, startupUnknown := true, false
				data := session.InstanceData{ID: "projected-owner", Title: "feature/login", Path: repoPath, Status: session.Ready, BackendType: "local", Worktree: session.GitWorktreeData{
					RepoPath: repoPath, WorktreePath: repoPath + "/missing-worktree", ExternalWorktree: external, BranchCreatedByUs: &ownedBranch,
				}}
				if projection != "archive report" {
					state := sessiongit.RelocationRecoveryClaimStale
					if projection == "cleanup" {
						state = sessiongit.RelocationRecoveryCleanupReady
					}
					data.Worktree.RelocationRecovery = &session.GitWorktreeRelocationRecoveryData{State: state, OriginalExternalWorktree: &external, OriginalBranchCreatedByUs: &ownedBranch, OriginalStartupStateUnknown: &startupUnknown}
				}
				if projection == "archive report" || projection == "report and relocation" {
					data.ArchiveReport = &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{Path: "/retained", Skipped: []sessiongit.ArchiveSkippedEntry{{Path: "locked", Reason: sessiongit.ArchiveSkipPermissionDenied}}}}}
				}
				// The real writer applies ForStorage; assert its compatibility projection
				// rather than constructing ExternalWorktree=true as the alleged ownership.
				require.NoError(t, appendInstanceData(repoID, data))
				disk, err := loadRepoInstanceData(repoID)
				require.NoError(t, err)
				require.Len(t, disk, 1)
				require.True(t, disk[0].Worktree.ExternalWorktree)
				failLoadFor(t, data.Title)
				_, _, release, _, err := m.reserveCreate(CreateSessionRequest{Title: "feature-login", RepoPath: repoPath, Program: "claude"})
				if release != nil {
					defer release()
				}
				require.Nil(t, m.instances[daemonInstanceKey(repoID, data.Title)], "the disk-only claim must decide admission")
				if external {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "archive directory")
				}
			})
		}
	}
}

func TestArchiveTitleMalformedOwnershipRefusesClaim(t *testing.T) {
	m := &Manager{}
	disk := []session.InstanceData{{Title: "feature/login", Worktree: session.GitWorktreeData{ExternalWorktree: true, RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{State: sessiongit.RelocationRecoveryClaimStale}}}}
	err := m.validateArchiveTitleLocked("repo", "feature-login", disk, nil, false)
	require.ErrorIs(t, err, errTitleCheckFatal, "an unreadable ownership claim must not be silently skipped or retried through 10,000 suffixes")
	require.NoError(t, m.validateArchiveTitleLocked("repo", "unrelated", disk, nil, false))
}

func TestArchiveTitleClaimsRenamedArchivedPath(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	data := session.InstanceData{Title: "foo (archived)", Path: repoPath, Status: session.Archived, BackendType: "local", Worktree: session.GitWorktreeData{WorktreePath: repoPath + "/archived/foo"}}
	require.NoError(t, appendInstanceData(repoID, data))
	failLoadFor(t, data.Title)
	_, _, release, _, err := m.reserveCreate(CreateSessionRequest{Title: "foo", RepoPath: repoPath, Program: "claude"})
	if release != nil {
		defer release()
	}
	require.ErrorContains(t, err, `session titled "foo (archived)" already maps to archive directory "foo"`)
}
