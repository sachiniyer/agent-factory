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
