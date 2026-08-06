package session

import "time"

// The state epoch (#2135).
//
// The daemon poll DECIDES FROM A SNAPSHOT: it captures pane content, and a moment
// later runs the usage-limit detector and the working check over that capture and
// writes the state they resolve to. Between those two moments an authoritative
// transition can land — a manual `c` retry or the auto-resume scheduler clearing a
// usage-limit block, a kill, an archive — and a decision made from content that
// PREDATES it then overwrites newer truth. #2135 is exactly that: a resume cleared
// the limit, re-delivered the prompt and persisted LiveRunning, and the in-flight
// poll re-parked the session at the wall from a capture taken before the resume —
// in memory AND, via the reset-time arm of the persist gate, on disk. The user was
// shown a limit-blocked session that was in fact working.
//
// stateEpoch makes "has the authoritative state moved since I looked?" answerable
// in ONE comparison. It is bumped under i.mu by every mutation of the lifecycle
// state the daemon reasons about — the liveness axis, the in-flight op axis, and
// the usage-limit reset time — so an observer captures it alongside the content it
// decides from and hands it back when it applies the decision. The apply is then a
// compare-and-set under that same mutex: same epoch → the decision is still about
// the state it was made about, and is applied; different epoch → something newer
// landed, the decision is known-stale, and it is DROPPED.
//
// It is bumped only on a REAL change (the tracked triple actually differs), so a
// re-observation of the state a session is already in — the poll's common case —
// never invalidates another observer's in-flight decision.
//
// Dropping is safe and self-healing precisely because the guard is
// per-observation rather than a time window: the poll re-observes on its next tick
// and re-decides from content that postdates the transition. A session that
// genuinely walks straight back into a usage-limit wall after a resume is
// therefore still parked on the very next tick — which a "suppress limit detection
// for N seconds after a resume" guard could not promise. Under-applying by one
// tick is recoverable; clobbering a newer transition is not.

// StateEpoch returns the instance's lifecycle-state generation counter (#2135).
// Capture it BEFORE the observation a decision will be made from, and hand it back
// to the epoch-scoped applier (TransitionEvent.AtEpoch / SetLimitReachedAtEpoch)
// so a decision that a newer transition has superseded is dropped instead of
// applied. See the file comment for why this is a counter and not a lock.
func (i *Instance) StateEpoch() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.stateEpoch
}

// InFlightOpAndEpoch reads the op axis and the state epoch TOGETHER under one lock.
//
// An observer that reads them separately has a window that neither read closes on
// its own (#2997). The daemon poll skips a session that has an op in flight and,
// further down, captures the epoch its observation will be scoped to. If a fence
// goes up BETWEEN those two reads, the skip has already passed and the epoch it then
// captures is the POST-fence one — so the observation it settles minutes later looks
// current, is applied rather than dropped, and overwrites the liveness the in-flight
// operation depends on. That is the same clobber the fence exists to prevent,
// reached by a path the fence cannot see.
//
// Reading both at once removes the window instead of narrowing it: the epoch is
// necessarily from a moment when the op was None, so any fence raised afterwards
// advances it and the epoch guard drops the observation. Callers must skip on a
// non-None op here, not merely earlier.
func (i *Instance) InFlightOpAndEpoch() (InFlightOp, uint64) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp, i.stateEpoch
}

// lifecycleStateLocked captures the state the epoch tracks: both axes plus the
// usage-limit reset time. Caller holds i.mu.
func (i *Instance) lifecycleStateLocked() (Liveness, InFlightOp, time.Time) {
	return i.liveness, i.inFlightOp, i.limitResetAt
}

// noteStateChangeLocked bumps the epoch when the tracked state differs from the
// snapshot lifecycleStateLocked took before the mutation. Caller holds i.mu.
// Every writer of the two axes or the reset time pairs with it, so "the epoch
// moved" means exactly "the authoritative state changed".
func (i *Instance) noteStateChangeLocked(lv Liveness, op InFlightOp, resetAt time.Time) {
	if i.liveness == lv && i.inFlightOp == op && i.limitResetAt.Equal(resetAt) {
		return
	}
	i.stateEpoch++
}
