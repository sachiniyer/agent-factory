package session

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// limitWallClaim is one recorded usage-limit wall fact: the identity it was
// attributed to, the reset claim it advertised, and the banner anchor that
// sighted it. It is the shape the "last wall" shadow carries past a
// ClearLimitReached so a cleared row can still recognize a re-read of the
// banner it parked on (#6959).
type limitWallClaim struct {
	agent    string
	account  string
	resetAt  time.Time
	evidence string
	banner   LimitBanner
	// runtimePID and runtimeStartID pin the claim to the concrete pane-root
	// process the wall was sighted on — the same durable launch evidence
	// runtimePID/runtimeStartID record for the launch itself. A sighting
	// judged against a claim from a replaced process is a different
	// emission, never residue. Zero means the recording runtime could not
	// be pinned (a remote or adopted runtime): the check falls back to the
	// position/phrase reads alone.
	runtimePID     int
	runtimeStartID uint64
}

// limitWallClaimsLocked is the row's recorded wall knowledge: the live claim
// while it is parked, and the shadow the last park left behind once it is not.
// Both describe walls the row ITSELF recorded — the claims a residue check
// compares a sighting against. Caller holds i.mu.
func (i *Instance) limitWallClaimsLocked() [2]limitWallClaim {
	live := limitWallClaim{}
	if i.liveness == LiveLimitReached {
		live = limitWallClaim{
			agent: i.limitAgent, account: i.limitAccount,
			resetAt: i.limitResetAt, evidence: i.limitResetEvidence,
			banner:     LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount},
			runtimePID: i.limitWallRuntimePID, runtimeStartID: i.limitWallRuntimeStartID,
		}
	}
	return [2]limitWallClaim{live, i.lastLimitWall}
}

// limitWallResidueLocked reports whether a sighting is a re-read of the row's
// OWN recorded wall — its live claim while parked, or the shadow the last
// park left behind — whose claimed reset has already passed. Such a sighting
// is residue: the pane merely still displays the emission that carried the
// expired claim, and treating it as a wall re-parks the row onto a deadline
// that already fired — the resume loop's send → clear → re-park → send cycle
// that replays the prompt on every idle poll (#6959 review).
//
// Residue must be PROVEN, not inferred: the sighting must sit at the recorded
// banner's position without a grown match count (a banner printed below is a
// genuinely new wall), and carry no phrase contradicting the recorded claim.
// A claim that recorded no anchor still proves residue by phrase alone — the
// same fallback sendPathReSightingLocked uses, since a missing in-memory
// anchor is not evidence of a new banner. A claim whose reset is unknown or
// still pending keeps its wall: only an EXPIRED claim makes the emission
// residue. Caller holds i.mu.
func (i *Instance) limitWallResidueLocked(banner LimitBanner, evidence string, now time.Time) bool {
	agent := i.currentAgentNameLocked()
	for _, c := range i.limitWallClaimsLocked() {
		if c.agent != agent || c.account != i.Account {
			continue
		}
		// The residue reads below prove the pane still displays the emission
		// the claim recorded — impossible once the concrete process that
		// displayed it is gone: a re-launched or handoff-replaced runtime
		// showing the same phrase at the same position is a NEW emission
		// that happens to resemble the old one, whatever the anchor says
		// (the A→B→A case — #6959 review). A claim or row without a pinned
		// process cannot distinguish runtimes and stays on the reads below.
		if !i.limitWallRuntimeCurrentLocked(c.runtimePID, c.runtimeStartID) {
			continue
		}
		if c.banner.Count > 0 && banner.Count > c.banner.Count {
			// More banner matches than the recorded sighting counted: a
			// banner printed BELOW the recorded one — a genuinely new wall.
			// The count is positional evidence independent of the anchor,
			// so it rejects the anchorless phrase fallback too: a recorded
			// banner at byte zero legitimately carries an empty prefix, and
			// a second emission re-using its phrase would otherwise read as
			// residue (#6959 review).
			continue
		}
		anchored := sameLimitBannerPosition(c.banner.Prefix, banner.Prefix)
		if !anchored && !(c.banner.Prefix == "" && evidence != "" && evidence == c.evidence) {
			continue
		}
		if evidence != "" && c.evidence != "" && evidence != c.evidence {
			continue
		}
		if !c.resetAt.IsZero() && !c.resetAt.After(now) {
			return true
		}
	}
	return false
}

// limitWallRuntimeCurrentLocked reports whether a wall claim's recorded
// pane-root process can still be the process up now. Both pinned and
// different proves a replacement — the pane the claim describes is gone and
// no sighting can be that emission's re-read. Either side unpinned (a remote
// or adopted runtime, a claim recorded before the pin existed) cannot
// distinguish the processes and stays admissible. Caller holds i.mu.
func (i *Instance) limitWallRuntimeCurrentLocked(pid int, startID uint64) bool {
	if pid == 0 || i.runtimePID == 0 {
		return true
	}
	return pid == i.runtimePID && startID == i.runtimeStartID
}

// limitWallPrefixPersistMax bounds the anchor tail serialized beside a wall
// claim. The position compare consumes only the bytes immediately above the
// banner, so a few screenfuls back is all the stale-read decision needs; the
// full capture a sighting produced can be far larger than a record row should
// carry.
const limitWallPrefixPersistMax = 2048

// limitWallPrefixTail bounds a serialized banner anchor to its trailing bytes
// — the end nearest the banner, which is what the position compare reads. The
// cut lands on a rune boundary: a mid-rune slice survives JSON as U+FFFD, and
// the repaired bytes would never suffix-match the unchanged pane again — a
// restart would then read the recorded wall's own re-parse as fresh evidence.
func limitWallPrefixTail(prefix string) string {
	if len(prefix) <= limitWallPrefixPersistMax {
		return prefix
	}
	start := len(prefix) - limitWallPrefixPersistMax
	for start < len(prefix) && !utf8.RuneStart(prefix[start]) {
		start++
	}
	return prefix[start:]
}

// limitWallClaimToData and limitWallClaimFromData convert the "last wall"
// shadow between its in-memory and durable shapes.
func limitWallClaimToData(w limitWallClaim) LimitWallClaimData {
	if w.agent == "" && w.resetAt.IsZero() && w.evidence == "" &&
		w.banner.Prefix == "" && w.banner.Count == 0 {
		return LimitWallClaimData{}
	}
	return LimitWallClaimData{
		Agent: w.agent, Account: w.account, ResetAt: w.resetAt,
		Evidence:     w.evidence,
		BannerPrefix: limitWallPrefixTail(w.banner.Prefix), BannerCount: w.banner.Count,
		RuntimePID: w.runtimePID, RuntimeStartID: w.runtimeStartID,
	}
}

func limitWallClaimFromData(d *LimitWallClaimData) limitWallClaim {
	if d == nil {
		return limitWallClaim{}
	}
	return limitWallClaim{
		agent: d.Agent, account: d.Account, resetAt: d.ResetAt,
		evidence:   d.Evidence,
		banner:     LimitBanner{Prefix: d.BannerPrefix, Count: d.BannerCount},
		runtimePID: d.RuntimePID, runtimeStartID: d.RuntimeStartID,
	}
}

// limitClaimsIntoData writes the limit-claim fields into the serialized row:
// the live claim and its banner anchor while LiveLimitReached (the same gate
// as LimitResetAt/ToInstanceData), and the "last wall" shadow unconditionally
// — a cleared row's residue knowledge is exactly what a restart must keep
// (#6959). Caller holds i.mu.
func (i *Instance) limitClaimsIntoData(data *InstanceData) {
	if i.liveness == LiveLimitReached {
		data.LimitResetAt = i.limitResetAt
		data.LimitResetEvidence = i.limitResetEvidence
		data.LimitAgent = i.limitAgent
		data.LimitAccount = i.limitAccount
		data.LimitBannerPrefix = limitWallPrefixTail(i.limitBannerPrefix)
		data.LimitBannerCount = i.limitBannerCount
		data.LimitWallRuntimePID = i.limitWallRuntimePID
		data.LimitWallRuntimeStartID = i.limitWallRuntimeStartID
	}
	if wall := limitWallClaimToData(i.lastLimitWall); !wall.IsZero() {
		data.LastLimitWall = &wall
	}
}

// hydrateLimitWallShadowOnRestore fills the shadow from the live claim when a
// restored row is parked but serialized no shadow — a row parked by a build
// that predates LastLimitWall still owns exactly one wall claim, its live
// one, and the residue and stale-read decisions must see the same wall after
// the restart they saw before (#6959).
func (i *Instance) hydrateLimitWallShadowOnRestore() {
	if i.liveness == LiveLimitReached && i.lastLimitWall.agent == "" {
		i.lastLimitWall = limitWallClaim{
			agent: i.limitAgent, account: i.limitAccount,
			resetAt: i.limitResetAt, evidence: i.limitResetEvidence,
			banner:     LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount},
			runtimePID: i.limitWallRuntimePID, runtimeStartID: i.limitWallRuntimeStartID,
		}
	}
}

// RecordedLimitWallExpired reports whether the sighted banner is the row's own
// recorded usage-limit wall whose claimed reset has already passed — residue
// left in the capture, not a wall the send must refuse (#6959 review). The
// readiness wait consults it before honoring a LimitReachedError: a parked
// row at reset+grace sends past the banner it already parked on, and only a
// banner the row never recorded is a live wall.
func (i *Instance) RecordedLimitWallExpired(prefix string, count int, evidence string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.limitWallResidueLocked(
		LimitBanner{Prefix: prefix, Count: count}, evidence, instanceNow())
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
	i.SetLimitReachedAtEvidence(resetAt, "", LimitBanner{})
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
func (i *Instance) SetLimitReachedAtEvidence(resetAt time.Time, evidence string, banner LimitBanner) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setLimitReachedLocked(resetAt, evidence, banner)
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
	return i.setLimitReachedAtEpoch(resetAt, evidence, banner, epoch, false)
}

// SetLimitReachedAtEpochAuthoritative is SetLimitReachedAtEpoch for a sighting
// whose reset instant came from the wall's own structural record rather than
// the pane parse (#6944): the transcript's epoch-seconds resetsAt is not a
// re-read against a later clock, so the "same phrase keeps the first parse"
// dedupe must not swallow it. When the row already parks on the same wall
// claim, the recorded instant is CORRECTED to the record's instant — the pane
// guess it was parked on, the zero an unparseable banner left, or the value a
// pre-fix build persisted — while evidence, banner anchor, and identity stay
// put: it is the same wall, finally with its real deadline.
func (i *Instance) SetLimitReachedAtEpochAuthoritative(resetAt time.Time, evidence string, banner LimitBanner, epoch uint64) bool {
	return i.setLimitReachedAtEpoch(resetAt, evidence, banner, epoch, true)
}

func (i *Instance) setLimitReachedAtEpoch(resetAt time.Time, evidence string, banner LimitBanner, epoch uint64, authoritative bool) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != epoch {
		return false
	}
	if i.liveness == LiveLimitReached {
		if resetAt.IsZero() && evidence == "" {
			return i.applyDegradedResightLocked(banner)
		}
		if evidence == i.limitResetEvidence {
			// Already parked on the SAME wall claim — a re-read of an
			// unchanged banner is not a new observation, whatever shape its
			// reset took: the stored reset stays (the first parse is nearest
			// the real window) and no durable fact moved, so nothing below
			// may run. The one exception is an authoritative instant the
			// stored claim does not already carry — a correction, not a
			// re-read (#6944 review).
			if authoritative && !resetAt.IsZero() && !i.limitResetAt.Equal(resetAt) {
				i.limitResetAt = resetAt
				if i.lastLimitWall.agent != "" {
					i.lastLimitWall.resetAt = resetAt
				}
				i.recordAccountLimitObservationAtLocked(i.limitAgent, i.Account, resetAt, evidence, true, authoritative, instanceNow().UTC())
			}
			return true
		}
	}
	if !i.setLimitReachedLocked(resetAt, evidence, banner) {
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
	if agent := i.currentAgentNameLocked(); agent != i.limitAgent || i.Account != i.limitAccount {
		return i.setLimitReachedLocked(time.Time{}, "", banner)
	}
	if sameLimitBannerPosition(i.limitBannerPrefix, banner.Prefix) {
		return true
	}
	if i.limitResetEvidence != "" && strings.Contains(banner.Prefix, i.limitResetEvidence) {
		return i.setLimitReachedLocked(time.Time{}, "", banner)
	}
	if i.limitBannerCount > 0 && banner.Count > i.limitBannerCount {
		return i.setLimitReachedLocked(time.Time{}, "", banner)
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
func (i *Instance) setLimitReachedLocked(resetAt time.Time, evidence string, banner LimitBanner) bool {
	if i.inFlightOp != OpNone {
		return false
	}
	agent := i.currentAgentNameLocked()
	if i.liveness != LiveLimitReached &&
		i.limitWallResidueLocked(banner, evidence, instanceNow()) {
		// The pane still shows the banner the row already parked on — and its
		// claimed reset has already passed. That sighting is residue, never a
		// wall: a park here re-arms the resume sweep's send on an expired
		// claim, replaying the prompt on every idle poll (#6959 review). Only
		// a banner the row never recorded parks.
		return false
	}
	resetAt, evidence = i.originalLimitResetLocked(agent, resetAt, evidence, banner,
		i.liveness == LiveLimitReached, false, time.Time{}, time.Time{})
	lv, op, prevReset := i.lifecycleStateLocked()
	i.assignLivenessLocked(LiveLimitReached)
	i.limitResetAt = resetAt
	i.limitResetEvidence = evidence
	i.limitBannerPrefix = banner.Prefix
	i.limitBannerCount = banner.Count
	// The wall was sighted on the runtime currently up: pin the claim to it
	// so a sighting judged after a replacement cannot pass as its re-read.
	i.limitWallRuntimePID = i.runtimePID
	i.limitWallRuntimeStartID = i.runtimeStartID
	i.attributeLimitIdentityAsLocked(agent)
	i.noteLimitWallClaimLocked(agent, resetAt, evidence, banner)
	i.recordAccountLimitObservationAtLocked(agent, i.Account, resetAt, evidence, lv == LiveLimitReached, false, instanceNow().UTC())
	i.noteStateChangeLocked(lv, op, prevReset)
	return true
}

// noteLimitWallClaimLocked records the claim the live wall fields just took
// as the row's last parked wall — the shadow a cleared row's re-park consults
// before believing a re-parse minted a new reset (#6959). It always mirrors
// what the row actually parked on: the RESOLVED claim, never the raw
// re-parse. The runtime pin rides from the live fields so a re-mint that only
// restores a prior claim (ReparkLimitUnderResumeFence, SetLimitResetAt) keeps
// the process the wall was sighted on, not the one up at re-mint time.
// Caller holds i.mu.
func (i *Instance) noteLimitWallClaimLocked(agent string, resetAt time.Time, evidence string, banner LimitBanner) {
	i.lastLimitWall = limitWallClaim{
		agent:      agent,
		account:    i.Account,
		resetAt:    resetAt,
		evidence:   evidence,
		banner:     banner,
		runtimePID: i.limitWallRuntimePID, runtimeStartID: i.limitWallRuntimeStartID,
	}
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
func (i *Instance) originalLimitResetLocked(agent string, resetAt time.Time, evidence string, banner LimitBanner, parked, freshEvidence bool, sightedAt, ordSightedAt time.Time) (time.Time, string) {
	if freshEvidence {
		return resetAt, evidence
	}
	if parked && i.limitAgent == agent && i.limitAccount == i.Account &&
		evidence != "" && evidence == i.limitResetEvidence &&
		i.limitWallRuntimeCurrentLocked(i.limitWallRuntimePID, i.limitWallRuntimeStartID) &&
		!ordSightedAt.After(i.accountLimitObservedAtLocked(agent, resetAt)) {
		return i.limitResetAt, i.limitResetEvidence
	}
	if w := i.lastLimitWall; w.agent == agent && w.account == i.Account &&
		i.limitWallRuntimeCurrentLocked(w.runtimePID, w.runtimeStartID) &&
		sameLimitBannerPosition(w.banner.Prefix, banner.Prefix) &&
		!(w.banner.Count > 0 && banner.Count > w.banner.Count) {
		switch {
		case resetAt.IsZero() && evidence == "":
			// The recorded wall's re-read with its clause clipped: same
			// position, nothing parsed. The recorded claim is still the best
			// evidence — and keeping it is what stops the re-park from
			// dropping to "unknown reset" when the banner merely scrolled.
			return w.resetAt, w.evidence
		case evidence != "" && evidence == w.evidence:
			// The phrase the recorded wall advertised re-reads identically:
			// this IS that banner's re-parse, and the first parse stands.
			return w.resetAt, w.evidence
		case evidence != "" && w.evidence == "" && !w.resetAt.IsZero():
			// The recorded wall's clause was never captured (a mirrored or
			// resolved park); the same-position read now shows one. The
			// recorded instant still stands — it is nearer the wall print —
			// and the now-visible phrase is adopted for future re-reads.
			return w.resetAt, evidence
		}
	}
	if banner == (LimitBanner{}) && evidence != "" {
		for idx := range i.accountLimitObservations {
			observation := &i.accountLimitObservations[idx]
			if observation.Agent == agent && observation.Account == i.Account &&
				observation.Evidence == evidence && !sightedAt.After(observation.ObservedAt) {
				return observation.ResetAt, observation.Evidence
			}
		}
	}
	return resetAt, evidence
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
	// was sighted on, which only the shadow still holds after the respawn's
	// clear — so a coincidentally matching banner on the replacement
	// runtime cannot pass as its re-read (#6959 review).
	i.limitWallRuntimePID = i.lastLimitWall.runtimePID
	i.limitWallRuntimeStartID = i.lastLimitWall.runtimeStartID
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
	if i.liveness != LiveLimitReached {
		return
	}
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
