package session

// projectRebuildTeardownForPreviousRelease keeps a rollback binary from
// driving a row whose post-rebuild stale-pane teardown was never confirmed.
// That reader ignores the additive RebuildTeardownPending flag, finds the
// rebuilt worktree present, and would reattach the surviving pane — whose cwd
// is the deleted inode — as Running: #5370 through the rollback door. It
// already treats StartupStateUnknown as inert (loads without Start, invisible
// to the status poll and the lost-restore loop), so the stale pane simply sits
// unclaimed until a current reader restores the row and finishes the teardown.
// The saved original lets that reader recover the real startup verdict.
func (d InstanceData) projectRebuildTeardownForPreviousRelease() InstanceData {
	if !d.RebuildTeardownPending {
		return d
	}
	if d.RebuildTeardownOriginalStartupStateUnknown == nil {
		original := d.StartupStateUnknown
		d.RebuildTeardownOriginalStartupStateUnknown = &original
	}
	d.StartupStateUnknown = true
	return d
}

// RestoreRebuildTeardownRollbackFence removes the compatibility projection
// before a current reader interprets the row — the real StartupStateUnknown
// comes back, and the pending flag itself keeps the teardown gate armed.
func (d InstanceData) RestoreRebuildTeardownRollbackFence() InstanceData {
	if d.RebuildTeardownOriginalStartupStateUnknown == nil {
		return d
	}
	d.StartupStateUnknown = *d.RebuildTeardownOriginalStartupStateUnknown
	d.RebuildTeardownOriginalStartupStateUnknown = nil
	return d
}
