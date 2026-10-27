// Re-attributing a recorded project root that did not resolve at snapshot
// time: the poll-side PASS half of the mechanism (#3247 arm 2, #3299). Split
// out of rootagent_heal.go, which keeps the heal orchestrator and its two
// backoff-paced read retries (registry streak, personal configs). This file
// holds the pass's own machinery — the rootReattributionProbe state the
// spawn/consume phases act on, the grace and result-freshness bounds
// (rootHealProbeGrace, rootHealProbeResultTTL), and reattributeUnresolvedRoots
// itself, the per-tick pass that spawns one probe per unresolved record and
// promotes a verified checkout onto its repo's real identity.
//
// The probe WORKER — what one probe actually establishes and the verdict-side
// gates that read it — stays in rootagent_reattribution_probe.go; the identity
// transition a promotion performs stays in rootagent_identity_move.go.

package daemon

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// rootReattributionProbe is one asynchronous git/marker check for an
// unresolved recorded root. Its result fields are written only by the probe
// goroutine, strictly before done is closed; the poll goroutine reads them
// only after observing that close, so no lock guards them.
type rootReattributionProbe struct {
	done chan struct{}
	// pass is the heal pass that spawned this probe. A pass gives its own
	// probes a bounded blocking wait; probes inherited from earlier passes
	// already had theirs, so they are only ever checked non-blockingly.
	pass uint64
	// completedAt stamps the probe's finish. A completed result is consumed
	// only while fresh (rootHealProbeResultTTL): freshness by completion
	// time, not by pass identity, is what lets a responsive-but-slow mount
	// heal on the next tick instead of being discarded forever, while still
	// bounding how stale an accepted identity check can be (#3299 review
	// rounds 4-5).
	completedAt time.Time
	// repo is set whenever git resolution succeeded, even for rejected
	// checkouts: the resolved identity keys the verdict bridge.
	repo    *config.RepoContext
	matches bool
	// candidate publishes the resolved identity the moment git resolution
	// succeeds, BEFORE the marker verdict: while this probe is unfinished or
	// unconsumed, that repo's root-agent decision is unknowable — the
	// project's personal layer (possibly enabled=false) still sits under the
	// derived ID — so the resolution choke points fail it closed rather than
	// let a legacy entry start the root mid-verification (#3299 review
	// round 8).
	candidate atomic.Pointer[config.RepoContext]
	// mismatch means the marker READ SUCCEEDED and the marker differs or is
	// absent — a proven different checkout. markerUnreadable means the read
	// itself failed (EACCES, EIO): identity unknowable, no rebind advice.
	mismatch         bool
	markerUnreadable bool
	// vanished narrows markerUnreadable: the recorded path itself
	// disappeared between git resolution and the marker read, so the remedy
	// is the path, not marker readability (#3299 review round 12).
	vanished bool
	// settled marks a consumed NEGATIVE result held in place until retryAt:
	// per-entry pacing, so a stalled sibling's hot pass cadence cannot make
	// this entry respawn a git probe every poll tick (#3299 review round 7).
	// Written only by the poll goroutine, after done is closed.
	settled bool
	retryAt time.Time
}

// rootHealProbeGrace bounds how long one heal pass waits for its probes: long
// enough that a healthy local filesystem completes within the same pass (so
// recovery still lands the tick the mount returns), short enough that a
// stalled network mount delays the poll loop by at most this much per tick
// rather than wedging it (#3299 review).
const rootHealProbeGrace = 2 * time.Second

// rootHealProbeResultTTL bounds how stale a completed probe result may be
// when consumed. It must comfortably cover the gap between a probe finishing
// just after its pass's grace and the next poll tick's pass — the
// responsive-but-slow-mount case — while keeping the window in which a
// checkout could be swapped after verification to seconds. The residual
// (a swap landing inside the TTL after a completed probe) is accepted, in
// writing, here: no probe-then-publish design closes it entirely, and the
// alternative — synchronous re-verification at consume time — is exactly the
// poll-goroutine filesystem stall the probes exist to avoid.
const rootHealProbeResultTTL = 30 * time.Second

// reattributeUnresolvedRoots re-attempts git resolution for recorded project
// roots that did not resolve at snapshot time (#3247 arm 2, residue closed by
// #3299). A successful RepoFromPath is content-bearing evidence — a mount
// flap cannot fabricate a resolved toplevel — but AVAILABILITY IS NOT
// IDENTITY (#3299 review): the resolved checkout must also carry the
// project's checkout marker (config.ProjectCheckoutMatches), or a different
// clone reusing the recorded path would inherit the project's personal layer
// and an autonomous root the user never opted that clone into; without the
// marker the project stays unresolved with its mismatch recorded, and the
// log and verdict name the rebind-then-restart remedy. The marker is probed
// at the RECORDED path, never at repo.Root (#3299 review round 2): the marker
// lives in the Git COMMON directory, and only the recorded spelling is
// guaranteed to bind to it — exactly how Register/Rebind probe it. Since
// #3361 repo.Root IS the linked worktree of a bare clone, so the two
// coincide in that shape; they still diverge for a subdirectory registration
// or a spelling that re-resolves through a symlink, where repo.Root is the
// main toplevel and the record names a path below or beside it.
//
// Both checks touch the recorded path's filesystem, so they run in per-entry
// probe goroutines and this pass consumes only completed results, waiting at
// most rootHealProbeGrace in total (#3299 review rounds 3-4): a healthy
// mount recovers within its own tick, a stalled one costs a bounded wait and
// stays fail-closed unresolved. Results are pass-scoped — a probe finishing
// after its pass's grace describes a filesystem from a previous cadence and
// is discarded for a fresh re-check rather than trusted late.
//
// Deletion state may be keyed by EITHER identity. An entry whose derived ID
// sits behind an ACTIVE delete fence (projectDeletes) is skipped outright,
// and on acceptance the snapshot records realID→derivedID in
// reattributedFrom: DeleteProject normalizes an unavailable path to the
// derived ID at whatever moment it runs — including after this pass — so
// every deletion-state consumer checks both identities through the published
// alias instead of relying on a carry-at-transition a concurrent delete
// could miss (#3299 review round 4).
//
// On acceptance the project's layers move from the recorded-path derived ID
// to the repo's REAL identity — which also differs when the recorded root is
// a linked worktree of a bare clone or a subdirectory registration, the
// residue the derived key cannot cover — and the recorded root joins
// projectRoots, so the singleton sweep can create the root this run instead
// of waiting for a daemon start. Mutates the candidate snapshot in place; the
// caller publishes.
// reattributeUnresolvedRoots returns the snapshot mutations' companion
// actions in settles: every probe-state transition that RELEASES a fail-closed
// gate (a negative settling, a successful probe retiring) runs only after the
// caller publishes the healed snapshot, so no concurrent reader can observe
// the release before the state that justifies it (#3299 review rounds 9-10).
func (m *Manager) reattributeUnresolvedRoots(healed *rootAgentSnapshot) (changed bool, settles []func()) {
	if len(healed.unresolvedRoots) == 0 {
		return false, nil
	}
	// Under the lock since #3611: the counter is still written only by the poll
	// goroutine, but the tombstone-release rule now READS it from whatever
	// goroutine is asking, to date the identity verdict it is about to trust.
	m.mu.Lock()
	m.rootHealPassSeq++
	pass := m.rootHealPassSeq
	m.mu.Unlock()
	cloneForWrite := func() {
		if changed {
			return
		}
		// Copy-on-first-write: the maps in the candidate snapshot are shared
		// with the published value until replaced.
		healed.personal = cloneLayerMap(healed.personal)
		healed.personalUnreadable = cloneStringMap(healed.personalUnreadable)
		healed.projectRoots = cloneResolvedRootMap(healed.projectRoots)
		healed.unresolvedRoots = cloneUnresolvedMap(healed.unresolvedRoots)
		changed = true
	}

	// Spawn phase: start (or refresh) every missing probe BEFORE waiting on
	// any result, so one stalled mount cannot starve its siblings of the
	// shared grace — probes run concurrently while the consume phase waits
	// (#3299 review round 5). A completed-but-expired result is replaced by a
	// fresh probe here; a still-running probe is left alone (one outstanding
	// goroutine per entry, however long its mount stalls).
	m.mu.Lock()
	for derivedID, record := range healed.unresolvedRoots {
		probe := m.rootHealProbes[derivedID]
		if probe != nil {
			select {
			case <-probe.done:
				if probe.settled {
					// A consumed negative rests until its own backoff
					// expires; only then does a fresh probe re-check.
					if !nowFunc().Before(probe.retryAt) {
						probe = nil
					}
				} else if nowFunc().Sub(probe.completedAt) > rootHealProbeResultTTL {
					// Freshness expiry, with ONE exception (review id
					// 3787592559): a VERIFIED match the consume phase held
					// back because its project is mid-delete must NOT be
					// replaced. That probe's candidate is the only thing
					// keeping the real repo ID attribution-pending, while the
					// delete's fence and tombstone are still keyed by the
					// derived ID alone — so swapping in a probe that may stall,
					// or find the path gone, re-opens exactly the resurrection
					// window round 12 closed by keeping the probe. It expires
					// once the fence clears, which is also the first moment a
					// fresh check means anything.
					if _, fenced := m.projectDeletes[derivedID]; !probe.matches || !fenced {
						probe = nil
					}
				}
			default:
				// Still running; keep it.
			}
		}
		if probe == nil {
			probe = &rootReattributionProbe{done: make(chan struct{}), pass: pass}
			// The replacement INHERITS the candidate it displaces (review id
			// 3884615895). A fresh probe publishes no candidate until its own
			// RepoFromPath returns, so the swap itself used to drop the gate
			// holding that repo's decision unknowable — and if the new probe
			// then stalls, or the path is gone, the gate never came back while
			// the deletion tombstone was still reachable only through an
			// unpublished alias. Inheriting keeps the gate continuous across
			// the swap; the new probe overwrites it the moment it resolves,
			// and a settled verdict releases it as before.
			if replaced := m.rootHealProbes[derivedID]; replaced != nil {
				if candidate := replaced.candidate.Load(); candidate != nil {
					probe.candidate.Store(candidate)
				}
			}
			m.rootHealProbes[derivedID] = probe
			go runRootReattributionProbe(probe, record)
		}
	}
	m.mu.Unlock()

	// Consume phase: only completed, fresh results mutate the snapshot. A
	// probe spawned by THIS pass gets a bounded blocking wait against the
	// shared once-only deadline; a probe inherited from an earlier pass
	// already had its grace and is checked non-blockingly — its result is
	// consumed the tick after it lands (the pending return keeps that tick
	// close instead of on the failure backoff curve).
	deadline := time.After(rootHealProbeGrace)
	expired := false
	for derivedID, record := range healed.unresolvedRoots {
		m.mu.Lock()
		probe := m.rootHealProbes[derivedID]
		settled := probe != nil && probe.settled
		m.mu.Unlock()
		if probe == nil || settled {
			// A settled negative neither pends nor re-consumes: it waits out
			// its own retryAt, invisible to the pass.
			continue
		}
		ready := false
		select {
		case <-probe.done:
			ready = true
		default:
		}
		if !ready && probe.pass == pass && !expired {
			select {
			case <-probe.done:
				ready = true
			case <-deadline:
				// time.After delivers exactly one value; never receive from
				// it twice (#3299 review round 4). Later entries fall through
				// to non-blocking checks only.
				expired = true
			}
		}
		if !ready {
			// Genuinely in flight: the next unconditional pass (every poll
			// tick) consumes the result the tick after it lands.
			continue
		}
		if nowFunc().Sub(probe.completedAt) > rootHealProbeResultTTL {
			// Completed, but stale — the filesystem it verified is from a
			// previous cadence. The spawn phase next pass replaces it.
			continue
		}
		if !probe.matches {
			// Hold the settled negative in place under its own backoff
			// (#3299 review round 7). The settle itself is DEFERRED to after
			// the caller publishes the healed snapshot, and applied under
			// m.mu (#3299 review round 9): settled releases the
			// attribution-pending gate, and a concurrent verdict observing
			// that release before the unreadable-marker bridge is published
			// would resolve fail-open for an instant; the unsynchronized
			// write also races rootAttributionPendingFor's locked read.
			deferredProbe := probe
			deferredID := derivedID
			settles = append(settles, func() {
				m.mu.Lock()
				m.rootHealProbeFailures[deferredID]++
				deferredProbe.retryAt = nowFunc().Add(rootEnsureBackoffFor(m.rootHealProbeFailures[deferredID]))
				deferredProbe.settled = true
				m.mu.Unlock()
			})
		}
		if !probe.matches {
			// A retry that resolved NOTHING is not evidence (#3299 review ids
			// 3884576551, 3910519842). An unavailable recorded path disproves
			// neither the last candidate nor the last marker verdict, so the
			// standing verdict stands until a CONCRETE identity result
			// supersedes it.
			//
			// That matters most for a proven mismatch, which is the only thing
			// stopping a dead project's personal layer from governing the
			// different clone now at its path: clearing it on an evidence-free
			// retry would let a stale enabled=true start an autonomous root in
			// an already-disproven checkout the moment the legacy sweep
			// resolved again. The entry's backoff is unaffected — the settle
			// above is queued before this returns.
			if probe.inconclusive() {
				continue
			}
			// The probe logged the specifics; a fresh probe next pass keeps
			// re-checking. Record the failure shape for verdict consumers — a
			// proven mismatch prescribes a rebind, an unreadable marker does
			// not. No layer moves: the checkout stays unverified.
			//
			// Written on EVERY concrete outcome, including one that agrees with
			// the flags already there (#3611). The old condition wrote only
			// when a flag changed, which is right for the flags and wrong for
			// the freshness mark beside them: a mismatch the probe re-proves
			// pass after pass would keep the mark of its FIRST proof, go stale
			// while the evidence for it is being renewed, and hold the dead
			// claimant's tombstone against the live occupant it disproves —
			// #3299 review round 15 reversed by an accounting bug rather than a
			// decision.
			//
			// The extra publishes it costs are paced by the probe cadence, not
			// the poll cadence: a settled entry rests on its own backoff, so
			// this arm runs once per probe result, not once per tick, and that
			// backoff decays to rootEnsureBackoffMax. What a publish is not
			// free of is legacyRepoIDSet, which forks git once per root_agents
			// path — bounded by the same curve, and smaller than the probe fork
			// that produced the result being consumed.
			cloneForWrite()
			record.identityMismatch = probe.mismatch
			record.markerUnreadable = probe.markerUnreadable
			record.pathVanished = probe.vanished
			// A probe RESULT establishes what the boot resolution could not,
			// so the boot-time "nobody answered" no longer holds — clearing it
			// here is what keeps a stale flag from wording a verdict whose
			// cause has since been established (#3793).
			record.rootProbeUnanswered = false
			record.identityPass = pass
			healed.unresolvedRoots[derivedID] = record
			// A COMPLETED negative outcome is a normal failed read: it feeds
			// the failure backoff rather than the hot pending cadence, or an
			// unavailable root would fork git on every poll tick forever
			// (#3299 review round 6).
			continue
		}
		repo := probe.repo
		m.mu.Lock()
		_, fenced := m.projectDeletes[derivedID]
		m.mu.Unlock()
		if fenced {
			// The next unconditional pass re-checks once the delete settles;
			// the completed match stays consumable while fresh, and the probe
			// deliberately stays IN the map: retiring it here would release
			// the pending gate without ever publishing the alias, and the
			// same pass's legacy sweep could recreate the mid-delete root
			// (#3299 review round 12).
			m.info().Printf("root agent snapshot: recorded project root %s resolves again, but project %s is mid-delete; leaving it unresolved so the delete keeps its target", record.root, record.projectID)
			continue
		}
		// A verified match is positive proof that the checkout at the recorded
		// path IS this project's. If a delete ran while that could not be
		// established — a temporarily unreadable marker, say — claimantForRecord
		// deliberately left its tombstone unclaimed, and an unclaimed tombstone
		// does not propagate through the alias about to be published, so the
		// still-live legacy or personal enable would recreate the root the user
		// just deleted (#3299 review id 3910107334). Promote it now, on the
		// evidence this pass just established, and BEFORE the alias is
		// published so no window opens between the two.
		//
		// Safe against the occupant shape this file is otherwise full of: an
		// occupant's tombstone at this same derived ID would mean the checkout
		// here is a different clone, which the marker match has just disproven.
		adoptTombstone := func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if claimant, ok := m.deletedRootRepos[derivedID]; ok && claimant == "" {
				m.deletedRootRepos[derivedID] = record.projectID
				m.info().Printf("root agent snapshot: recorded project root %s is verified as project %s's own checkout; adopting its unclaimed deletion tombstone so the delete is not undone by the identity transition", record.root, record.projectID)
			}
		}
		adoptTombstone()
		// …and AGAIN after the caller publishes. The call above only samples
		// what exists now, and a delete resolving concurrently can install its
		// unclaimed tombstone between that sample and the publish — the probe
		// then retires having adopted nothing, and rootDeletionTombstoneApplies
		// refuses to propagate an unattributed tombstone through the very alias
		// this pass published, so the enable recreates the deleted root
		// (#3299 review id 3910299610). Settles run after the publish, which
		// makes the pair effectively atomic from a later delete's point of
		// view: one that installs its tombstone after this runs necessarily
		// saw the published alias and claims through it instead.
		settles = append(settles, adoptTombstone)
		cloneForWrite()
		// The RECORDED root is the create path, exactly as
		// projectRootAgentLayers publishes it for a project that resolved at
		// boot: identity comes from repo.ID, but an in-place root agent runs
		// at the checkout the user registered (#3361's identity/workspace
		// boundary). Parity between the two paths is the contract.
		// Write the reconciliation DOWN, once (#3530). A record keyed by an
		// invented id has just been proven to belong to repo.ID, and the next
		// time this project's path is unavailable that fact cannot be
		// recomputed — it is what lets a delete-by-recorded-path or the TUI
		// reach the project's real state after the path is gone. One-way: the
		// writer never overwrites an identity already recorded.
		// The fences are read by the WRITE itself, under the registry lock
		// (#3530 review ids 3919824579, 3920131413): a check out here could
		// only be earlier than the write, never ordered against it, and having
		// both was what let a DECLINE look like an already-recorded identity
		// (#3530 review id 3920258558). The promotion below keeps its own
		// check, because it moves in-memory state rather than durable state.
		if config.IsDerivedRepoID(derivedID) {
			// The fence is re-asked under the registry lock, so a delete that
			// took that lock first — finding a row with nothing recorded, and
			// no evidence to match it by — is not overtaken by this write
			// (#3530 review id 3920131413).
			wrote, err := config.ReconcileProjectRepoID(record.projectID, repo.ID, m.identityTransitionUnfenced(derivedID, repo.ID))
			switch {
			case errors.Is(err, config.ErrIdentityWriteDeclined):
				// A DECLINE is not a completed write (#3530 review id
				// 3920258558). Falling through would publish the transition
				// whose durable half never happened — and if the delete clears
				// its fence in between, the promotion would even succeed,
				// retiring the probe and leaving the record to revert to its
				// provisional identity on the next start.
				//
				// And the work is CONSUMED, not postponed (#3530 review id
				// 3920441888). Keeping the proof would have the next pass write
				// the identity and promote the row the moment the fence
				// cleared — resurrecting the project the delete just removed,
				// on evidence gathered before it ran. The delete owns this
				// identity's outcome; a transition into it is abandoned, and a
				// LATER pass re-derives one from scratch if the checkout is
				// still there to prove it.
				settledProbe := probe
				settles = append(settles, func() {
					m.mu.Lock()
					settledProbe.settled = true
					settledProbe.retryAt = nowFunc().Add(rootEnsureBackoffFor(m.rootHealProbeFailures[derivedID] + 1))
					m.rootHealProbeFailures[derivedID]++
					m.mu.Unlock()
				})
				m.info().Printf("root agent snapshot: recorded project root %s is verified as repo %s, but a delete holds that identity, so it was not recorded; abandoning this transition — a later pass re-derives one if the checkout is still there to prove it", record.root, repo.ID)
				continue
			case err != nil:
				// The durable write is what makes the promotion survive a
				// restart, so a failure must NOT be followed by an in-memory
				// promotion: the snapshot would move the project to repo.ID
				// while the record still says nothing, and the next daemon
				// start would put it back under the invented id with its
				// layers left behind. Leave everything where it is and let the
				// next pass retry (#3530 review id 3914971915).
				m.warn().Printf("root agent snapshot: recorded project root %s resolves to repo %s, but its identity could not be written to the registry record for project %s; leaving the project under its provisional identity and retrying on the ensure cadence: %v", record.root, repo.ID, record.projectID, err)
				continue
			case wrote:
				m.info().Printf("root agent snapshot: project %s resolved to repo %s for the first time; recording that identity so an absent path still addresses this project", record.projectID, repo.ID)
			}
		}
		// EVERYTHING keyed under the identity being left behind has to come
		// with the project (#3530 review ids 3914971803, 3914971837). Leaving
		// any of it behind reproduces the fail-open this whole mechanism exists
		// to close: resolution under the new id would miss a personal disable,
		// miss a fail-closed latch, or miss a deletion, and start a root from
		// the lower-precedence layers.
		//
		// Deliberately NOT limited to a provisional identity (#3530 review id
		// 3916912953). A REAL id can be left behind too: a reconciled record
		// whose recorded root is a linked workspace keeps that path while its
		// repository's identity root moves, so the same checkout verifies
		// against a marker while resolving to a different real id. Gating the
		// move on IsDerivedRepoID published the project under the new identity
		// with its layers, latch and tombstone still filed under the old one —
		// which is the fail-open, arriving by another door. The durable
		// Project.RepoID stays where it was: that field is one-way by
		// construction and a real→real change is a rebind's business (#3611),
		// while the in-memory snapshot follows the resolution exactly as the
		// startup path already does for a project whose path resolves.
		// Retirement is deferred past publication (#3299 review round 10) and
		// queued only HERE, once the transition is certain to complete: the
		// probe's presence is what keeps this repo attribution-pending, and
		// releasing it before the healed snapshot is visible would let a
		// concurrent verdict resolve against the OLD layers for an instant.
		//
		// "Once it is certain" is the part that moved (#3530 review id
		// 3915722486). Queued right after the fence check above, the
		// retirement still ran when either step below declined to complete the
		// transition — a late delete fence deferring the promotion, or a
		// reconciliation write that failed — so the gate holding the real
		// repository closed was dropped while the deletion tombstone and the
		// personal layer were still keyed by the provisional id. The same
		// pass's legacy sweep could then create the real-ID root in the middle
		// of that delete. Deferring the promotion has to defer its retirement
		// too; both retry on the next pass, together.
		if !promoteRecordedIdentity(m, healed, derivedID, repo.ID) {
			m.info().Printf("root agent snapshot: recorded project root %s is verified as repo %s, but the identity it is filed under (%s) is held by a delete or by another live project; leaving it unresolved for now", record.root, repo.ID, derivedID)
			continue
		}
		retiredID := derivedID
		settles = append(settles, func() {
			m.mu.Lock()
			delete(m.rootHealProbes, retiredID)
			delete(m.rootHealProbeFailures, retiredID)
			m.mu.Unlock()
		})
		healed.projectRoots[repo.ID] = resolvedProjectRoot{
			root: record.root, identityRoot: repo.IdentityPath(), projectID: record.projectID, checkoutID: record.checkoutID,
		}
		delete(healed.unresolvedRoots, derivedID)
		m.info().Printf("root agent snapshot: recorded project root %s resolves again (repo %s, checkout marker verified); its personal layer applies under the repo's real identity and the singleton sweep can ensure it this run", record.root, repo.ID)
	}
	return changed, settles
}
