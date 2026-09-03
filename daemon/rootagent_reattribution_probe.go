package daemon

import (
	"context"
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
	return !p.matches && !p.mismatch && !p.markerUnreadable && !p.vanished && !p.foreignIdentity
}

// rootReattributionProbeStepTimeout bounds EACH git-touching step of one probe
// (#3599). Not the probe as a whole: the step that REBINDS the gate to whatever
// is at the path now must get its own budget, because the case it exists for is
// precisely the one where the step before it overran. Sharing a single deadline
// would leave the re-resolution cancelled before it ran, which is the fail-open
// #3334 (review id 3787592555) added it to close.
//
// The value is rootHealProbeResultTTL's, deliberately reused rather than tuned
// separately, and the two ends it has to fit between are:
//
//   - ABOVE rootHealProbeGrace, and not marginally. A probe is allowed to
//     outlive its pass — that is what the freshness TTL is for, so a
//     responsive-but-slow mount heals on a later tick instead of never. A budget
//     at the grace would convert every such mount into a permanent
//     markerUnreadable.
//   - Finite, because the gate is what is at stake. While a step runs, the
//     recorded root's identity is held fail-closed and a repository the path may
//     have flipped to is not held at all; unbounded meant "for as long as the
//     mount stalls", i.e. forever.
//
// What this does NOT claim: no bound here closes the flip window. B is only
// knowable by resolving the path, which is the operation being waited on, so
// "ungated indefinitely" becomes "ungated for at most a step" and no better.
// Keying the gate on something other than the first-resolved identity is #3599
// option 2, still filed.
//
// It bounds the GIT SUBPROCESSES. A stat or a read wedged on a stale mount is
// not interruptible from Go; what covers that is the probe goroutine itself,
// which is why this work is off the poll loop to begin with.
//
// A var, not a const, only so tests can drive it; production never reassigns it.
var rootReattributionProbeStepTimeout = rootHealProbeResultTTL

// probeStepContext is the deadline one git-touching probe step runs under. Each
// step takes a fresh one: see rootReattributionProbeStepTimeout for why the
// budget is per step and not per probe.
func probeStepContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), rootReattributionProbeStepTimeout)
}

// probeResolveRecordedRoot resolves the recorded path's repository identity
// under one step's budget.
func probeResolveRecordedRoot(root string) (*config.RepoContext, error) {
	ctx, cancel := probeStepContext()
	defer cancel()
	return config.RepoFromPathContext(ctx, root)
}

// probeReadCheckoutMarker reads the checkout marker at the recorded path under
// one step's budget. A read the deadline kills comes back as an error carrying
// ErrRepoProbeUnanswered — never as a clean "does not match" — so the caller
// below classifies it as an unreadable marker rather than as a proven mismatch
// that would release the repository it contradicts (#3500, #3599).
func probeReadCheckoutMarker(record unresolvedProjectRecord) (bool, error) {
	ctx, cancel := probeStepContext()
	defer cancel()
	return config.ProjectCheckoutMatchesContext(ctx, record.root, record.checkoutID)
}

// runRootReattributionProbe performs the blocking half of one re-attribution
// check — git resolution and the checkout-marker probe, both of which touch
// the recorded path's filesystem — so the poll goroutine never blocks on a
// stalled mount. It logs its own negative outcomes because the consuming pass
// only learns pass/fail.
//
// Every git step carries rootReattributionProbeStepTimeout (#3599). The stats
// and file reads between them cannot be bounded from Go at all, which is the
// other half of why this work sits in its own goroutine.
func runRootReattributionProbe(probe *rootReattributionProbe, record unresolvedProjectRecord) {
	defer func() {
		probe.completedAt = nowFunc()
		close(probe.done)
	}()
	repo, err := probeResolveRecordedRoot(record.root)
	if err != nil {
		// Nothing established and nothing gated yet, so this is inconclusive
		// and the entry settles onto its backoff for a fresh probe later. That
		// is also where a first resolution the step budget killed lands: the
		// alternative it replaces was a goroutine wedged on the mount forever,
		// holding this entry's probe slot against every later pass (#3599).
		return
	}
	// SCOPE GATE FIRST (#3299 review id 3911002404). A foreign identity is
	// deferred to #3530, so this probe has no business publishing it as a
	// candidate or reading its marker: publishing gates that REAL repository
	// through rootAttributionPendingFor, and the marker read below waits on the
	// recorded path's filesystem — bounded per step since #3599, but a stalled
	// foreign worktree would still block a legitimate legacy or singleton root
	// reached through another path in that same repository for the whole of it.
	// Classify and leave.
	if repo.ID != config.RepoIDForRecordedRoot(record.root) {
		probe.foreignIdentity = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s resolves to repo %s, whose identity root is not that path, so its layers cannot be attributed without a second identity; project %s stays unresolved until #3530 namespaces the derived fallback", record.root, repo.ID, record.projectID)
		return
	}
	probe.repo = repo
	probe.candidate.Store(repo)
	matches, markerErr := probeReadCheckoutMarker(record)
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
	//
	// Which is why it takes its OWN step budget rather than sharing the marker
	// read's (#3599): the case it exists for is exactly the one where that read
	// overran, so a single probe-wide deadline would leave this cancelled
	// before it ran and the gate still on the first identity.
	verify, verr := probeResolveRecordedRoot(record.root)
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
	recheck, recheckErr := probeReadCheckoutMarker(record)
	if recheckErr != nil || recheck != matches {
		probe.markerUnreadable = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s changed under verification — its checkout marker did not read the same twice; leaving project %s unresolved (re-checked on the ensure cadence): %v", record.root, record.projectID, recheckErr)
		return
	}
	// SCOPE GATE (#3299, residue deferred to #3530). Only a recorded root that
	// IS its repository's identity root is re-attributed here. When the two
	// differ — a linked worktree of a bare clone, a subdirectory registration,
	// or a spelling that re-resolves through a symlink — attributing the record
	// would give the project a SECOND identity, and a derived recorded-path
	// hash is equal BY CONSTRUCTION to the real identity of any repository
	// later main-rooted at that path. Every consumer of that alias then needs
	// its own collision guard, which is the class #3530 removes by namespacing
	// the derived fallback. Until it lands, these records stay unresolved
	// exactly as they are on master today: this defers the residue, it does not
	// regress anything.
	//
	// A concrete verdict rather than an inconclusive one, so the entry settles
	// onto its backoff instead of re-forking git every pass.
	if verify.ID != config.RepoIDForRecordedRoot(record.root) {
		probe.foreignIdentity = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s resolves to repo %s, whose identity root is not that path, so its layers cannot be attributed without a second identity; project %s stays unresolved until #3530 namespaces the derived fallback", record.root, verify.ID, record.projectID)
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
	defer m.mu.Unlock()
	for derivedID, probe := range m.rootHealProbes {
		// A settled probe normally releases the gate: a proven mismatch says
		// a different project's layers do not govern this repo, and an
		// unreadable marker holds it closed through the snapshot bridge
		// instead. An INCONCLUSIVE settle does neither — it established
		// nothing — and settling it anyway dropped a gate its inherited
		// candidate was the last thing holding (#3299 review id 3908517185).
		//
		// The sequence: a verified probe held behind a derived-ID delete
		// fence past the TTL, the fence clears, the replacement inherits the
		// real repo ID, and the path is gone by the time it runs. That
		// replacement writes no bridge and no verdict, and the alias was
		// never published because the verified result was never consumed —
		// so the tombstone stays reachable only through an alias that does
		// not exist, and a legacy entry through another path in the real
		// repository recreates the deleted root. The inherited candidate
		// stays pending until a concrete verdict supersedes it or the alias
		// is published.
		if probe.settled && !probe.inconclusive() {
			continue
		}
		if c := probe.candidate.Load(); c != nil && c.ID == repoID {
			return derivedID
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

func cloneResolvedRootMap(in map[string]resolvedProjectRoot) map[string]resolvedProjectRoot {
	out := make(map[string]resolvedProjectRoot, len(in))
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
