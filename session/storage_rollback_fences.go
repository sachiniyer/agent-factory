package session

// RestoreArchiveRollbackFence removes the previous-release safety projection
// from a persisted row. FromInstanceData uses it before reconstructing an
// Instance; storage-only cleanup paths use it before manually reconstructing a
// GitWorktree. Keeping that decoding here prevents a current daemon from
// mistaking its own old-reader fence for the session's real ownership.
func (d InstanceData) RestoreArchiveRollbackFence() InstanceData {
	if d.ArchiveReport == nil || d.ArchiveReport.RollbackFence == nil {
		return d
	}
	fence := d.ArchiveReport.RollbackFence
	d.StartupStateUnknown = fence.OriginalStartupStateUnknown
	d.Worktree.ExternalWorktree = fence.OriginalExternalWorktree
	d.Worktree.BranchCreatedByUs = cloneBoolPointer(fence.OriginalBranchCreatedByUs)
	if fence.RelocationRecoveryProjected {
		d.Worktree.RelocationRecovery = archiveRollbackRelocationRecovery(fence.OriginalRelocationRecovery)
	}
	return d
}

// RestoreRuntimeCleanupRollbackFence removes the ordinary-sandbox projection
// ForStorage writes so a current reader interprets the row as the ordinary
// session it is, not the retention row a previous release needs to see. The
// previous release's FromInstanceData only rebuilds a teardown for a UserKilled
// tombstone or an unknown-cleanup row, so ForStorage publishes an ordinary
// sandbox row's RuntimeCleanup under a projected RuntimeCleanupStateUnknown=true
// and saves the real false here. A current reader restores the real value before
// deciding whether the row is unknown-cleanup or ordinary; a rolled-back reader
// ignores this additive field and treats the projected row as a retention row,
// rebuilding and preserving the handle instead of orphaning the runtime.
func (d InstanceData) RestoreRuntimeCleanupRollbackFence() InstanceData {
	if d.OriginalRuntimeCleanupStateUnknown == nil {
		return d
	}
	d.RuntimeCleanupStateUnknown = *d.OriginalRuntimeCleanupStateUnknown
	d.OriginalRuntimeCleanupStateUnknown = nil
	return d
}
