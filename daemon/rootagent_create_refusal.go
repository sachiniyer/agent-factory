package daemon

import (
	"fmt"
	"time"
)

// The per-repo create-boundary refusal record (#3714), split out of the create
// path along the one concern it adds: making a DURABLE identity refusal legible
// on the surface that answers "will this repo's root agent materialize".
//
// THE GAP IT CLOSES. #3366/#3711 re-prove the checkout marker at the create
// boundary and fail the ensure closed on a mismatch, and the refusal is a
// per-attempt ensure failure — it lives in the daemon log and nowhere else.
// rootAgentMaterializeVerdictFor is snapshot-derived, and the snapshot still
// holds a perfectly healthy projectRoots binding for the repo, so the verdict
// read rootAgentWillMaterialize while the sweep refused every tick. Every
// send-prompt, watch and monitor delivery targeting `root` in that repo then
// waited targetDeliverWait for a root that was never coming and answered "being
// recreated (tmux momentarily absent)" — a cause that is not the cause, and a
// remedy (wait) that is not the remedy (rebind, then restart).
//
// WHAT MAKES THIS ONE REPORTABLE WHEN OTHER ENSURE FAILURES ARE NOT. The
// verdict answers "does policy allow this root", and no retryable per-attempt
// ensure failure is a cause there: a legacy root_agents path pointing at a
// not-yet-cloned repo reads as will-materialize while its sweep fails every
// tick (#1122), and reporting that would turn the verdict into a health report.
// The create-boundary identity refusal differs in one way that matters — it is
// DURABLE. An original checkout that comes back heals on the next attempt, but
// a genuinely swapped one refuses forever, and a user told "being recreated"
// for hours has been sent nowhere.
//
// WHICH IS WHY THE RECORD IS AN OUTCOME, NOT A COUNTER. It holds the result of
// the most recent create-boundary identity proof and nothing else: a refusal
// records, a proof clears. Durability is therefore expressed by the record
// SURVIVING the sweep's retries rather than by a streak threshold — a transient
// unreadable marker that heals on the backoff clears itself on the next
// attempt, so it never latches, while a real swap keeps re-recording for as
// long as it is real. No escalation threshold is consulted: #3500's exists to
// decide when a LOG line may claim persistence, which is a different question
// from what the current state is.

// rootCreateRefusalClass is the outcome class of a create-boundary identity
// refusal, carried so the verdict can pick the clause that already exists for
// it instead of inventing wording. The three are exactly
// rootCheckoutIdentityError's three negative outcomes, which prescribe three
// different remedies (#3299 review rounds 4-5, 12).
type rootCreateRefusalClass int

const (
	// rootCreateRefusalNone is the zero value, and exists so that no class
	// read apart from its error can silently read as a real outcome. The
	// judgment below returns it on a PROOF, where every other value would be a
	// lie about a checkout that passed.
	rootCreateRefusalNone rootCreateRefusalClass = iota
	// rootCreateRefusalMismatch: the marker read succeeded and the checkout at
	// the bound path does not carry the project's id — a different clone. The
	// remedy is a rebind plus a daemon restart.
	rootCreateRefusalMismatch
	// rootCreateRefusalMarkerUnreadable: the marker could not be READ, so
	// identity is unknowable — neither absence nor a proven mismatch. The
	// remedy is the readability problem, and deliberately NOT a rebind, which
	// over a transiently unreadable ORIGINAL checkout would be destructive.
	rootCreateRefusalMarkerUnreadable
	// rootCreateRefusalRootGone: the bound path is determinately absent. The
	// remedy is the path, not the marker.
	rootCreateRefusalRootGone
)

// rootCreateRefusal is one repo's standing create-boundary identity refusal.
type rootCreateRefusal struct {
	class rootCreateRefusalClass
	// at stamps the refusing attempt on the injectable package clock. It is
	// what separates a live, repeating refusal from the boot-time
	// non-resolution the same clause otherwise describes: without it a
	// consumer's message cannot say whether the daemon is still trying.
	at time.Time
	// binding is the snapshot binding the refusal was made against. A refusal
	// is evidence about THAT path and THAT marker id, so a snapshot the healer
	// has since replaced (a re-attribution rebinding the repo, #3299) makes it
	// evidence about nothing — and its projectID would name the wrong project
	// in the rebind remedy. Recorded here so the read can require the binding
	// to still be current rather than trusting the repo ID alone.
	binding resolvedProjectRoot
}

// recordRootCreateRefusal makes class the repo's standing refusal, replacing
// any earlier one. Keyed by REPO ID, not by the ensure state key, because that
// is the key the verdict asks with — and because two root_agents spellings of
// one repository (a linked worktree path and the main root) are two ensure
// states and one repo. It takes m.mu itself and assumes nothing about which
// goroutine is calling: #3721 moves the create off the poll goroutine, and this
// is written from inside the block that moves.
func (m *Manager) recordRootCreateRefusal(repoID string, binding resolvedProjectRoot, class rootCreateRefusalClass) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rootCreateRefusals[repoID] = rootCreateRefusal{class: class, at: nowFunc(), binding: binding}
}

// clearRootCreateRefusal retires the repo's standing refusal after a proof.
func (m *Manager) clearRootCreateRefusal(repoID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rootCreateRefusals, repoID)
}

// rootCreateRefusalFor returns the repo's standing refusal, but only while it
// is still evidence about the binding the caller holds. A mismatch on a binding
// the snapshot no longer has is stale by construction; it is left in the map
// rather than deleted so this stays a read, and the next create boundary either
// overwrites it or clears it.
func (m *Manager) rootCreateRefusalFor(repoID string, binding resolvedProjectRoot) (rootCreateRefusal, bool) {
	if (binding == resolvedProjectRoot{}) {
		return rootCreateRefusal{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	refusal, ok := m.rootCreateRefusals[repoID]
	if !ok || refusal.binding != binding {
		return rootCreateRefusal{}, false
	}
	return refusal, true
}

// proveRootCreateCheckout is verifyRootCreateCheckout plus the bookkeeping that
// keeps the per-repo record in step with its answer, and it is the ONE seam
// every create-boundary proof goes through — the pre-reap arm and the create
// itself — so a third arm cannot be added that refuses without recording.
//
// It clears on the PROOF rather than only on a completed create. The record
// says what the last identity proof found, and a pass that proves the checkout
// and then fails for an unrelated reason (a reap that cannot take the op lock)
// has still disproven the identity refusal; leaving it standing there would
// report a healed checkout as a mismatch for as long as the other cause lasted.
//
// A legacy root_agents candidate (nil identity) proves nothing about any
// registry marker and so must neither record nor clear: #3334 and #3366 both
// leave that shape ungated, and a legacy create for a repo that also has a
// registry binding must not be allowed to erase what that binding's own create
// established.
func (m *Manager) proveRootCreateCheckout(repoID string, identity *resolvedProjectRoot) error {
	if identity == nil {
		return nil
	}
	err := verifyRootCreateCheckout(identity)
	if err == nil {
		m.clearRootCreateRefusal(repoID)
		return nil
	}
	if class, ok := rootCheckoutRefusalClassOf(err); ok {
		m.recordRootCreateRefusal(repoID, *identity, class)
	}
	return err
}

// rootCreateRefusalClause is what a refusing verdict appends to the cause
// clause it borrowed: the cause wording is reused verbatim (#3714 asks for no
// new cause vocabulary), and this says the one thing that wording cannot —
// that this is a live, repeating refusal at create time rather than a
// condition established once at daemon start, and how long it has been one.
// Empty for a verdict that carries no refusal.
func rootCreateRefusalClause(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return fmt.Sprintf(" — the daemon's last attempt to create it refused for this reason %s ago, and it keeps retrying on its ensure cadence", rootCreateRefusalAge(nowFunc().Sub(at)))
}

// rootCreateRefusalAge renders an age at a resolution a human acts on: seconds
// while it could still be a blip, whole minutes once it cannot. A negative age
// (a clock stepping backwards between the record and the read) reads as 0s
// rather than as a refusal from the future.
func rootCreateRefusalAge(d time.Duration) time.Duration {
	if d < time.Minute {
		if d < 0 {
			return 0
		}
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}
