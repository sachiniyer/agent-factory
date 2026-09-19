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
// own copy.
func ambiguousHandoffDelivery(status PromptDeliveryStatus) bool {
	return status == PromptCouldNotConfirm || status == PromptSentUnverified
}

// confirmableHandoffDelivery reports whether an operator may retire a pending
// mission on the attestation that it already landed (#4429): any ambiguous
// verdict, plus a recorded delivery whose settle a crash interrupted.
// Positive non-delivery is excluded — automatic recovery owns that resend.
func confirmableHandoffDelivery(status PromptDeliveryStatus) bool {
	return ambiguousHandoffDelivery(status) || status == PromptDelivered
}
