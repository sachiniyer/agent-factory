package session

import (
	"slices"
	"time"
)

// Durable named-account limit observations (#6802): the ledger a session
// files against an (agent, account) identity when it parks on a usage wall.
// It outlives the live wall — a swap replacement clears the session's park
// but cannot make the exhausted account eligible again — and it feeds the
// daemon's account exclusion plus the postdate boundary a retraction must
// beat. Rows merge only on a changed fact; a provider-authoritative record
// corrects a row's instant in either direction (#6944 review).

// recordAccountLimitObservationLocked writes the named-identity wall into the
// durable observation list. wasParked says the session was already parked at
// LiveLimitReached when this observation landed — i.e. the call is a re-read
// of a banner still displayed, not a fresh wall event.
//
// The record mutates only when a FACT changes (#6802 review — re-seeing an
// unchanged wall must never become a write):
//   - a first sighting appends a row;
//   - a re-sight while NOT parked is a new wall event — the account recovered
//     and hit the limit again — so the same claim's sighting instant advances
//     and the change is durable;
//   - a re-sight while parked that tightens the reset claim updates it — "a
//     new reset" is a changed fact — and re-stamps the sighting.
//
// A parked re-sight that changes no field is a NO-OP: the wall's event time
// is when its fact was first observed, and the stale banner still on screen
// adds nothing a retraction must beat. Callers re-detecting an unchanged
// banner every tick would otherwise rewrite the row (and republish the
// session) once per second for as long as the session stayed parked.
// Caller holds i.mu.
func (i *Instance) recordAccountLimitObservationLocked(agent, account string, resetAt time.Time, evidence string, wasParked bool) {
	i.recordAccountLimitObservationAtLocked(agent, account, resetAt, evidence, wasParked, false, "", instanceNow().UTC())
}

// recordAccountLimitObservationAtLocked is recordAccountLimitObservationLocked
// with the sighting instant supplied by the caller: a park that copies a wall
// it did not itself observe stamps the SOURCE's event time, never a later
// instant — a copy that postdates its source observation outlives every
// retraction that was already on its way between the source's sighting and the
// park (#6880). Caller holds i.mu.
//
// authoritative marks resetAt as the provider's own record rather than a
// pane-derived read: the merged row takes that instant in EITHER direction,
// because the later-only merge would otherwise pin a bound the record
// disproved — and the row feeds account exclusion plus the postdate
// boundary a retraction must beat (#6944 review).
//
// sameWallEvidence carries the provenance a timed-row correction needs: the
// parked wall's own clause when the sighting IS that wall's re-read (its
// banner is the parked banner — the caller proves it; a wider capture may
// have enriched the clause's text, so the sighting's own evidence cannot be
// the match), and "" when no such proof holds. A timed row matches it to be
// corrected; "" never matches — an untimed row needs no provenance, it has
// no instant to lower (#6975 review).
func (i *Instance) recordAccountLimitObservationAtLocked(agent, account string, resetAt time.Time, evidence string, wasParked bool, authoritative bool, sameWallEvidence string, sightedAt time.Time) {
	if agent == "" || account == "" {
		return
	}
	sighted := sightedAt.UTC()
	if sighted.IsZero() {
		// A caller with no instant is observing the wall right now; stamping
		// zero would write an observation that orders against no retraction at
		// all — the same wedge the explicit instant exists to close (#6880).
		sighted = instanceNow().UTC()
	}
	// One row per reset claim KIND, not per identity: merging a
	// known-reset wall into an unknown one — or the reverse — would
	// either drop the never-expires property or lower the instant a
	// retraction must postdate. Distinct claims are distinct facts and
	// keep their own rows; the strictest one still excludes (#6802
	// review). An authoritative sighting carrying the provider's instant
	// is the one crossing allowed: when no same-kind row exists, the
	// identity's single unknown-reset row IS the corrected wall's claim —
	// a park that could not read its banner filed it, and the record
	// proves the deadline that claim lacked (#6944 review). A timed row
	// existing alongside means the unknown one is a DIFFERENT wall's
	// claim and stands.
	target := -1
	zeroIdx := -1
	for idx := range i.accountLimitObservations {
		observation := &i.accountLimitObservations[idx]
		if observation.Agent != agent || observation.Account != account {
			continue
		}
		if observation.ResetAt.IsZero() == resetAt.IsZero() {
			target = idx
			break
		}
		if observation.ResetAt.IsZero() {
			zeroIdx = idx
		}
	}
	if target < 0 && authoritative && !resetAt.IsZero() {
		target = zeroIdx
	}
	if target >= 0 {
		observation := &i.accountLimitObservations[target]
		// resetAt arrives already resolved: the caller's
		// originalLimitResetLocked has judged whether the sighting is the
		// recorded wall's re-read or a genuinely new fact, so the merge
		// keeps only the safer bound — a repeated phrase that resolved
		// to the recorded instant merges in place, and a new wall that
		// re-uses a phrase (the next day's identical "reset at 3pm")
		// still tightens the row to its own instant (#6959).
		retained := RetainedAccountLimitReset(observation.ResetAt, resetAt)
		if authoritative && (observation.ResetAt.IsZero() ||
			(sameWallEvidence != "" && observation.Evidence == sameWallEvidence)) {
			// The record's instant corrects a bound in either direction
			// ONLY when the row is provably the sighting's own wall: an
			// adopted zero-kind row filed no instant to lower, and a timed
			// row carrying the parked wall's own clause — while the
			// sighting re-reads that wall's banner — is that wall's claim.
			// The row's stored clause, not the sighting's, is the match:
			// a wider capture legitimately rewrites the clause text for
			// the same wall. A timed row carrying a DIFFERENT clause is a
			// distinct wall, and lowering it to the record's earlier
			// instant would release the account while that wall still
			// stands (#6975 review) — the stricter bound stands instead.
			retained = resetAt
		}
		if wasParked && observation.ResetAt.Equal(retained) &&
			!i.reSightCrossesSendAttemptLocked(observation.ObservedAt, sighted) {
			return
		}
		if !retained.Equal(observation.ResetAt) {
			observation.ResetAt = retained
			// The phrase follows the reset it produced: a claim that
			// tightened the boundary names the phrase that carried it.
			observation.Evidence = evidence
		}
		// The row's newest sighting never rewinds: the instant is when the
		// row last saw this claim's wall, and a copy stamped with an older
		// source instant — or a lagging detection clock — must not move it
		// back below a sighting the row genuinely made, or a proof dated in
		// the gap would retract a wall S saw AFTER the proof (#6880 review).
		if sighted.After(observation.ObservedAt) {
			observation.ObservedAt = sighted
		}
		i.accountLimitObsUnpersisted = true
		i.touchLocked()
		return
	}
	i.accountLimitObservations = append(i.accountLimitObservations, AccountLimitObservationData{
		Agent: agent, Account: account, ResetAt: resetAt, Evidence: evidence, ObservedAt: sighted,
	})
	i.accountLimitObsUnpersisted = true
	i.touchLocked()
}

// reSightCrossesSendAttemptLocked reports whether a parked re-sight of an
// unchanged claim still earns its stamp: the pending swap's notice send is the
// recency boundary pendingSwapIncomingWallLocked orders wall freshness against
// (#6921), so the FIRST sighting landing after the attempt instant is a new
// fact — the wall genuinely re-observed after the send — while later re-sights
// of the unchanged claim add nothing and stay the no-op #6802 keeps. A row
// with no pending send attempt has no boundary to cross. Caller holds i.mu.
func (i *Instance) reSightCrossesSendAttemptLocked(stored, sighted time.Time) bool {
	pending := i.pendingAccountSwap
	if pending == nil || pending.MissionDeliveryAttemptAt.IsZero() ||
		!sighted.After(pending.MissionDeliveryAttemptAt) {
		return false
	}
	return !stored.After(pending.MissionDeliveryAttemptAt)
}

// RetainedAccountLimitReset merges two negative quota observations without
// making the account eligible sooner than either observation permits. An
// unknown reset dominates because af has no expiry fact; otherwise the later
// reset is the safer boundary.
func RetainedAccountLimitReset(prior, observed time.Time) time.Time {
	if prior.IsZero() || observed.IsZero() {
		return time.Time{}
	}
	if observed.After(prior) {
		return observed
	}
	return prior
}

// AccountLimitObservations returns durable named-identity quota evidence. It is
// independent of the session's current liveness: a successful replacement
// clears the live wall but cannot make the exhausted account eligible again.
// The daemon owns expiry policy because it also owns the reset grace window.
func (i *Instance) AccountLimitObservations() []AccountLimitObservationData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]AccountLimitObservationData(nil), i.accountLimitObservations...)
}

// AccountLimitObservationsUnpersisted reports whether the durable observation
// list changed in memory without a row-write carrying it yet (#6802 review).
func (i *Instance) AccountLimitObservationsUnpersisted() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.accountLimitObsUnpersisted
}

// MarkAccountLimitObservationsPersisted records that a whole-row write
// carrying written has landed. It clears the pending flag only when the
// observation list written is the one now held: a re-sighting that landed
// after the written snapshot stays pending and is written on the next tick.
func (i *Instance) MarkAccountLimitObservationsPersisted(written []AccountLimitObservationData) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if slices.Equal(written, i.accountLimitObservations) {
		i.accountLimitObsUnpersisted = false
	}
}
