package session

import "github.com/sachiniyer/agent-factory/session/git"

// The archive-rollback projections: how a record's worktree facts become the
// git layer's rollback fence, and back. Split from storage.go, which holds the
// record shapes themselves.

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func archiveRollbackFence(data InstanceData) *git.ArchiveRollbackFence {
	fence := &git.ArchiveRollbackFence{
		OriginalStartupStateUnknown: data.StartupStateUnknown,
		OriginalExternalWorktree:    data.Worktree.ExternalWorktree,
		OriginalBranchCreatedByUs:   cloneBoolPointer(data.Worktree.BranchCreatedByUs),
		RelocationRecoveryProjected: true,
	}
	if recovery := data.Worktree.RelocationRecovery; recovery != nil {
		fence.OriginalRelocationRecovery = &git.ArchiveRollbackRelocationRecovery{
			State:                              recovery.State,
			CleanupLifecycle:                   recovery.CleanupLifecycle,
			AlternatePath:                      recovery.AlternatePath,
			IdentityKnown:                      recovery.IdentityKnown,
			Device:                             recovery.Device,
			Inode:                              recovery.Inode,
			FileType:                           recovery.FileType,
			CleanupGeneration:                  recovery.CleanupGeneration,
			CleanupOriginalExternalWorktree:    cloneBoolPointer(recovery.CleanupOriginalExternalWorktree),
			CleanupOriginalBranchCreatedByUs:   cloneBoolPointer(recovery.CleanupOriginalBranchCreatedByUs),
			CleanupOriginalStartupStateUnknown: cloneBoolPointer(recovery.CleanupOriginalStartupStateUnknown),
			OriginalExternalWorktree:           cloneBoolPointer(recovery.OriginalExternalWorktree),
			OriginalBranchCreatedByUs:          cloneBoolPointer(recovery.OriginalBranchCreatedByUs),
			OriginalStartupStateUnknown:        cloneBoolPointer(recovery.OriginalStartupStateUnknown),
		}
	}
	return fence
}

func archiveRollbackRelocationRecovery(recovery *git.ArchiveRollbackRelocationRecovery) *GitWorktreeRelocationRecoveryData {
	if recovery == nil {
		return nil
	}
	return &GitWorktreeRelocationRecoveryData{
		State:                              recovery.State,
		CleanupLifecycle:                   recovery.CleanupLifecycle,
		AlternatePath:                      recovery.AlternatePath,
		IdentityKnown:                      recovery.IdentityKnown,
		Device:                             recovery.Device,
		Inode:                              recovery.Inode,
		FileType:                           recovery.FileType,
		CleanupGeneration:                  recovery.CleanupGeneration,
		CleanupOriginalExternalWorktree:    cloneBoolPointer(recovery.CleanupOriginalExternalWorktree),
		CleanupOriginalBranchCreatedByUs:   cloneBoolPointer(recovery.CleanupOriginalBranchCreatedByUs),
		CleanupOriginalStartupStateUnknown: cloneBoolPointer(recovery.CleanupOriginalStartupStateUnknown),
		OriginalExternalWorktree:           cloneBoolPointer(recovery.OriginalExternalWorktree),
		OriginalBranchCreatedByUs:          cloneBoolPointer(recovery.OriginalBranchCreatedByUs),
		OriginalStartupStateUnknown:        cloneBoolPointer(recovery.OriginalStartupStateUnknown),
	}
}

func archiveReportKillFence(report git.ArchiveReport) *GitWorktreeRelocationRecoveryData {
	tree := report.RetainedTrees[0]
	fence := report.RollbackFence
	originalExternal := fence.OriginalExternalWorktree
	originalBranch := cloneBoolPointer(fence.OriginalBranchCreatedByUs)
	originalStartup := fence.OriginalStartupStateUnknown
	return &GitWorktreeRelocationRecoveryData{
		// A previous reader refuses explicit kill while any recovery record is
		// unresolved, but its restore path consumes an identity-qualified
		// AlternatePath as a worktree candidate. Retained trees can be snapshots
		// from an older archive cycle, so never publish their pathname here. A
		// claim-stale record with no alternate and the retained source's identity
		// cannot match the cross-filesystem published archive and therefore makes
		// both kill and restore fail closed. Current readers remove this synthetic
		// record through RollbackFence before reconstructing the worktree.
		State:                       git.RelocationRecoveryClaimStale,
		IdentityKnown:               true,
		Device:                      tree.Device,
		Inode:                       tree.Inode,
		FileType:                    tree.FileType,
		OriginalExternalWorktree:    &originalExternal,
		OriginalBranchCreatedByUs:   originalBranch,
		OriginalStartupStateUnknown: &originalStartup,
	}
}
