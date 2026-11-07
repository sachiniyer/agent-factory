package session

import (
	"time"
	"unicode/utf8"
)

// The recorded-wall machinery of the usage-limit liveness axis: the wall
// claim a park records, the residue verdict that recognizes a re-read of it,
// the runtime pins that bind it to the emitting runtime, and its conversion
// to and from the durable record. The park/clear paths themselves live in
// limit_liveness.go.

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
	// runtimeGen pins the claim to the agent runtime generation that sighted
	// it — the provenance read available even when no concrete process can be
	// resolved (a remote or adopted runtime leaves runtimePID 0, which the
	// PID pin reads as "admissible"). A sighting judged against a claim from
	// a replaced generation is a different emission, never residue (#6972).
	runtimeGen uint64
	// promptSent records that this wall's resume prompt already landed — the
	// tombstone a re-park of the same claim keeps so the scheduler knows the
	// send for this wall already went out (#6972 item 4).
	promptSent bool
}

// limitWallClaimsLocked is the row's recorded wall knowledge: the live claim
// while it is parked, and the shadow the last park left behind once it is not.
// Both describe walls the row ITSELF recorded — the claims a residue check
// compares a sighting against. Index 0 is the live claim, index 1 the retired
// shadow. Caller holds i.mu.
func (i *Instance) limitWallClaimsLocked() [2]limitWallClaim {
	live := limitWallClaim{}
	if i.liveness == LiveLimitReached {
		live = limitWallClaim{
			agent: i.limitAgent, account: i.limitAccount,
			resetAt: i.limitResetAt, evidence: i.limitResetEvidence,
			banner:     LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount},
			runtimePID: i.limitWallRuntimePID, runtimeStartID: i.limitWallRuntimeStartID,
			runtimeGen: i.limitWallRuntimeGen, promptSent: i.limitPromptSent,
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
// anchor is not evidence of a new banner. A claim whose reset is still
// pending keeps its wall: only an EXPIRED claim — or a RETIRED one, the
// shadow a clear left behind, whose emission the row already moved past even
// when it advertised no reset — makes the emission residue (#6972 item 3).
//
// freshRuntime is the caller's knowledge that the sighting was read on a
// pane it just launched: that pane can never carry the recorded emission,
// so a lookalike banner is a new wall no matter what the pins say — a PID
// pin of 0 must not stand in for "same runtime" (#6972 item 1).
// Caller holds i.mu.
func (i *Instance) limitWallResidueLocked(banner LimitBanner, evidence string, now time.Time, freshRuntime bool) bool {
	if freshRuntime {
		return false
	}
	agent := i.currentAgentNameLocked()
	claims := i.limitWallClaimsLocked()
	limit := len(claims)
	if i.liveness == LiveLimitReached {
		// A parked row's shadow only mirrors its live claim — the row has not
		// moved past that emission, so the retired verdict must not apply to
		// it (a reset-less live wall would otherwise read as its own residue
		// and refuse every re-sight, #6972 item 3).
		limit = 1
	}
	for idx, c := range claims[:limit] {
		if c.agent != agent || c.account != i.Account {
			continue
		}
		// The residue reads below prove the pane still displays the emission
		// the claim recorded — impossible once the RUNTIME that displayed it
		// is gone: a re-launched or handoff-replaced runtime showing the same
		// phrase at the same position is a NEW emission that happens to
		// resemble the old one, whatever the anchor says (the A→B→A case —
		// #6959 review). The generation pin is what binds the claim when no
		// concrete process can be resolved; a PID pin of 0 alone cannot
		// stand in for "same runtime" (#6972 item 1).
		if !i.limitWallRuntimeCurrentLocked(c.runtimePID, c.runtimeStartID, c.runtimeGen) {
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
		if c.resetAt.IsZero() {
			// A reset-less claim still parked IS the wall — the residue
			// verdict belongs to the retired shadow alone: the row already
			// moved past that emission, so a re-read of it can only re-arm
			// the interval replay (#6972 item 3).
			if idx == 0 {
				continue
			}
			return true
		}
		if !c.resetAt.After(now) {
			return true
		}
	}
	return false
}

// limitWallRuntimeCurrentLocked reports whether a wall claim's recorded
// runtime can still be the runtime up now. Both pinned and different proves
// a replacement — the pane the claim describes is gone and no sighting can
// be that emission's re-read. The runtime GENERATION is the check that holds
// when no concrete process can be resolved: either side's PID unpinned (a
// remote or adopted runtime, a claim recorded before the pin existed) cannot
// distinguish processes, but a generation mismatch always can (#6972).
// Caller holds i.mu.
func (i *Instance) limitWallRuntimeCurrentLocked(pid int, startID, gen uint64) bool {
	if pid != 0 && i.runtimePID != 0 &&
		(pid != i.runtimePID || startID != i.runtimeStartID) {
		return false
	}
	return gen == i.agentRuntimeGeneration
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
		RuntimeGen: w.runtimeGen, PromptSent: w.promptSent,
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
		runtimeGen: d.RuntimeGen, promptSent: d.PromptSent,
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
		data.LimitWallRuntimeGen = i.limitWallRuntimeGen
		data.LimitPromptSent = i.limitPromptSent
	}
	if wall := limitWallClaimToData(i.lastLimitWall); !wall.IsZero() {
		data.LastLimitWall = &wall
	}
}

// retireLimitWallShadowOnRuntimeReplaceLocked drops the retired wall shadow
// when the agent runtime it names is gone for good: a replacement pane can
// never display the emission the claim recorded, so the shadow can no longer
// matter — keeping it is dead weight in instances.json and a stale veto a
// future banner could trip over (#6972 item 5). The one exception is the
// respawn fence: ReparkLimitUnderResumeFence still reads the shadow to
// restore the wall claim it is re-parking, so under OpRespawning the shadow
// survives the bump and the repark re-mints it. Caller holds i.mu.
func (i *Instance) retireLimitWallShadowOnRuntimeReplaceLocked() {
	if i.inFlightOp != OpRespawning {
		i.lastLimitWall = limitWallClaim{}
	}
}

// restoreLimitClaims writes the serialized row's limit-claim fields back onto
// the instance — the mirror of limitClaimsIntoData, kept beside it so the two
// field sets cannot drift. The shadow is filled next by
// hydrateLimitWallShadowOnRestore for records that predate LastLimitWall.
func (i *Instance) restoreLimitClaims(data InstanceData, limitAgent, limitAccount string,
	accountLimitObservations []AccountLimitObservationData) {
	i.limitResetAt = data.LimitResetAt
	i.limitResetEvidence = data.LimitResetEvidence
	i.limitAgent = limitAgent
	i.limitAccount = limitAccount
	i.limitBannerPrefix = data.LimitBannerPrefix
	i.limitBannerCount = data.LimitBannerCount
	i.limitWallRuntimePID = data.LimitWallRuntimePID
	i.limitWallRuntimeStartID = data.LimitWallRuntimeStartID
	i.limitWallRuntimeGen = data.LimitWallRuntimeGen
	i.limitPromptSent = data.LimitPromptSent
	i.agentRuntimeGeneration = data.AgentRuntimeGeneration
	i.lastLimitWall = limitWallClaimFromData(data.LastLimitWall)
	i.accountLimitObservations = accountLimitObservations
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
			runtimeGen: i.limitWallRuntimeGen, promptSent: i.limitPromptSent,
		}
	}
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
		runtimeGen: i.limitWallRuntimeGen, promptSent: i.limitPromptSent,
	}
}
