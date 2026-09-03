package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
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
// The refusal is a retryable ensure failure, not a latch: the sweep keeps
// re-checking on its backoff curve, so an original checkout that comes back —
// or a marker that becomes readable again — heals with no restart. That is the
// same always-on contract every other ensure failure honors (#1122).
//
// Which is also why it stays out of rootAgentMaterializeVerdictFor. That
// verdict is snapshot-derived and answers "does policy allow this root" — a
// deletion, a fail-closed unknown, a disable, a project that never resolved.
// No retryable per-attempt ensure failure is a cause there today (a legacy
// root_agents path pointing at a not-yet-cloned repo reads as will-materialize
// while its sweep fails every tick), and this one is the same shape. Making a
// DURABLE mismatch legible on that surface is worth doing, but it needs
// per-repo refusal state the verdict can read, which is its own slice.
func verifyRootCreateCheckout(identity *resolvedProjectRoot) error {
	if identity == nil {
		return nil
	}
	matches, err := config.ProjectCheckoutMatches(identity.root, identity.checkoutID)
	if err != nil {
		return fmt.Errorf("the checkout marker for project %s at %s could not be read, so the checkout there cannot be proven to be the registered one; not starting its root agent until the marker is readable again: %w", identity.projectID, identity.root, err)
	}
	if matches {
		return nil
	}
	// config.ProjectCheckoutMatches answers a determinately-absent root with a
	// plain false, so "no marker" covers two situations that read very
	// differently to whoever has to fix it. Separate them before wording the
	// refusal, on the same determinate-absence rule the re-attribution probe
	// uses; an ambiguous stat proves nothing, so it falls through to the
	// mismatch wording rather than claiming the path is gone.
	if absent, statErr := recordRootAbsent(identity.root); statErr == nil && absent {
		return fmt.Errorf("the recorded root %s of project %s is gone, so there is no registered checkout to start its root agent in; bring the path back, or run `af projects rebind %s <path>` if the checkout moved", identity.root, identity.projectID, identity.projectID)
	}
	return fmt.Errorf("the checkout at %s does not carry project %s's marker %s — a different clone may be reusing the path, so its root agent is not started there (run `af projects rebind %s <path>` if this checkout replaces it, then restart the daemon: the running snapshot keeps the marker id it captured at start)", identity.root, identity.projectID, identity.checkoutID, identity.projectID)
}
