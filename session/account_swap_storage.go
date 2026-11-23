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

// projectOffBoxSwapInFlightForPreviousRelease keeps a rollback binary from
// silently unfencing a row whose daemon died between sending a swap request
// and reconciling it (#7793): to the previous release the record's meaning is
// unknown, so StartupStateUnknown — a lifecycle fence that release already
// loads inert — is projected over it, exactly like the pending account swap.
// It runs FIRST of the pending projections so its captured original is the
// row's real value; its restore therefore runs last. An older binary ignores
// the record field on rewrite and drops it, but keeps the projected fence —
// the row degrades to the same kill-only state a swap-indeterminate row has
// always had, never to an unfenced one.
func (d InstanceData) projectOffBoxSwapInFlightForPreviousRelease() InstanceData {
	if d.OffBoxSwapInFlight == nil {
		return d
	}
	rec := cloneInFlightOffBoxSwap(d.OffBoxSwapInFlight)
	if rec.OriginalStartupStateUnknown == nil {
		original := d.StartupStateUnknown
		rec.OriginalStartupStateUnknown = &original
	}
	d.OffBoxSwapInFlight = rec
	d.StartupStateUnknown = true
	return d
}

// RestoreOffBoxSwapInFlightRollbackFence removes the compatibility projection
// before a current reader interprets the lifecycle. The in-flight projector
// runs FIRST in ForStorage — it captures the real StartupStateUnknown before
// the account-swap, handoff, and rebuild-teardown projectors overwrite it —
// so its restore runs LAST, after theirs (#7793).
//
// Unlike the sibling restores it does NOT put the projected-away value back:
// the record itself is this build's fence. While it exists the row's startup
// state IS unknown — the recorded request may still be queued on the host, so
// no gate may treat the row as settled — and the projected flag stays up in
// memory as the ONE predicate every lifecycle/runtime gate already reads. The
// only way down is the record's own definite verdict, which resolves the flag
// when it clears the record.
func (d InstanceData) RestoreOffBoxSwapInFlightRollbackFence() InstanceData {
	if d.OffBoxSwapInFlight == nil || d.OffBoxSwapInFlight.OriginalStartupStateUnknown == nil {
		return d
	}
	rec := cloneInFlightOffBoxSwap(d.OffBoxSwapInFlight)
	d.StartupStateUnknown = true
	rec.OriginalStartupStateUnknown = nil
	d.OffBoxSwapInFlight = rec
	return d
}

// restoreMissingAccountSwapMissionEvidence fails closed whenever a pending manual
// transaction has replacement panes but no transaction-scoped verdict. Its
// session-wide prompt verdict may describe an unrelated prompt, so it cannot
// prove that this mission missed; a current delivery writes evidence here instead.
func (d InstanceData) restoreMissingAccountSwapMissionEvidence() InstanceData {
	pending := d.PendingAccountSwap
	if pending == nil || !pending.Manual || !pending.ReplacementPanesStarted ||
		pending.MissionDeliveryStatus != "" {
		return d
	}
	pending = cloneAccountSwapData(pending)
	pending.MissionDeliveryStatus = PromptCouldNotConfirm
	d.PendingAccountSwap = pending
	return d
}
