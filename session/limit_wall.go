package session

import (
	"strings"
	"time"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
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
	// retired marks the shadow as a claim the row genuinely moved past — a
	// send-landed, manual, or swap-commit clear — versus the standing-claim
	// mirror every park writes. The distinction must be durable: a churned
	// row serializes no live fields, so after restart the mirror comparison
	// alone cannot tell the two apart and an unsent wall would read as
	// residue (#6972, Codex).
	retired bool
	// retiredKnown records that the retirement verdict was explicitly
	// written — every in-memory park mirror and clear sets it — versus a
	// shadow restored from a record that predates the bit. An unflagged
	// serialized shadow keeps the verdict its writer's build held: a
	// serialized shadow on a non-parked row was a retired wall (#6972,
	// Codex v1.0.347 upgrade path).
	retiredKnown bool
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

// limitWallPrefixDistinguishing reports whether a recorded anchor carries
// content that could prove position — real text above the banner's own row.
// A prefix that is empty, only the banner's gutter marker, or only blank and
// styled lines admits no positional proof (sameLimitBannerPosition rejects
// it for the same reason) and falls back to the phrase/count reads (#6972,
// Codex).
func limitWallPrefixDistinguishing(prefix string) bool {
	body := prefix[:strings.LastIndexByte(prefix, '\n')+1]
	return strings.TrimSpace(xansi.Strip(body)) != ""
}

// limitWallAnchorAdmits reports whether a sighting can still be the recorded
// emission's re-read. Positional proof via sameLimitBannerPosition settles it
// when the claim's anchor carries distinguishing bytes; a claim recorded at
// the capture's start (an empty prefix) cannot be told apart positionally, so
// the fallback reads what remains: the phrase the claim advertised, or — for
// a claim that advertised none — an unchanged match count (a claim that
// recorded count 0 has no anchor at all and admits nothing). The
// count-growth and runtime checks run before this read, so the ambiguity left
// is "same emission" versus "a lookalike new emission at the same start" —
// undecidable locally and resolved toward the recorded claim, the same
// direction the parked same-phrase dedupe takes.
func limitWallAnchorAdmits(claim LimitBanner, claimEvidence string, banner LimitBanner, evidence string) bool {
	if sameLimitBannerPosition(claim.Prefix, banner.Prefix) {
		return true
	}
	if limitWallPrefixDistinguishing(claim.Prefix) {
		return false
	}
	if evidence != "" && evidence == claimEvidence {
		return true
	}
	return evidence == "" && claimEvidence == "" && claim.Count > 0 && banner.Count == claim.Count
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
		if idx == 1 && !i.limitWallShadowIsRetiredLocked() {
			// The shadow still mirrors a standing claim — its live fields were
			// never cleared, so nothing retired the wall. Ordinary pane churn
			// can settle liveness out of parked WITHOUT delivering the pending
			// prompt (the transition keeps the claim fields), and judging the
			// mirror as "retired" would ignore the unchanged banner forever —
			// the row settles Ready and the send is never made (#6972, Codex).
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
		if !limitWallAnchorAdmits(c.banner, c.evidence, banner, evidence) {
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

// limitWallShadowIsRetiredLocked reports whether the shadow is a claim the
// row actually moved past. An explicit retired mark settles it outright —
// in memory every real clear flags it, and the bit is what survives the
// serialization boundary. When standing-claim fields still exist, a shadow
// that no longer mirrors them is retired by construction (a shape that
// should not occur in memory — every park re-mirrors — kept as a belt to
// the flag). When no standing fields exist at all the flag is the only
// record left: a row serialized mid-churn carries its mirror with
// retired=false, and treating it as retired would strand an unsent wall's
// pending send after restart (#6972, Codex). Caller holds i.mu.
func (i *Instance) limitWallShadowIsRetiredLocked() bool {
	w := i.lastLimitWall
	if w.retired {
		return true
	}
	if w.retiredKnown {
		// The verdict was explicitly recorded: an unflagged-standing mirror
		// is not retired, and with standing fields still present the mirror
		// compare is the belt.
		return i.limitAgent != "" && !i.limitWallShadowMirrorsStandingClaimLocked()
	}
	// A shadow restored from a record that predates the retirement bit:
	// its writer's only verdict was that a serialized shadow on a
	// non-parked row was a retired wall — keep that verdict across the
	// upgrade or an expired, already-answered banner re-parks and replays
	// its prompt (#6972, Codex v1.0.347 upgrade path). Standing claim
	// fields would only exist on a parked row, and that case reads the
	// live claim, never this shadow.
	return true
}

// limitWallShadowMirrorsStandingClaimLocked reports whether the retired-wall
// shadow is still only a mirror of the row's standing claim. The shadow is
// rewritten on every park so the row's wall record survives restart; it
// becomes a retired claim only when a clear — the send-landed, manual, or
// transition clear — zeroes the live fields. Churn transitions keep them, so
// a shadow equal to the standing claim is that claim, not evidence the row
// moved past a wall. Caller holds i.mu.
func (i *Instance) limitWallShadowMirrorsStandingClaimLocked() bool {
	w := i.lastLimitWall
	return w.agent != "" &&
		w.agent == i.limitAgent && w.account == i.limitAccount &&
		w.resetAt.Equal(i.limitResetAt) && w.evidence == i.limitResetEvidence &&
		w.banner.Prefix == i.limitBannerPrefix && w.banner.Count == i.limitBannerCount
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
	d := LimitWallClaimData{
		Agent: w.agent, Account: w.account, ResetAt: w.resetAt,
		Evidence:     w.evidence,
		BannerPrefix: limitWallPrefixTail(w.banner.Prefix), BannerCount: w.banner.Count,
		RuntimePID: w.runtimePID, RuntimeStartID: w.runtimeStartID,
		RuntimeGen: w.runtimeGen, PromptSent: w.promptSent,
	}
	if w.retiredKnown {
		retired := w.retired
		d.Retired = &retired
	}
	return d
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
		retired: d.Retired != nil && *d.Retired, retiredKnown: d.Retired != nil,
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
			retiredKnown: true,
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
		retiredKnown: true,
	}
}
