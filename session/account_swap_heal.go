package session

// A committed account swap whose replacement is rebuilt by a CREATE rather than
// a respawn (the root agent's reap-and-recreate heal, #4400) reaches none of the
// respawn path's own settlement: RespawnForAccountSwapWithLiveBoundary marks the
// pane-start proof, and demotePendingAccountSwapCarry re-renders the mission,
// but both run off an accountSwapLaunch plan a create never builds. These two
// methods are that path's equivalents, named for what the heal can actually
// prove.

// MarkHealedAccountSwapPanesStarted records the durable pane-start proof for a
// committed swap whose replacement a create has just brought up.
//
// Without it the scheduler reads a healthy, fully-restored root as an
// incomplete replacement boundary: ValidateAccountSwapReplacementPanes fails on
// the carried ReplacementPanesStarted=false, and the repair path stops the root
// and launches it a second time — an avoidable outage plus a second failure
// window (Codex on #4400, round 8).
//
// The caller marks this AFTER the create returns, never before: the proof means
// every pane's start returned successfully, and a create that fails has proven
// nothing. That order leaves a sub-second window in which a scheduler tick can
// still see the unproven marker, which costs exactly the repair this prevents —
// the pre-existing outcome, not a new one.
func (i *Instance) MarkHealedAccountSwapPanesStarted() error {
	return i.markAccountSwapReplacementPanesStarted()
}

// RefreshPendingManualAccountSwapMission re-renders a committed manual swap's
// mission from the conversation outcome the transaction NOW records.
//
// A manual swap embeds that outcome in the mission text itself, so a swap that
// was committed carrying its conversation says the history is still available.
// When a heal cannot resume that conversation it demotes the carry to a stated
// fresh start, and the stored mission would otherwise keep promising the agent
// messages this replacement does not have (Codex on #4400, round 8). The
// respawn path re-renders for the same reason when its own carry falls back.
//
// A no-op for an automatic swap, which states its outcome at delivery instead.
func (i *Instance) RefreshPendingManualAccountSwapMission() {
	i.mu.RLock()
	pending := i.pendingAccountSwap
	var from, to string
	var manual bool
	if pending != nil {
		from, to, manual = pending.From, pending.To, pending.Manual
	}
	i.mu.RUnlock()
	if !manual {
		return
	}
	i.refreshPendingManualAccountSwapMission(from, to)
}
