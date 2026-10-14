package session

// projectPendingHandoffForPreviousRelease keeps a rollback binary from
// replaying an ambiguous pending mission. That reader ignores the additive
// mission verdict, but already treats StartupStateUnknown as inert. The saved
// original lets a current reader recover the real startup state.
func (d InstanceData) projectPendingHandoffForPreviousRelease() InstanceData {
	if d.PendingHandoffMission == "" || !ambiguousHandoffDelivery(d.HandoffDeliveryStatus) {
		return d
	}
	if d.HandoffOriginalStartupStateUnknown == nil {
		original := d.StartupStateUnknown
		d.HandoffOriginalStartupStateUnknown = &original
	}
	d.StartupStateUnknown = true
	return d
}

// RestoreHandoffRollbackFence removes the compatibility projection before a
// current reader decides whether the known replacement pane can be retried.
func (d InstanceData) RestoreHandoffRollbackFence() InstanceData {
	if d.HandoffOriginalStartupStateUnknown == nil {
		return d
	}
	d.StartupStateUnknown = *d.HandoffOriginalStartupStateUnknown
	d.HandoffOriginalStartupStateUnknown = nil
	return d
}

// restoreMissingHandoffMissionEvidence migrates a pending mission written by a
// release that had no mission-scoped verdict. Missing evidence is ambiguous: it
// permits an operator retry after inspection, but never automatic replay.
func (d InstanceData) restoreMissingHandoffMissionEvidence() InstanceData {
	if d.PendingHandoffMission != "" && d.HandoffDeliveryStatus == "" {
		d.HandoffDeliveryStatus = PromptCouldNotConfirm
	}
	return d
}

// ambiguousHandoffDelivery reports whether a mission verdict leaves it unknown
// whether the mission landed. It is the one list of those verdicts: the retry,
// confirm, and rollback-projection gates all read it rather than keeping their
// own copy. HandoffDeliveryUnresolved is its exported mirror, so the daemon's
// raw ghost predicate (rawTaskRunHoldsSlot) reads the same list rather than
// keeping its own.
func ambiguousHandoffDelivery(status PromptDeliveryStatus) bool {
	return status == PromptCouldNotConfirm || status == PromptSentUnverified
}

// HandoffDeliveryUnresolved reports whether a pending handoff mission's
// delivery verdict leaves it unknown whether the mission landed. It is the
// exported mirror of ambiguousHandoffDelivery so daemon code that decides on a
// raw persisted row — which has no Instance and so cannot call the unexported
// helper — still reads the one verdict list rather than duplicating it.
//
// projectPendingHandoffForPreviousRelease projects StartupStateUnknown=true
// onto any row whose PendingHandoffMission carries one of these verdicts, with
// no backend gate, so the projection reaches local rows mid-mission exactly as
// it reaches sandbox rows. A current reader restores the fence before deciding
// (RestoreHandoffRollbackFence); a raw reader that does not would short-circuit
// on the projected value and release a ghost whose loaded form holds, which is
// the divergence this helper exists to close — see rawTaskRunHoldsSlot.
func HandoffDeliveryUnresolved(status PromptDeliveryStatus) bool {
	return ambiguousHandoffDelivery(status)
}

// confirmableHandoffDelivery reports whether an operator may retire a pending
// mission on the attestation that it already landed (#4429): any ambiguous
// verdict, plus a recorded delivery whose settle a crash interrupted.
// Positive non-delivery is excluded — automatic recovery owns that resend.
// owesMissionDeliveryLocked reports whether this session still owes a takeover
// brief someone has to resolve: an agent handoff's pending mission, or a manual
// account swap's mission whose delivery was never confirmed. Both settle their
// fence on the incoming runtime (#4429), so neither shows on the op axis, and
// both are resolved by the same operator verbs — retry-limit, or its
// --delivered attestation. Caller holds i.mu.
func (i *Instance) owesMissionDeliveryLocked() bool {
	return i.pendingHandoffMission != "" || i.pendingManualAccountSwapDeliveryUnconfirmedLocked()
}

func confirmableHandoffDelivery(status PromptDeliveryStatus) bool {
	return ambiguousHandoffDelivery(status) || status == PromptDelivered
}
