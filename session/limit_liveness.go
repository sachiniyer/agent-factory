package session

import (
	"fmt"
	"slices"
	"strings"
	"time"

	xansi "github.com/charmbracelet/x/ansi"
)

// The usage-limit half of the daemon-owned liveness axis (#1146): parking a
// session on a provider quota wall, the durable named-identity observation
// ledger that outlives the parked state, and the resume-fence bookkeeping
// for the loop that un-parks it (#2997). The axis itself lives in
// liveness.go; this file is everything the wall needs beyond it.

// LimitBanner is the detector's anchor for WHICH banner produced a usage-limit
// read (#6823): where the last banner match sat inside the captured pane and
// how many matches the capture held. The claim fields (resetAt, evidence) say
// what the wall advertised; the anchor says which banner advertised it — the
// signal that lets a parked session tell a clipped re-read of its recorded
// banner from a banner printed after it, a distinction the claim fields alone
// cannot make: both degrade to (zero, "") identically.
type LimitBanner struct {
	// Prefix is the captured pane's bytes before the last banner match — the
	// match's absolute position inside that capture.
	Prefix string
	// Count is the number of banner matches the capture held. Zero means the
	// read carried no anchor (a path that observed no pane, like a legacy or
	// mirrored park), in which case nothing about position can be proven.
	Count int
}

// RecordedLimitWallExpired reports whether the sighted banner is the row's own
// recorded usage-limit wall whose claimed reset has already passed — residue
// left in the capture, not a wall the send must refuse (#6959 review). The
// readiness wait consults it before honoring a LimitReachedError: a parked
// row at reset+grace sends past the banner it already parked on, and only a
// banner the row never recorded is a live wall.
//
// freshRuntime is the caller's knowledge that the wait is running against a
// pane it just launched: such a pane can never carry the recorded emission,
// so the answer is always "not residue" — a lookalike banner there is a new
// wall, and a PID pin of 0 must not stand in for "same runtime" (#6972).
func (i *Instance) RecordedLimitWallExpired(prefix string, count int, evidence string, freshRuntime bool) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.limitWallResidueLocked(
		LimitBanner{Prefix: prefix, Count: count}, evidence, instanceNow(), freshRuntime)
}

// SetLimitReached marks the instance blocked on a usage-limit wall (#1146): it
// sets the LiveLimitReached liveness and stores the parsed reset time (zero
// when the banner carried none) for the sidebar badge and the auto-resume
// scheduler (daemon/limitresume.go). There is no legacy Status value for
// SetStatus to decompose onto, so
// the daemon single-writer (#960) sets the liveness axis directly here. Skips a
// row with any operation in flight so it never clobbers that operation's fence.
//
// Callers that parsed a reset PHRASE (the send-path park via
// task.LimitReachedError) should use SetLimitReachedAtEvidence so the phrase
// accompanies its reset — without it the daemon's first poll fails the
// same-phrase dedupe and re-parses against a later clock (#6765).
func (i *Instance) SetLimitReached(resetAt time.Time) {
	i.SetLimitReachedAtEvidence(resetAt, "", LimitBanner{}, false)
}

// SetLimitReachedAtEvidence is SetLimitReached for a park that parsed a reset
// phrase: it stores both the reset time and the evidence (the reset phrase the
// detector consumed) so the daemon's first poll over the still-displayed banner
// recognizes the re-read as the same observation and keeps the first parse's
// reset (#6765). The send-path park's parse IS the first parse of the banner,
// so its phrase must be recorded rather than cleared — otherwise a time-only
// phrase re-parsed after the stated clock time resolves to the next day and the
// drift becomes durable once the poll adopts the phrase.
//
// banner is the detector's anchor for the sighting (#6823) — the pane bytes
// before the last banner match and the match count — recorded beside the claim
// so a later clipped re-read can be told apart from a banner printed after it.
//
// freshRuntime is the caller's knowledge that the sighting was read on a pane
// it just launched: that pane can never carry a previously recorded emission,
// so a lookalike banner is a new wall, never residue (#6972). Callers that
// did not launch the pane — the poll, send-path re-detections — pass false.
func (i *Instance) SetLimitReachedAtEvidence(resetAt time.Time, evidence string, banner LimitBanner, freshRuntime bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setLimitReachedLocked(resetAt, evidence, banner, freshRuntime)
}

// SetLimitReachedAtEpoch is SetLimitReached for a decision derived from an
// OBSERVATION — the daemon poll's usage-limit detection over captured pane
// content (#2135). It applies the block only while the instance's state epoch is
// still the one the observation was captured at; if a newer authoritative
// transition has landed since (a resume's ClearLimitReached above all, but equally
// a kill or an archive) the decision is known-stale and is dropped. Reports
// whether it applied.
//
// The check and the write are one critical section under i.mu, which is the whole
// point: an epoch read followed by a separate SetLimitReached would leave the same
// window it closes.
//
// evidence is the reset phrase the detector consumed to produce resetAt. A poll
// re-reads the same phrase whenever the banner is unchanged, and that re-read is
// not a new observation (#6765): the time-only and relative forms re-resolve to
// a LATER instant on every parse, so applying the re-read would move the reset
// — and postpone the auto-resume — forever. The first parse of a phrase is the
// one nearest the wall the provider printed, so an already-parked instance keeps
// it. A different phrase (or one seen while not yet parked) is new evidence and
// applies.
//
// The one shape the claim cannot self-describe is a parked re-read of
// (resetAt=zero, evidence=""): a capture of the recorded banner after its reset
// clause scrolled out of the visible pane and a banner the wall printed
// afterward BOTH report it (#6823). applyDegradedResightLocked decides from the
// banner anchor and the current identity what can be proven — and, when nothing
// can, holds the wall at "unknown reset" without dirtying the durable record
// rather than guessing.
func (i *Instance) SetLimitReachedAtEpoch(resetAt time.Time, evidence string, banner LimitBanner, epoch uint64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != epoch {
		return false
	}
	if i.liveness == LiveLimitReached {
		if resetAt.IsZero() && evidence == "" {
			return i.applyDegradedResightLocked(banner)
		}
		if evidence == i.limitResetEvidence &&
			i.limitWallRuntimeCurrentLocked(i.limitWallRuntimePID, i.limitWallRuntimeStartID, i.limitWallRuntimeGen) &&
			!(i.limitBannerCount > 0 && banner.Count > i.limitBannerCount) {
			// Already parked on the SAME wall claim — a re-read of an
			// unchanged banner is not a new observation, whatever shape its
			// reset took: the stored reset stays (the first parse is nearest
			// the real window) and no durable fact moved, so nothing below may
			// run. The one provable exception is count GROWTH: more banner
			// matches than the recorded sighting counted means a banner
			// printed below the recorded one — a new wall re-using the
			// phrase, and keeping the claim here would pin its send
			// tombstone onto a wall whose prompt never went out (#6972,
			// Codex). A moved anchor at an unchanged count is ambiguous —
			// the capture window may simply have slid — so position alone
			// does not veto. The runtime pin is checked for the same reason:
			// a replacement runtime's identical banner is a new emission and
			// must not keep the dead runtime's tombstone.
			return true
		}
	}
	if !i.setLimitReachedLocked(resetAt, evidence, banner, false) {
		return false
	}
	return true
}

// applyDegradedResightLocked classifies a (resetAt=zero, evidence="") hit over
// an already-parked session (#6823). The claim alone is ambiguous — a clipped
// re-read of the recorded banner and a banner the wall printed afterward
// degrade to it identically — so the decision falls to the signals around the
// read:
//
//   - CURRENT IDENTITY: the recorded wall is attributed to limitAgent/
//     limitAccount; a banner still on the pane after the identity moved (an
//     account swap committed, the program changed) is not that wall re-read —
//     it is the current identity's wall, recorded and attributed as one.
//   - BANNER POSITION: the recorded banner is still the last match when the
//     capture ends at the same place relative to it — the pane lines ABOVE the
//     match's own row are byte-identical or suffix-related (the capture window
//     slid while the banner sat still; the match row's own lead marker is not
//     positional evidence, see sameLimitBannerPosition). That proves a clipped
//     re-read: detection already stands, so nothing else may run — the first
//     parse's reset, its phrase, and the recorded observations all stay, and
//     nothing is dirtied.
//   - BANNER ORDER/COUNT: the recorded reset clause still in view in the new
//     prefix means the match that produced this hit sits BELOW it — a banner
//     printed after the recorded wall. More matches than the recorded sighting
//     counted proves the same. Either is a genuinely new wall and is recorded.
//
// When none of that distinguishes the read — a moved position with no visible
// clause, an unchanged count — the poll holds the wall but claims only what the
// capture verifiably shows: a wall with NO known reset. The stored instant is
// dropped rather than kept: keeping it could schedule a resume — or release
// the live parked wall's account exclusion — at a deadline the wall no longer
// verifiably owns, the exact fail-open the issue names. The recorded identity
// stays (re-attributing on a guess could blame a healthy account), and nothing
// durable is minted: the recorded observations keep their own bounds and no
// never-expiring untimed row is appended on a guess.
func (i *Instance) applyDegradedResightLocked(banner LimitBanner) bool {
	// A sighting on a runtime that replaced the parked wall's emitter is a
	// new emission, never a clipped re-read — the pane that displayed the
	// recorded banner is gone (#6972, Codex).
	if agent := i.currentAgentNameLocked(); agent != i.limitAgent || i.Account != i.limitAccount ||
		!i.limitWallRuntimeCurrentLocked(i.limitWallRuntimePID, i.limitWallRuntimeStartID, i.limitWallRuntimeGen) {
		return i.setLimitReachedLocked(time.Time{}, "", banner, false)
	}
	if sameLimitBannerPosition(i.limitBannerPrefix, banner.Prefix) {
		return true
	}
	if i.limitResetEvidence != "" && strings.Contains(banner.Prefix, i.limitResetEvidence) {
		return i.setLimitReachedLocked(time.Time{}, "", banner, false)
	}
	if i.limitBannerCount > 0 && banner.Count > i.limitBannerCount {
		return i.setLimitReachedLocked(time.Time{}, "", banner, false)
	}
	if i.inFlightOp != OpNone {
		return false
	}
	lv, op, prevReset := i.lifecycleStateLocked()
	i.limitResetAt = time.Time{}
	i.limitResetEvidence = ""
	i.noteLimitWallClaimLocked(i.limitAgent, time.Time{}, "",
		LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount})
	i.noteStateChangeLocked(lv, op, prevReset)
	return true
}

// sameLimitBannerPosition reports whether two banner prefixes describe the same
// last-match position — the capture ended at the same place relative to the
// banner (the window may have slid while the banner sat still). The comparison
// uses only the full lines ABOVE the match's own row: the row's trailing bytes
// are the agent's banner lead marker ("⎿  ", "■ "), identical for every
// anchored match of that agent, so they carry no positional information — a
// recorded prefix that IS just the marker would suffix-match any later
// banner's gutter and falsely prove a re-read of a different wall. The shared
// body must also carry distinguishing CONTENT: a shorter body that is only
// whitespace or ANSI styling (a blank line before the banner leaves
// storedBody == "\n") suffix-matches every longer capture and proves nothing —
// a banner whose capture holds no real text above its row has no position.
func sameLimitBannerPosition(stored, current string) bool {
	storedBody := stored[:strings.LastIndexByte(stored, '\n')+1]
	currentBody := current[:strings.LastIndexByte(current, '\n')+1]
	shorter, longer := storedBody, currentBody
	if len(longer) < len(shorter) {
		shorter, longer = longer, shorter
	}
	if strings.TrimSpace(xansi.Strip(shorter)) == "" {
		return false
	}
	return strings.HasSuffix(longer, shorter)
}

// sameLiveLimitBannerLocked reports whether a sighting's banner anchor is the
// SAME banner the live wall row already carries — the unchanged banner a
// re-read of the same pane returns — versus a newly printed wall. It mirrors
// applyDegradedResightLocked's two discriminating reads: the last-match
// position must coincide (the capture window may slide while the banner sits
// still), and the visible match count must not have grown past the recorded
// one — more matches than the recorded sighting counted means a banner
// printed BELOW the recorded wall: a genuinely new wall. Caller holds i.mu.
func (i *Instance) sameLiveLimitBannerLocked(banner LimitBanner) bool {
	return sameLimitBannerPosition(i.limitBannerPrefix, banner.Prefix) &&
		!(i.limitBannerCount > 0 && banner.Count > i.limitBannerCount)
}

// setLimitReachedLocked is the shared body: mark the instance limit-blocked and
// record its reset time, evidence, and banner anchor, skipping a row with any
// operation in flight so it never clobbers that operation's fence. Reports
// whether it applied. Caller holds i.mu.
//
// evidence is the reset phrase the detector consumed to produce resetAt. A park
// that parsed a phrase (the send-path park via task.LimitReachedError) records
// it so the daemon's first poll recognizes the re-read as the same observation
// and keeps the first parse's reset (#6765). A park that resolved a reset
// without a phrase passes "" — the slot is cleared rather than left describing
// a reset it did not produce. banner carries the sighting's anchor (#6823);
// a caller that observed no pane passes the zero LimitBanner.
func (i *Instance) setLimitReachedLocked(resetAt time.Time, evidence string, banner LimitBanner, freshRuntime bool) bool {
	if i.inFlightOp != OpNone {
		return false
	}
	agent := i.currentAgentNameLocked()
	if i.liveness != LiveLimitReached &&
		i.limitWallResidueLocked(banner, evidence, instanceNow(), freshRuntime) {
		// The pane still shows the banner the row already parked on — and its
		// claimed reset has already passed. That sighting is residue, never a
		// wall: a park here re-arms the resume sweep's send on an expired
		// claim, replaying the prompt on every idle poll (#6959 review). Only
		// a banner the row never recorded parks.
		return false
	}
	resetAt, evidence, promptSent := i.originalLimitResetLocked(agent, resetAt, evidence, banner,
		i.liveness == LiveLimitReached, false, time.Time{}, time.Time{})
	lv, op, prevReset := i.lifecycleStateLocked()
	i.assignLivenessLocked(LiveLimitReached)
	i.limitResetAt = resetAt
	i.limitResetEvidence = evidence
	i.limitBannerPrefix = banner.Prefix
	i.limitBannerCount = banner.Count
	i.limitPromptSent = promptSent
	// The wall was sighted on the runtime currently up: pin the claim to it
	// so a sighting judged after a replacement cannot pass as its re-read —
	// the generation pin is the read that still binds when no concrete
	// process can be resolved (#6972).
	i.limitWallRuntimePID = i.runtimePID
	i.limitWallRuntimeStartID = i.runtimeStartID
	i.limitWallRuntimeGen = i.agentRuntimeGeneration
	i.attributeLimitIdentityAsLocked(agent)
	i.noteLimitWallClaimLocked(agent, resetAt, evidence, banner)
	i.recordAccountLimitObservationAtLocked(agent, i.Account, resetAt, evidence, lv == LiveLimitReached, instanceNow().UTC())
	i.noteStateChangeLocked(lv, op, prevReset)
	return true
}

// originalLimitResetLocked resolves which reset claim a limit sighting's park
// should record. A clock-time ("reset at 3pm") or relative ("try again in N")
// phrase parses against the READING clock, so a retry that re-parses the
// banner still on the pane mints a later instant on every pass — the re-read
// rolls the wall forward forever and the session never resumes (#6959). The
// FIRST parse is the one nearest the wall the provider printed, so a sighting
// the row already recorded keeps the recorded claim and the re-parse adds
// nothing:
//
//   - parked and the sighting's phrase IS the live claim's — the same wall
//     re-read the #6765 dedupe names. A sighting carrying a resolved reset
//     but NO phrase is new evidence, never a re-read: re-parsing a
//     phrase-bearing banner always yields the phrase again, so a phraseless
//     reset came from a different evidence path and applies.
//   - the shadow the last park left behind — the cleared row's memory of its
//     own wall. Identity AND anchor must match; then a repeated phrase is the
//     recorded wall's re-parse, and a clause-free read at the recorded
//     position is that wall clipped — either keeps the first parse. A
//     re-worded clause, a resolved-reset-without-phrase, or a banner printed
//     below is a different fact and falls through.
//   - the durable observation ledger — the named identity's recorded wall —
//     but only for a sighting that carries no pane anchor of its own (the
//     committed-copy shape). A phrase match alone cannot tell a clipped
//     re-read from a NEW banner that happens to re-use the phrase (the next
//     day's identical "reset at 3pm"), so an anchored sighting is judged by
//     the live and shadow reads above — positional evidence a copied,
//     pane-free sighting simply does not have.
//
// parked reports the row sat at its live wall when the sighting was read.
// freshEvidence is the caller's knowledge that the sighting is new wall
// evidence, never the recorded wall's re-read — a fresh replacement
// runtime's emission or a different banner on the same pane, the shapes the
// send-path pair's wasParked classification already refuses (#6880). No
// recorded claim may stand in for it, not even a coincidentally identical
// phrase at a coincidentally identical anchor: on a fresh pane those prove
// only that the new emission resembles the old, and its own parse is the
// wall's current claim.
//
// sightedAt is the sighting's event time when the park COPIES a wall fact —
// the committed-admission twin stamps the source's own observation instant —
// and zero for a sighting this row read itself. It orders the ledger arm's
// phrase match: a copy whose source observed the wall AFTER this row's own
// record of that claim is fresher evidence, so the source's resolved
// deadline stands rather than the row's older one; a copy carrying nothing
// newer than the record borrows the recorded claim. ordSightedAt is the same
// source instant, but only when the sighting carries external provenance —
// the committed twin passes its source instant, while every self-read
// sighting passes zero, since the row's own re-read clock is not new
// evidence to order its claim against (#6959 review). It orders the live
// arm the same way the ledger arm orders the durable row: a parked row must
// not let a sibling's newer same-phrase observation be vetoed by its own
// older claim.
//
// A sighting none of those recognize keeps its own parse: a genuinely new
// limit fact parks with its own reset.
//
// The third return is the resolved claim's send tombstone: a park that borrows
// the live or shadow claim inherits whether this wall's resume prompt already
// landed, so the scheduler can tell "the row is parked waiting for the wall"
// from "the row already sent past this wall" (#6972 item 4).
func (i *Instance) originalLimitResetLocked(agent string, resetAt time.Time, evidence string, banner LimitBanner, parked, freshEvidence bool, sightedAt, ordSightedAt time.Time) (time.Time, string, bool) {
	if freshEvidence {
		return resetAt, evidence, false
	}
	if parked && i.limitAgent == agent && i.limitAccount == i.Account &&
		evidence != "" && evidence == i.limitResetEvidence &&
		i.limitWallRuntimeCurrentLocked(i.limitWallRuntimePID, i.limitWallRuntimeStartID, i.limitWallRuntimeGen) &&
		!(i.limitBannerCount > 0 && banner.Count > i.limitBannerCount) &&
		!ordSightedAt.After(i.accountLimitObservedAtLocked(agent, resetAt)) {
		return i.limitResetAt, i.limitResetEvidence, i.limitPromptSent
	}
	if w := i.lastLimitWall; w.agent == agent && w.account == i.Account &&
		i.limitWallRuntimeCurrentLocked(w.runtimePID, w.runtimeStartID, w.runtimeGen) &&
		limitWallAnchorAdmits(w.banner, w.evidence, banner, evidence) &&
		!(w.banner.Count > 0 && banner.Count > w.banner.Count) {
		switch {
		case resetAt.IsZero() && evidence == "":
			// The recorded wall's re-read with its clause clipped: same
			// position, nothing parsed. The recorded claim is still the best
			// evidence — and keeping it is what stops the re-park from
			// dropping to "unknown reset" when the banner merely scrolled.
			return w.resetAt, w.evidence, w.promptSent
		case evidence != "" && evidence == w.evidence:
			// The phrase the recorded wall advertised re-reads identically:
			// this IS that banner's re-parse, and the first parse stands.
			return w.resetAt, w.evidence, w.promptSent
		case evidence != "" && w.evidence == "" && !w.resetAt.IsZero():
			// The recorded wall's clause was never captured (a mirrored or
			// resolved park); the same-position read now shows one. The
			// recorded instant still stands — it is nearer the wall print —
			// and the now-visible phrase is adopted for future re-reads.
			return w.resetAt, evidence, w.promptSent
		}
	}
	if banner == (LimitBanner{}) && evidence != "" {
		for idx := range i.accountLimitObservations {
			observation := &i.accountLimitObservations[idx]
			if observation.Agent == agent && observation.Account == i.Account &&
				observation.Evidence == evidence && !sightedAt.After(observation.ObservedAt) {
				return observation.ResetAt, observation.Evidence, false
			}
		}
	}
	return resetAt, evidence, false
}

// accountLimitObservedAtLocked is the sighting instant the durable ledger
// carries for this identity's same-reset-kind claim — the record a copied
// sighting must not postdate to borrow the recorded claim: a copy whose
// source re-observed the wall AFTER the row's own record is fresher evidence
// and stands on its own (#6959 review). Zero when no row matches. Caller
// holds i.mu.
func (i *Instance) accountLimitObservedAtLocked(agent string, resetAt time.Time) time.Time {
	for idx := range i.accountLimitObservations {
		observation := &i.accountLimitObservations[idx]
		if observation.Agent == agent && observation.Account == i.Account &&
			observation.ResetAt.IsZero() == resetAt.IsZero() {
			return observation.ObservedAt
		}
	}
	return time.Time{}
}

// attributeLimitIdentityLocked publishes which runtime's identity produced the
// recorded limit wall — its agent namespace and account label — so the wall is
// never observable without its attribution (account-swap admission reads the
// pair). Each field is written, and the row touched, only when it changes.
// Caller holds i.mu.
func (i *Instance) attributeLimitIdentityLocked() {
	i.attributeLimitIdentityAsLocked(i.currentAgentNameLocked())
}

// attributeLimitIdentityAsLocked is attributeLimitIdentityLocked with the
// agent namespace named by the caller rather than read off the running pane:
// the pre-spawn park files a wall for an identity the pane does not yet run,
// where currentAgentNameLocked would attribute it to the outgoing agent
// (#6915 review). Caller holds i.mu.
func (i *Instance) attributeLimitIdentityAsLocked(agent string) {
	if i.limitAgent != agent {
		i.limitAgent = agent
		i.touchLocked()
	}
	if i.limitAccount != i.Account {
		i.limitAccount = i.Account
		i.touchLocked()
	}
}

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
	i.recordAccountLimitObservationAtLocked(agent, account, resetAt, evidence, wasParked, instanceNow().UTC())
}

// recordAccountLimitObservationAtLocked is recordAccountLimitObservationLocked
// with the sighting instant supplied by the caller: a park that copies a wall
// it did not itself observe stamps the SOURCE's event time, never a later
// instant — a copy that postdates its source observation outlives every
// retraction that was already on its way between the source's sighting and the
// park (#6880). Caller holds i.mu.
func (i *Instance) recordAccountLimitObservationAtLocked(agent, account string, resetAt time.Time, evidence string, wasParked bool, sightedAt time.Time) {
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
	for idx := range i.accountLimitObservations {
		observation := &i.accountLimitObservations[idx]
		// One row per reset claim KIND, not per identity: merging a
		// known-reset wall into an unknown one — or the reverse — would
		// either drop the never-expires property or lower the instant a
		// retraction must postdate. Distinct claims are distinct facts and
		// keep their own rows; the strictest one still excludes (#6802
		// review).
		if observation.Agent == agent && observation.Account == account &&
			observation.ResetAt.IsZero() == resetAt.IsZero() {
			// resetAt arrives already resolved: the caller's
			// originalLimitResetLocked has judged whether the sighting is the
			// recorded wall's re-read or a genuinely new fact, so the merge
			// keeps only the safer bound — a repeated phrase that resolved
			// to the recorded instant merges in place, and a new wall that
			// re-uses a phrase (the next day's identical "reset at 3pm")
			// still tightens the row to its own instant (#6959).
			retained := RetainedAccountLimitReset(observation.ResetAt, resetAt)
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

// SetLimitResetAt records only the display-only reset time (#1146), leaving both
// axes untouched. The read-only TUI reconcile uses it to mirror the daemon's
// parsed reset time after it has already applied LiveLimitReached on the liveness
// axis (Phase 1d applies liveness UNCONDITIONALLY via SetLiveness): the reset
// time rides the liveness as pure display metadata, so it is set on its own
// rather than through SetLimitReached, which would re-drive the liveness axis and
// carry SetLimitReached's transient-op guard into a path that must be
// unconditional.
func (i *Instance) SetLimitResetAt(resetAt time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, prevReset := i.lifecycleStateLocked()
	i.limitResetAt = resetAt
	// A mirrored reset carries no reset phrase; the slot must not keep
	// describing a parse it did not produce (#6765).
	i.limitResetEvidence = ""
	i.noteLimitWallClaimLocked(i.limitAgent, resetAt, "",
		LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount})
	i.noteStateChangeLocked(lv, op, prevReset)
}

// ClearLimitReached moves a limit-blocked instance back to LiveRunning so the
// daemon poll re-resolves its real state on the next tick and the [limit] badge
// clears (#1146). A no-op when the instance is not limit-blocked, so the resume
// action (and the auto-resume scheduler) can call it unconditionally.
// ReparkLimitUnderResumeFence restores a limit window while the caller holds the
// resume fence. It is the transaction-owned twin of SetLimitReached, in the same
// shape as RecordHandoffSwap is to SwapAgentProgram: the plain setter refuses while
// ANY op is in flight, which is right for every other writer and wrong for the one
// that owns the operation.
//
// The resume needs it because it re-parks BEFORE delivering its prompt (#2997). With
// the fence held across that stretch — which is what keeps the poll from settling the
// fresh runtime Ready and ending the task run — the plain setter would no-op and this
// episode's reset time would be lost, leaving the auto-resume scheduler nothing to
// schedule off. Silently, too: it reports the refusal only through a bool that path
// had no reason to read.
//
// Refuses unless OpRespawning is actually held, so it cannot become a back door
// around the guard it deliberately steps past.
func (i *Instance) ReparkLimitUnderResumeFence(resetAt time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return fmt.Errorf("re-parking the limit for %q requires the resume fence (in-flight op is %s)",
			i.Title, opLabel(i.inFlightOp))
	}
	lv, op, prevReset := i.lifecycleStateLocked()
	// Direct write, not assignLivenessLocked: this is the transaction owner
	// restoring its own parked disposition after a refused respawn, not a pane
	// observation. While a rebuild teardown is owed, the coerce-to-Lost guard
	// exists to keep LYING observations from reviving the row — but it must not
	// strand the owner: a pending row coerced to LiveLost is refused by the
	// ordinary lost-restore gate when a PendingAccountSwap rides it, and the
	// limit scheduler admits only a row still parked at LiveLimitReached
	// (#5371 review). The teardown obligation stays armed on its own flag and
	// re-runs inside the retry's respawn regardless of what the row reads.
	i.liveness = LiveLimitReached
	// The reset is the one the owner read off this row when it began the
	// attempt — the stored evidence still describes it, so the slot stays.
	i.limitResetAt = resetAt
	// The repark restores the wall's own claim — including the runtime it
	// was sighted on, its anchor, and whether its send already landed, all
	// of which only the shadow still holds after the respawn's clear — so a
	// coincidentally matching banner on the replacement runtime cannot pass
	// as its re-read (#6959 review), and a wall the row already sent past
	// keeps its tombstone (#6972).
	i.limitWallRuntimePID = i.lastLimitWall.runtimePID
	i.limitWallRuntimeStartID = i.lastLimitWall.runtimeStartID
	i.limitWallRuntimeGen = i.lastLimitWall.runtimeGen
	i.limitPromptSent = i.lastLimitWall.promptSent
	i.limitBannerPrefix = i.lastLimitWall.banner.Prefix
	i.limitBannerCount = i.lastLimitWall.banner.Count
	i.noteLimitWallClaimLocked(i.limitAgent, resetAt, i.limitResetEvidence,
		LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount})
	i.noteStateChangeLocked(lv, op, prevReset)
	return nil
}

// RestoreLivenessUnderResumeFence restores the liveness a committed respawn
// attempt found on entry after that attempt fails — the transaction-owned
// twin of Transition(ObserveLiveness) for the same reason
// ReparkLimitUnderResumeFence is the twin of SetLimitReached: it is owner
// bookkeeping about the attempt's own precondition, not a pane observation,
// so the rebuild-teardown coerce-to-Lost guard in assignLivenessLocked must
// not apply to it. Without it a committed manual account swap whose respawn
// armed the teardown obligation is parked at LiveLost — refused by the
// ordinary lost-restore gate on PendingAccountSwap, and ineligible for the
// scheduled-swap retry that only admits LiveRunning/LiveReady — stranded with
// no owning loop (#5371 review).
//
// Refuses unless OpRespawning is actually held, matching
// ReparkLimitUnderResumeFence, so it cannot become a back door around the
// observation guard it deliberately steps past.
func (i *Instance) RestoreLivenessUnderResumeFence(l Liveness) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return fmt.Errorf("restoring liveness for %q requires the resume fence (in-flight op is %s)",
			i.Title, opLabel(i.inFlightOp))
	}
	lv, op, prevReset := i.lifecycleStateLocked()
	i.liveness = l
	i.noteStateChangeLocked(lv, op, prevReset)
	return nil
}

func (i *Instance) ClearLimitReached() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.clearLimitReachedLocked(false)
}

// ClearLimitReachedSent is ClearLimitReached for the clear that follows a
// LANDED resume send: it marks the retired wall claim's tombstone so a
// re-park of the same emission — the unchanged banner still on the pane —
// keeps the fact that this wall's prompt already went out, and the scheduler
// never re-sends for it (#6972 item 4).
func (i *Instance) ClearLimitReachedSent() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.clearLimitReachedLocked(true)
}

func (i *Instance) clearLimitReachedLocked(sent bool) {
	if i.liveness != LiveLimitReached {
		return
	}
	if sent {
		i.lastLimitWall.promptSent = true
	}
	// The row genuinely moved past this wall: the mirror it parked becomes a
	// retired claim — the durable distinction a restored row needs to keep
	// calling the unchanged banner residue (#6972).
	i.lastLimitWall.retired = true
	i.lastLimitWall.retiredKnown = true
	i.limitWallUnpersisted = true
	lv, op, prevReset := i.lifecycleStateLocked()
	i.assignLivenessLocked(LiveRunning)
	i.limitResetAt = time.Time{}
	i.limitResetEvidence = ""
	i.limitAgent = ""
	i.limitAccount = ""
	i.limitBannerPrefix = ""
	i.limitBannerCount = 0
	i.limitWallRuntimePID = 0
	i.limitWallRuntimeStartID = 0
	i.limitWallRuntimeGen = 0
	i.limitPromptSent = false
	// The epoch bump here is what a racing poll checks: it is the resume's
	// completion point, so any limit re-detection made from content captured before
	// it is stale by definition and must not land (#2135).
	i.noteStateChangeLocked(lv, op, prevReset)
}

// LimitReached reports whether the instance is blocked on a usage limit (#1146).
// Every render/serialize site keys its [limit] badge off this rather than the
// composed Status, which has no limit value (LiveLimitReached composes to Ready).
func (i *Instance) LimitReached() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveLimitReached
}

// LimitResetAt returns the parsed usage-limit reset time and whether one is
// known (#1146). It reports (zero, false) when the session is not limit-blocked
// or the banner carried no parseable reset time, so a stale reset value can
// never leak onto a recovered session.
func (i *Instance) LimitResetAt() (time.Time, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveLimitReached || i.limitResetAt.IsZero() {
		return time.Time{}, false
	}
	return i.limitResetAt, true
}

// LimitResetEvidence returns the reset phrase whose parse produced the stored
// reset (#6765), or "" when the session is not limit-blocked or the stored
// reset was written by a path that observed no phrase. It is gated the same
// way ToInstanceData serializes the row, so a pre-poll capture here compares
// cleanly against the snapshot the persist gate re-reads.
func (i *Instance) LimitResetEvidence() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveLimitReached {
		return ""
	}
	return i.limitResetEvidence
}

// LimitWallPromptSent reports whether the parked wall's resume prompt already
// landed — the tombstone ClearLimitReachedSent writes and a re-park of the
// same claim preserves. The scheduler skips a wall carrying it: the send it
// would fire already went out, so one more would be a replay (#6972 item 4).
func (i *Instance) LimitWallPromptSent() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveLimitReached && i.limitPromptSent
}

// LimitBannerAnchor returns the banner anchor recorded with the current wall
// (#6823), zero-valued off the limit wall. The handoff re-park reads it to
// carry the recorded sighting's anchor onto the row it restores.
func (i *Instance) LimitBannerAnchor() LimitBanner {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveLimitReached {
		return LimitBanner{}
	}
	return LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount}
}

// LimitAccount returns the account whose runtime produced the current limit
// observation. Empty is the ambient identity; ok is false off the limit wall.
func (i *Instance) LimitAccount() (account string, ok bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveLimitReached {
		return "", false
	}
	return i.limitAccount, true
}

// LimitIdentity returns the agent namespace and account label whose runtime
// produced the current wall. Account labels are meaningful only within their
// agent namespace; account is empty for the ambient identity.
func (i *Instance) LimitIdentity() (agent, account string, ok bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveLimitReached {
		return "", "", false
	}
	return i.limitAgent, i.limitAccount, true
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

// LimitWallUnpersisted reports whether the durable wall record — the parked
// claim's anchor and tombstone, or the retired shadow — changed in memory
// without a whole-row write carrying it yet (#6972, Codex). The poll's change
// gate diffs liveness/reset/evidence only, so without this flag a parked
// row's claim mutation under an unchanged liveness stays memory-only.
func (i *Instance) LimitWallUnpersisted() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.limitWallUnpersisted
}

// MarkLimitWallPersisted records that a whole-row write carrying written has
// landed. It clears the pending flag only when the shadow written is the one
// now held — a wall mutation that landed after the written snapshot stays
// pending and is written on the next tick. Both sides compare in restored
// form so the serialized prefix tail cannot defeat the match.
func (i *Instance) MarkLimitWallPersisted(written *LimitWallClaimData) {
	i.mu.Lock()
	defer i.mu.Unlock()
	current := limitWallClaimToData(i.lastLimitWall)
	if limitWallClaimFromData(written) == limitWallClaimFromData(&current) {
		i.limitWallUnpersisted = false
	}
}
