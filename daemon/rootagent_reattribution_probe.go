package daemon

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The re-attribution PROBE and the gates that read it (#3299). The heal pass in
// rootagent_heal.go decides what to do with a probe's verdict; this file is the
// probe itself — what it establishes, what it deliberately does not, and the
// two directions in which a not-yet-published identity can still be looked up.
//
// The separation matters for one reason above tidiness: almost every review
// finding on this mechanism has been about the difference between a probe that
// REPORTED something and one that merely finished. Keeping the classification
// in one file makes that distinction reviewable on its own.

// inconclusive reports that this probe established NOTHING: git did not
// resolve the recorded path, so there is no identity, no mismatch, no marker
// verdict and no absence finding. It is the shape an unavailable mount
// produces, and it is deliberately distinct from a settled NEGATIVE — a
// mismatch or an unreadable marker are findings, and they release what they
// contradict. This one contradicts nothing, so it may not release anything
// either (#3299 review ids 3884576551, 3908517185).
func (p *rootReattributionProbe) inconclusive() bool {
	// "Established no identity verdict" — which is the question every consumer
	// actually asks. It used to be spelled `p.repo == nil`, a proxy that broke
	// once a probe could resolve the path and still learn nothing from it: an
	// unanswered RE-resolution leaves repo set while proving nothing (#3299
	// review id 3911002406).
	return !p.matches && !p.mismatch && !p.markerUnreadable && !p.vanished
}

// runRootReattributionProbe performs the blocking half of one re-attribution
// check — git resolution and the checkout-marker probe, both of which touch
// the recorded path's filesystem — so the poll goroutine never blocks on a
// stalled mount. It logs its own negative outcomes because the consuming pass
// only learns pass/fail.
func runRootReattributionProbe(probe *rootReattributionProbe, record unresolvedProjectRecord) {
	defer func() {
		probe.completedAt = nowFunc()
		close(probe.done)
	}()
	repo, err := config.RepoFromPath(record.root)
	if err != nil {
		return
	}
	probe.repo = repo
	probe.candidate.Store(repo)
	matches, markerErr := config.ProjectCheckoutMatches(record.root, record.checkoutID)
	if rootReattributionProbeHookForTest != nil {
		rootReattributionProbeHookForTest(record.root)
	}
	// Re-resolve BEFORE trusting ANY marker outcome (#3299 review rounds
	// 13-15): a mount flip between the first resolution and the marker read
	// means the marker was read from a different repository, and binding that
	// verdict — match OR mismatch — to the first identity releases or claims
	// the wrong repo. A vanished or changed resolution is unknowable, re-bound
	// to whatever is at the path now, and re-checked next pass.
	//
	// The marker-ERROR path re-resolves too (review id 3787592555). A read can
	// fail precisely BECAUSE the path flipped to another repository whose
	// marker or git metadata is unreadable, and returning with probe.repo
	// still on the first resolution fails THAT repo closed while the
	// repository actually at the path stays ungated — so a legacy root_agents
	// entry resolving there starts its root off the lower-precedence layers
	// alone, which is the fail-open this whole pass exists to close.
	verify, verr := config.RepoFromPath(record.root)
	switch {
	case verr != nil && errors.Is(verr, config.ErrRepoProbeUnanswered):
		// git never answered the RE-resolution, so this pass established
		// nothing at all — not even that the marker is unreadable (#3299
		// review id 3911002406). Recording a concrete verdict here would
		// overwrite a previously PROVEN same-path mismatch with
		// markerUnreadable and fail the occupant's own legitimate root closed
		// for the backoff, on a probe that observed nothing. Leaving every
		// flag unset makes this inconclusive, and the standing verdict holds.
		log.WarningLog.Printf("root agent snapshot: recorded project root %s could not be re-checked — git never answered the probe — so project %s's previous identity verdict stands (re-checked on the ensure cadence): %v", record.root, record.projectID, verr)
		return
	case verr != nil:
		probe.markerUnreadable = true
		// THREE states, not two (review id 3787021890, sharpened by #3500).
		// "vanished" prescribes "bring the path back", so it may only be set
		// when the path is provably gone:
		//
		//   - git never answered (ErrRepoProbeUnanswered): a subprocess
		//     outcome, not a verdict on the path. Claim nothing about it —
		//     converting "could not establish" into a verdict is the #3500
		//     defect, and repoResolveClaim is master's wording for it.
		//   - git answered and the path is absent: vanished, remedy is to
		//     restore it.
		//   - git answered and the path is there: its repository metadata or
		//     access needs repair, and telling the user to bring back a path
		//     they can see would send them after the wrong thing while every
		//     retry kept failing.
		claim := repoResolveClaim(fmt.Sprintf("project %s recorded root", record.projectID), record.root, verr)
		if errors.Is(verr, config.ErrRepoProbeUnanswered) {
			log.WarningLog.Printf("root agent snapshot: %s during identity verification; leaving it unresolved (re-checked on the ensure cadence): %v", claim, verr)
			return
		}
		if absent, statErr := recordRootAbsent(record.root); statErr == nil && absent {
			probe.vanished = true
			log.WarningLog.Printf("root agent snapshot: recorded project root %s vanished during identity verification; leaving project %s unresolved (re-checked on the ensure cadence): %v", record.root, record.projectID, verr)
			return
		}
		log.WarningLog.Printf("root agent snapshot: %s during identity verification, and the path is still present, so its repository metadata or access needs repair; leaving project %s unresolved (re-checked on the ensure cadence): %v", claim, record.projectID, verr)
		return
	case verify.ID != repo.ID:
		probe.repo = verify
		probe.candidate.Store(verify)
		probe.markerUnreadable = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s changed identity during verification; leaving project %s unresolved (re-checked on the ensure cadence)", record.root, record.projectID)
		return
	case markerErr != nil:
		// The marker could not be READ — identity is unknowable, which is
		// neither absence nor a proven mismatch. No rebind advice: the
		// original checkout may merely be transiently unreadable, and
		// rebinding over it would be destructive (#3299 review round 5).
		probe.markerUnreadable = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s resolves, but its checkout marker could not be read or holds an invalid id; leaving project %s unresolved until the marker is repaired (re-checked on the ensure cadence): %v", record.root, record.projectID, markerErr)
		return
	}
	// Identity equality is NOT checkout equality (review id 3884615893). When
	// the recorded root is a repository's own main root, its ID is a hash of
	// that pathname — so the original checkout and a stranger's clone put
	// there both resolve to the SAME id, and the verify above cannot see the
	// swap. Accepting the first read would then apply this project's policy
	// to a stranger's checkout (starting its program there), and rejecting it
	// would discard a personal disable that is still correct once the
	// original returns. Re-read and require the two reads to AGREE; a
	// disagreement is unknowable, not a verdict.
	//
	// Two agreeing reads do not make this atomic — nothing available here
	// can, and rootHealProbeResultTTL's comment already owns that residue.
	// What they remove is the silent WRONG answer: a swap inside the window
	// now lands on the fail-closed branch instead of on a confident one.
	recheck, recheckErr := config.ProjectCheckoutMatches(record.root, record.checkoutID)
	if recheckErr != nil || recheck != matches {
		probe.markerUnreadable = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s changed under verification — its checkout marker did not read the same twice; leaving project %s unresolved (re-checked on the ensure cadence): %v", record.root, record.projectID, recheckErr)
		return
	}
	if !matches {
		probe.mismatch = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s resolves, but the checkout there does not carry project %s's marker %s — a different clone may be reusing the path; leaving it unresolved (run `af projects rebind %s <path>` if this checkout replaces it, then restart the daemon: the running snapshot keeps the marker id it captured at start)", record.root, record.projectID, record.checkoutID, record.projectID)
		return
	}
	probe.matches = true
}

// rootHealPrePublishHookForTest, when non-nil, runs immediately before the
// healed snapshot is published. It exists for one race that is otherwise not
// reproducible: a delete resolving concurrently can install its unclaimed
// tombstone AFTER the acceptance path sampled for one and BEFORE the alias
// that would carry it becomes visible (#3299 review id 3910299610). That
// window is microseconds wide in a real daemon, so a test that races it pins
// nothing; this holds it open.
var rootHealPrePublishHookForTest func()

// rootReattributionProbeHookForTest, when non-nil, runs inside
// runRootReattributionProbe in the window BETWEEN the checkout-marker read and
// the re-resolution that binds the verdict to an identity. That window is the
// entire subject of the rounds-13-15 rebinding rules and of review findings
// 3787021890 and 3787592555, and nothing else can hold it open: a real mount
// flip or metadata breakage lands inside microseconds, so a test that races it
// pins nothing. Tests mutate the recorded path here and assert the
// classification the probe reaches.
var rootReattributionProbeHookForTest func(root string)

// rootPromotionFenceHookForTest, when non-nil, runs inside
// promoteDerivedIdentity BEFORE it re-checks the delete fence — the window
// between the consume phase's own fence check and the promotion's. A delete
// landing there defers the promotion, and #3530 review id 3915722486 is about
// what the pass must NOT have already done by then. Nothing else can hold that
// window open: it is two locked reads apart in a real daemon.
var rootPromotionFenceHookForTest func(derivedID string)

// recordRootAbsent reports whether a recorded project root is GONE, as opposed
// to present but unresolvable. It is what separates the "bring the path back"
// remedy from "repair its metadata or access" (review id 3787021890), so the
// distinction has to come from the filesystem rather than from a failed
// RepoFromPath, which cannot tell them apart. Stat, not Lstat: a dangling
// symlink at the recorded spelling is a path whose target the user does have
// to bring back. A stat that fails for any OTHER reason is itself unknowable
// and reported as an error, which keeps the generic remedy — never a guess in
// either direction.
func recordRootAbsent(root string) (bool, error) {
	if _, err := os.Stat(root); err != nil {
		// Determinate absence is more than ErrNotExist: an ancestor replaced by
		// a regular file (ENOTDIR), a symlink loop, or an over-long name all
		// PROVE no checkout is there. Recognising only ErrNotExist answered
		// "unknown" for a path that is provably gone, which left a delete's
		// tombstone unclaimed and therefore unreleasable (#3299 review id
		// 3910107324). config owns that rule; this defers to it rather than
		// keeping a second, shorter copy.
		if config.PathDeterminatelyAbsent(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// rootAttributionPendingFor reports that an unconsumed re-attribution probe
// has RESOLVED repoID as some unresolved project's real identity but not yet
// delivered its marker verdict into the published snapshot (#3299 review
// round 8). Until it does, the repo's decision is unknowable: the project's
// personal layer — possibly enabled=false, possibly itself unreadable — still
// sits under the derived ID where resolution cannot see it. Settled negatives
// are NOT pending: a proven mismatch releases the repo (a different project's
// layers do not govern it) and an unreadable marker holds it closed through
// the snapshot bridge instead.
//
// The gate deliberately applies to DELETED projects' probes too (#3299
// review rounds 9-11, converged): while a dead project's marker read stalls,
// fail-closed doubles as the deletion suppression the alias will carry once
// the probe completes; exempting or retiring the probe (both tried) opened a
// window where an in-memory legacy entry could resurrect the deleted root
// before the alias existed. The cost — a replacement checkout at that path
// waits out the stall before its repo can start legacy roots — is the fail-
// closed direction, and it clears the moment the probe's marker read
// finishes (a mismatch settles and releases).
func (m *Manager) rootAttributionPendingFor(repoID string) bool {
	return m.pendingReattributionDerivedID(repoID) != ""
}

// pendingReattributionRealID returns the REAL identity an unconsumed probe has
// already resolved for derivedID, or "" when none has. It is
// pendingReattributionDerivedID's other direction, and DeleteProject needs both
// (#3299 review id 3910107330): a delete arriving by PATH or by derived ID
// while a probe is stalled mid-marker-read finds no published alias, so without
// this it would proceed under the derived ID, archive nothing under the
// candidate real identity, and still deregister the project — leaving live
// sessions as orphans with no registry record.
func (m *Manager) pendingReattributionRealID(derivedID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	probe := m.rootHealProbes[derivedID]
	if probe == nil || (probe.settled && !probe.inconclusive()) {
		return ""
	}
	if c := probe.candidate.Load(); c != nil && c.ID != derivedID {
		return c.ID
	}
	return ""
}

// probeProvedItsCheckout reports what a probe has ESTABLISHED about whether the
// checkout at the recorded path is the record's own, without blocking:
// verified when the marker matched, disproven when the marker read succeeded
// and differed. Neither, when it has not finished or finished without a verdict
// — that is the UNKNOWN state, and it is a state, not a "no".
//
// matches and mismatch are written by the probe goroutine before it closes
// done, so observing done closed is what makes reading them safe.
func probeProvedItsCheckout(p *rootReattributionProbe) (verified, disproven bool) {
	select {
	case <-p.done:
	default:
		return false, false
	}
	return p.matches, p.mismatch
}

// probeStillDeciding reports that a probe holds an unconsumed candidate other
// than the identity its record is filed under — the state in which "which
// project does this id name" has two answers.
//
// ONLY a proven mismatch releases it (#3530 review id 3917756777). A settled
// markerUnreadable or vanished outcome is not a release: those establish
// nothing about whether the candidate is this record's checkout, and treating
// them as one let a delete by either identity act on half the project — the
// record deregistered without its sessions, or the sessions archived without
// the record.
func probeStillDeciding(probe *rootReattributionProbe, recordedID string) bool {
	if probe == nil {
		return false
	}
	if _, disproven := probeProvedItsCheckout(probe); disproven {
		return false
	}
	candidate := probe.candidate.Load()
	return candidate != nil && candidate.ID != recordedID
}

// recordedRootIsGone reports that the recorded root of the project filed under
// recordedID is PROVABLY absent, so no verdict about a checkout there is
// coming (#3530 review id 3917756769).
//
// It is the escape hatch on the refusal below, and it is load-bearing rather
// than an optimisation. A probe whose re-resolution went unanswered settles
// INCONCLUSIVE while keeping its candidate, every replacement inherits that
// candidate, and an absent path makes each replacement inconclusive in turn —
// so nothing ever verifies, disproves or retires it, and without this the
// refusal would stand for the daemon's life while telling the user to retry.
//
// Determinate absence is the same positive evidence claimantForRecord requires:
// a stat that fails in a way that proves nothing is there. A stalled mount says
// nothing and keeps the gate closed.
func (m *Manager) recordedRootIsGone(recordedID string) bool {
	record, ok := m.rootAgentLayers.Load().unresolvedRoots[recordedID]
	if !ok || record.root == "" {
		return false
	}
	absent, err := recordRootAbsent(record.root)
	return err == nil && absent
}

// identityTransitionPendingFor reports that the daemon is mid-transition on the
// identity a request named: a probe keyed by it holds an unconsumed candidate
// that is some OTHER identity, so which project the id names is being decided
// right now (#3530 review ids 3915722493, 3916379586, 3917445659).
//
// It is a REFUSAL predicate, not a redirect. An earlier round tried to follow
// the probe — delete under the identity it had resolved — and that keys on an
// id, which is exactly what a repository at a reused path can also legitimately
// own: deleting an occupant whose real id equals a stale record's recorded one
// found that record's probe and aimed the delete at a different project's
// sessions. The collision this whole change removes, re-entered through the
// probe map. Nothing here acts across identities any more; the caller refuses
// and the next pass, which has the record in hand, completes the transition.
func (m *Manager) identityTransitionPendingFor(repoID string) bool {
	m.mu.Lock()
	probe := m.rootHealProbes[repoID]
	m.mu.Unlock()
	if !probeStillDeciding(probe, repoID) {
		return false
	}
	return !m.recordedRootIsGone(repoID)
}

// identityTransitionPendingOn is the same question from the other side: some
// record's probe has resolved repoID as ITS candidate, so a request naming
// repoID may be naming that record rather than the repository whose id it is
// (#3530 review ids 3916379577, 3916912942, 3917445684).
//
// Callers ask it only when they could find no registry row for repoID, which is
// precisely the state a mid-transition record produces — its row still answers
// to the identity it is filed under. With a row in hand the request has already
// selected its project and nothing is ambiguous.
func (m *Manager) identityTransitionPendingOn(repoID string) bool {
	m.mu.Lock()
	deciding := make([]string, 0, len(m.rootHealProbes))
	for recordedID, probe := range m.rootHealProbes {
		if recordedID == repoID || !probeStillDeciding(probe, recordedID) {
			continue
		}
		if candidate := probe.candidate.Load(); candidate != nil && candidate.ID == repoID {
			deciding = append(deciding, recordedID)
		}
	}
	m.mu.Unlock()
	// The absence check stats the filesystem, so it runs outside the lock.
	for _, recordedID := range deciding {
		if !m.recordedRootIsGone(recordedID) {
			return true
		}
	}
	return false
}

// pendingReattributionDerivedID returns the DERIVED recorded-path ID of an
// unconsumed re-attribution probe that has already resolved repoID as its
// candidate identity, or "" when none has. It is the same question
// rootAttributionPendingFor asks, answered with the identity on the other side
// of the not-yet-published alias, because DeleteProject needs that half: a
// RepoID-only delete for the real ID finds no registry row (the record still
// hashes to the derived ID) and, before reattributedFrom exists, has nothing
// else to follow — so it archives every session, skips DeregisterProject, and
// reports success while the durable registration survives to reappear on the
// next daemon start (review id 3787021883).
//
// Map keys are never "", so the empty string is an unambiguous "no pending
// probe resolved this repo".
func (m *Manager) pendingReattributionDerivedID(repoID string) string {
	m.mu.Lock()
	deciding := make([]string, 0, len(m.rootHealProbes))
	for recordedID, probe := range m.rootHealProbes {
		// The SAME disproof-only release the delete gate uses (#3530 review id
		// 3918120753). Only a proven mismatch says a different project's layers
		// do not govern this repo; an unreadable marker or a vanished path
		// established nothing, and on master they were held closed by the
		// invented-to-real bridge that this change removes. Without the bridge,
		// releasing on them leaves the unreadable verdict published under the
		// recorded id alone while the candidate repo resolves from the global
		// or legacy layers — starting a root the project's personal disable
		// forbids, and adopt-first keeps it running when the gate returns.
		//
		// An INCONCLUSIVE settle never released it either (#3299 review id
		// 3908517185): a verified probe held behind a delete fence past the
		// TTL, whose replacement inherits the real repo ID and finds the path
		// gone, writes no verdict at all — and the inherited candidate is then
		// the only thing holding the gate.
		if probe == nil {
			continue
		}
		if _, disproven := probeProvedItsCheckout(probe); disproven {
			continue
		}
		candidate := probe.candidate.Load()
		if candidate == nil || candidate.ID != repoID {
			continue
		}
		// A CONCRETE settled negative — an unreadable marker, a path that
		// vanished mid-verification — is published in the snapshot under the
		// identity the RECORD is filed under. When that is the same id being
		// asked about, the record's own flags carry the fail-closed verdict and
		// name the actual remedy ("repair the marker"), so holding the gate
		// here would only replace that with a vaguer "pending" one.
		//
		// When they DIFFER, nothing else carries it (#3530 review id
		// 3918120753): the unreadable state sits under the recorded id while
		// the candidate repository resolves from the global and legacy layers,
		// never seeing the project's personal disable. On master the
		// invented-to-real bridge held it; this change removed the bridge, so
		// the gate carries the rule instead.
		//
		// An INCONCLUSIVE settle establishes nothing either way and never
		// releases (#3299 review id 3908517185): a verified probe held behind a
		// delete fence past the TTL, whose replacement inherits the real repo
		// ID and finds the path gone, writes no verdict at all — and the
		// inherited candidate is then the only thing holding the gate.
		if probe.settled && !probe.inconclusive() && recordedID == repoID {
			continue
		}
		deciding = append(deciding, recordedID)
	}
	m.mu.Unlock()
	// The absence escape, for the same reason the delete has one (#3530 review
	// id 3917756769): an unanswered re-resolution keeps its candidate forever
	// through inheriting replacements, so without this a provably-gone recorded
	// root would fail its candidate repository closed for the daemon's life.
	// Stats the filesystem, so it runs outside the lock.
	for _, recordedID := range deciding {
		if !m.recordedRootIsGone(recordedID) {
			return recordedID
		}
	}
	return ""
}

func cloneLayerMap(in map[string]*config.RootAgentLayer) map[string]*config.RootAgentLayer {
	out := make(map[string]*config.RootAgentLayer, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneUnresolvedMap(in map[string]unresolvedProjectRecord) map[string]unresolvedProjectRecord {
	out := make(map[string]unresolvedProjectRecord, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
