package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

func TestGhostWorktreeRemovable_RestoresOwnershipBeforeRollbackFence(t *testing.T) {
	originalExternal := false
	originalBranchCreated := true
	originalStartupUnknown := false
	projectedBranchCreated := false
	data := &session.InstanceData{
		Worktree: session.GitWorktreeData{
			RepoPath:          "/missing/repo",
			WorktreePath:      "/archived/worktree",
			SessionName:       "ghost",
			BranchName:        "af/ghost",
			ExternalWorktree:  true,
			BranchCreatedByUs: &projectedBranchCreated,
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State:                       git.RelocationRecoveryCleanupStalled,
				IdentityKnown:               true,
				CleanupGeneration:           "generation",
				OriginalExternalWorktree:    &originalExternal,
				OriginalBranchCreatedByUs:   &originalBranchCreated,
				OriginalStartupStateUnknown: &originalStartupUnknown,
			},
		},
	}

	if !ghostWorktreeRemovable(data) {
		t.Fatal("the rollback-safe external-worktree projection was read before the recovery record restored AF ownership")
	}
}
