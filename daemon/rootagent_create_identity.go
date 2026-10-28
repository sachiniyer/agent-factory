package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The create-boundary identity re-check for registry-backed root agents
// (#3366), split out of rootagent.go along the identity concern.
//
// THE CLASS. The root-agent snapshot binds a repo ID to a create path ONCE —
// at boot, or on re-attribution, where the checkout marker IS verified at bind
// time (#3299/#3334). From then on every create the ensure sweep reaches
// through that binding trusted the path for the rest of the daemon run:
// reserveCreate re-resolves it live for repo IDENTITY, but nothing re-read the
// checkout MARKER. So a verified checkout could be removed and a different
// clone put at the same path, and the next create — a first-ever one, a heal
// after a tmux outage, or a kill-grace expiry — would start the autonomous
// root, carrying the registered project's personal layer, inside a checkout
// that was never proven to be the project's.
//
// AVAILABILITY IS NOT IDENTITY is the rule the rest of this cluster already
// states (#3299 review): git resolving the path says a repo is THERE, and only
// the registry's marker says it is the recorded one. This is that rule applied
// at the one moment the binding is acted on rather than merely held.

// createVerifiedRoot re-proves the checkout and only then creates. The proof has
// to be the LAST thing that happens before a create, not merely something that
// happened earlier in the pass (#3366 review): between the ensure loop's
// pre-reap check and the create sits real blocking work — reapDeadRoot's tmux
// teardown, VS Code editor shutdown and record delete, then a transcript-store
// scan for a carried conversation — and every one of those is window a swap can
// land in. The resume fallbacks retry CreateSession twice more, further out
// still, so they go through here too rather than inheriting a proof taken before
// the failure that provoked them.
//
// This does not make verification and creation atomic; nothing available here
// can, and CreateSession does its own work after this returns. What it removes
// is the AVOIDABLE part of the window — every blocking operation the ensure pass
// itself performs — leaving only the irreducible residue this cluster already
// accepts in writing at rootHealProbeResultTTL.
//
// A nil identity (the legacy root_agents path) verifies nothing, so this is a
// plain CreateSession there.
func (m *Manager) createVerifiedRoot(repoID string, identity *resolvedProjectRoot, req CreateSessionRequest) (session.InstanceData, error) {
	if rootCreateVerifyHookForTest != nil {
		rootCreateVerifyHookForTest()
	}
	if err := m.proveRootCreateCheckout(repoID, identity); err != nil {
		return session.InstanceData{}, err
	}
	return m.CreateSession(context.Background(), req)
}

// rootCreateVerifyHookForTest, when non-nil, runs at the top of
// createVerifiedRoot — standing in for everything that happens between the
// ensure pass's pre-reap identity check and this create's own: the reap's tmux
// teardown, editor shutdown and record delete, and a transcript-store scan. A
// real swap lands inside those milliseconds, so a test that races it pins
// nothing; this holds the window open (the rootReattributionProbeHookForTest
// idiom, for the same reason).
var rootCreateVerifyHookForTest func()

// rootCheckoutRefusal marks an error as an identity refusal from this file,
// leaving its message untouched. The create path needs the distinction: a
// CreateSession failure that IS a refusal must not be retried as if a
// conversation had failed to resume, or one swap would produce three refusals
// and a log that blames the agent's history for a checkout problem.
//
// It carries the outcome class alongside the message because the per-repo
// refusal record the verdict reads (#3714) needs the judgment in a form it can
// switch on, and the judgment is made here — deriving it back out of the
// message would be a second, drifting classifier.
type rootCheckoutRefusal struct {
	err   error
	class rootCreateRefusalClass
}

func (r rootCheckoutRefusal) Error() string { return r.err.Error() }
func (r rootCheckoutRefusal) Unwrap() error { return r.err }

// rootCheckoutRefusalClassOf reports whether err came from
// verifyRootCreateCheckout and, if so, which outcome it was.
func rootCheckoutRefusalClassOf(err error) (rootCreateRefusalClass, bool) {
	var refusal rootCheckoutRefusal
	if errors.As(err, &refusal) {
		return refusal.class, true
	}
	return rootCreateRefusalNone, false
}

// isRootCheckoutRefusal reports whether err came from verifyRootCreateCheckout.
func isRootCheckoutRefusal(err error) bool {
	_, ok := rootCheckoutRefusalClassOf(err)
	return ok
}

// verifyRootCreateCheckout re-proves that the checkout at a registry-backed
// candidate's bound path still carries its project's marker, and returns the
// refusal to fail the create closed when it does not. A nil identity — the
// legacy root_agents path — verifies nothing and returns nil.
//
// WHY LEGACY IS EXEMPT, deliberately. A root_agents entry is an opt-in the user
// wrote against a PATH; there is no registry record behind it and so no
// recorded checkout id a clone there must match. #3334 already settled that
// shape in the same direction: a PROVEN mismatch RELEASES the repo, precisely
// so a legacy opt-in naming that path still applies. Gating it here would
// reverse that, which is a behavior change #3366 does not ask for and no
// evidence here supports.
//
// Fail closed on all three negative outcomes, worded apart because they
// prescribe different remedies (#3299 review rounds 4-5, 12):
//
//   - the marker cannot be READ: identity is unknowable, which is neither
//     absence nor a proven mismatch. Prescribing a rebind here could destroy a
//     transiently unreadable ORIGINAL checkout, so it prescribes none.
//   - the recorded root is determinately GONE: there is no checkout to start
//     anything in, and the remedy is to bring the path back.
//   - the marker is absent or holds another id: a different clone is at the
//     path, and the remedy is a rebind plus a daemon restart.
//
// WHERE THE CALL SITS, and why that is part of the fix. ensureResolvedRoot
// calls this at its create boundary — below the adopt-first early return, above
// the reap:
//
//   - Below the adopt, so a LIVE root costs no marker read. The ensure sweep
//     runs on the daemon's one-second poll cadence and this read touches the
//     bound path's filesystem, so verifying above the adopt would put a stalled
//     mount on the poll goroutine every tick. One read per CREATE has no
//     poll-loop exposure, which is what makes the check affordable at all.
//   - Above the reap, so a refusal costs nothing. The reap deletes the dead root
//     record, and that record holds the only pointer to the conversation (#2616)
//     and tab roster (#2628) the replacement carries. Verifying after it would
//     turn a swapped checkout — or a transiently unreadable marker — into
//     permanent loss of exactly what the heal exists to preserve.
//
// The kill-grace re-create (#1223) passes through the same boundary, so it is
// covered without a second call site.
//
// The one cost of sitting above the reap, named rather than left to be found:
// when reapDeadRoot declines without an error — a user kill holding the title's
// op lock, an async conversation capture still in flight — ensureResolvedRoot
// returns without arming its backoff, so this read (which forks a git rev-parse
// through resolveProjectBinding) repeats on the next tick. Those windows are
// operation-length, not failure-length; a reap that fails PERSISTENTLY returns
// an error and takes the backoff. Paying a fork per tick for a couple of seconds
// is the right side of the trade against reaping a record we are about to
// refuse to replace.
//
// The refusal is a retryable ensure failure, not a latch: the sweep keeps
// re-checking on its backoff curve, so an original checkout that comes back —
// or a marker that becomes readable again — heals with no restart. That is the
// same always-on contract every other ensure failure honors (#1122).
//
// It is now also legible to rootAgentMaterializeVerdictFor, which #3714 gave a
// per-repo record of this outcome to read. The verdict stays snapshot-derived
// for every OTHER question — a deletion, a fail-closed unknown, a disable, a
// project that never resolved — and no other retryable per-attempt ensure
// failure became a cause there: a legacy root_agents path pointing at a
// not-yet-cloned repo still reads as will-materialize while its sweep fails
// every tick (#1122). What earned this one a place is durability. The record
// is kept by proveRootCreateCheckout, which wraps every call to this function.
func verifyRootCreateCheckout(identity *resolvedProjectRoot) error {
	if identity == nil {
		return nil
	}
	if class, err := rootCheckoutIdentityError(identity); err != nil {
		return rootCheckoutRefusal{err: err, class: class}
	}
	return nil
}

// rootCheckoutIdentityError is verifyRootCreateCheckout's judgment, split out so
// the refusal wrapper is applied in exactly one place. Each negative outcome
// names its own class beside its own wording, so the two are decided together
// and a later reader cannot pair a remedy with the wrong cause; a proof returns
// rootCreateRefusalNone rather than a class it does not have.
func rootCheckoutIdentityError(identity *resolvedProjectRoot) (rootCreateRefusalClass, error) {
	matches, err := config.ProjectCheckoutMatches(identity.root, identity.checkoutID)
	if err != nil {
		return rootCreateRefusalMarkerUnreadable, fmt.Errorf("the checkout marker for project %s at %s could not be read, so the checkout there cannot be proven to be the registered one; not starting its root agent until the marker is readable again: %w", identity.projectID, identity.root, err)
	}
	if matches {
		return rootCreateRefusalNone, nil
	}
	// config.ProjectCheckoutMatches answers a determinately-absent root with a
	// plain false, so "no marker" covers two situations that read very
	// differently to whoever has to fix it. Separate them before wording the
	// refusal, on the same determinate-absence rule the re-attribution probe
	// uses; an ambiguous stat proves nothing, so it falls through to the
	// mismatch wording rather than claiming the path is gone.
	if absent, statErr := recordRootAbsent(identity.root); statErr == nil && absent {
		return rootCreateRefusalRootGone, fmt.Errorf("the recorded root %s of project %s is gone, so there is no registered checkout to start its root agent in; bring the path back, or run `af projects rebind %s <path>` if the checkout moved", identity.root, identity.projectID, identity.projectID)
	}
	return rootCreateRefusalMismatch, fmt.Errorf("the checkout at %s does not carry project %s's marker %s — a different clone may be reusing the path, so its root agent is not started there (run `af projects rebind %s <path>` if this checkout replaces it, then restart the daemon: the running snapshot keeps the marker id it captured at start)", identity.root, identity.projectID, identity.checkoutID, identity.projectID)
}
