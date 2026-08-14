package session

// projectPendingAccountSwapForPreviousRelease keeps a rollback binary from
// ignoring the recovery obligation, treating the rewritten Account as an
// explicit pin, and resuming an unrelated conversation under that identity.
// StartupStateUnknown is a lifecycle fence that release already loads inert.
// The nested original makes the projection idempotent when a storage-only path
// rewrites an already-projected row.
func (d InstanceData) projectPendingAccountSwapForPreviousRelease() InstanceData {
	if d.PendingAccountSwap == nil {
		return d
	}
	pending := cloneAccountSwapData(d.PendingAccountSwap)
	if pending.OriginalStartupStateUnknown == nil {
		original := d.StartupStateUnknown
		pending.OriginalStartupStateUnknown = &original
	}
	d.PendingAccountSwap = pending
	d.StartupStateUnknown = true
	return d
}

// RestoreAccountSwapRollbackFence removes the compatibility projection before
// a current reader interprets the pending-delivery lifecycle.
func (d InstanceData) RestoreAccountSwapRollbackFence() InstanceData {
	if d.PendingAccountSwap == nil || d.PendingAccountSwap.OriginalStartupStateUnknown == nil {
		return d
	}
	pending := cloneAccountSwapData(d.PendingAccountSwap)
	d.StartupStateUnknown = *pending.OriginalStartupStateUnknown
	pending.OriginalStartupStateUnknown = nil
	d.PendingAccountSwap = pending
	return d
}
