package session

import "fmt"

// RestoreRelocationRecoveryOriginals reverses the rollback-safe ownership and
// lifecycle projection made by ForStorage before a current reader interprets a
// recovery record. Normal instance loading and daemon ghost cleanup share this
// boundary so neither can read compatibility fields as deletion authority.
func (data InstanceData) RestoreRelocationRecoveryOriginals() (InstanceData, error) {
	recovery := data.Worktree.RelocationRecovery
	if recovery == nil {
		return data, nil
	}
	recoveryClone := *recovery
	data.Worktree.RelocationRecovery = &recoveryClone
	recovery = &recoveryClone
	// First, so the shape check below never mistakes a real stall projected
	// through the worktree-missing fence for the bare fence itself.
	if err := restoreMissingFencedState(recovery); err != nil {
		return InstanceData{}, err
	}
	state, err := runtimeRelocationRecoveryState(recovery)
	if err != nil {
		return InstanceData{}, err
	}
	originalExternal := recovery.OriginalExternalWorktree
	originalBranch := recovery.OriginalBranchCreatedByUs
	originalStartup := recovery.OriginalStartupStateUnknown
	if recovery.CleanupLifecycle != "" {
		originalExternal = recovery.CleanupOriginalExternalWorktree
		originalBranch = recovery.CleanupOriginalBranchCreatedByUs
		originalStartup = recovery.CleanupOriginalStartupStateUnknown
	}
	if originalExternal == nil || originalBranch == nil || originalStartup == nil {
		return InstanceData{}, fmt.Errorf("rollback safety metadata is missing")
	}
	data.Worktree.ExternalWorktree = *originalExternal
	branchCreatedByUs := *originalBranch
	data.Worktree.BranchCreatedByUs = &branchCreatedByUs
	data.StartupStateUnknown = *originalStartup

	// Return a canonical current-reader record. Ghost checkpoint writers persist
	// this row directly, so leaving the previous release's safe ownership values
	// in Original* would make the next ForStorage projection forget the real owner.
	recovery.State = state
	recovery.CleanupLifecycle = ""
	recovery.OriginalExternalWorktree = cloneBoolPointer(originalExternal)
	recovery.OriginalBranchCreatedByUs = cloneBoolPointer(originalBranch)
	recovery.OriginalStartupStateUnknown = cloneBoolPointer(originalStartup)
	recovery.CleanupOriginalExternalWorktree = nil
	recovery.CleanupOriginalBranchCreatedByUs = nil
	recovery.CleanupOriginalStartupStateUnknown = nil
	// The worktree-missing rollback projection is not a relocation record; with
	// the ownership values restored above, it reads back as the flag it stands
	// for (#5102). An older binary that rewrote the row dropped the flag and its
	// reason but kept this record, which is why the shape, not Missing, decides.
	if isWorktreeMissingRollbackFence(recovery) {
		data.Worktree.RelocationRecovery = nil
		data.Worktree.Missing = true
		if data.Worktree.MissingReason == "" {
			data.Worktree.MissingReason = WorktreeMissingDeletedReason(data.Worktree.WorktreePath)
		}
	}
	return data, nil
}
