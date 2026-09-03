package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// repoResolveClaim words what a failed config.RepoFromPath entitles a
// root-agent sweep to SAY about a candidate path. It exists as one function
// because both sweeps narrate the same repoErr, and before #3500 both said the
// same wrong thing: every resolution failure became "does not resolve to a git
// repository", including the ones where git never answered.
//
// "git answered, and the answer is no" and "we could not ask git" are different
// states, and only the first is a claim about the path. The second is a claim
// about a subprocess — killed, unstartable, or abandoned when its 100ms
// WaitDelay expired on a loaded box — and it establishes nothing about the
// configuration the reader is about to go and audit. The report that opened
// #3500 sent a maintainer to a root_agents entry naming a directory
// `git rev-parse --show-toplevel` resolves perfectly well.
//
// This is the rule #3371 (an exec failure fabricating a definite "no -N
// support") and #3478 (an unconfirmed teardown reported as confirmed) already
// applied in their own subsystems: never convert "could not establish" into a
// verdict. subject names the thing being resolved ("root_agents entry",
// "project <id> root") so one wording serves both sites.
func repoResolveClaim(subject, path string, err error) string {
	switch {
	case config.RepoProbeUnanswered(err):
		// One sentence for every surface that has to say this, so the honest
		// half cannot drift between the daemon log and the CLI/TUI sites the
		// #3504 sweep fixed.
		return config.RepoProbeUnansweredClaim(subject, path)
	case config.PathIsDeterminatelyFree(filepath.Clean(config.ExpandTilde(path)), err):
		return fmt.Sprintf("%s %q does not resolve to a git repository", subject, path)
	default:
		// THE THIRD STATE, and the one this wording was missing (#3794). git
		// ran and exited without a verdict — dubious ownership, an unreadable
		// .git, an invalid .git file — which is neither "we could not ask" nor
		// "the answer is no". Reporting it as the second is #3500's overclaim
		// wearing a completed exit status: a repository may own the path
		// through every one of those failures, so the remedy is the failure,
		// not the path, and a maintainer sent to look at the path finds
		// nothing wrong with it.
		//
		// Same predicate as the dedup set's, so the log line and the set
		// membership cannot disagree about which state this is, and #3771's
		// vocabulary, so the app poll and the daemon log do not either.
		return fmt.Sprintf("%s %q could not be resolved: git ran and failed without a verdict, so whether the path is a git repository is unknown", subject, path)
	}
}

// rootDeletionTombstoneApplies reports whether a root-agent deletion tombstone
// is about repoID. m.mu must be held; layers is the snapshot the caller already
// loaded, so the answer stays consistent with the rest of its resolution.
//
// One function because both the ensure sweep and the materialize verdict ask
// it, and a verdict that disagreed with the sweep would explain a root that is
// running, or fail to explain one that is not.
//
// The reason it needs care at all: a tombstone is recorded under whichever
// identity the deleted project was filed under, and a project whose root did
// not resolve was filed under the DERIVED alias. Before #3530 that alias was a
// plain hash of the recorded path, so it equalled, by construction, the real
// identity of whatever repository was later main-rooted there — one delete
// could suppress an unrelated live project's root. Namespacing the fallback
// retires that collision outright: a derived id carries the `d-` prefix
// RepoIDFromRoot can never produce, so the two value spaces cannot meet.
//
// The claimant rule outlives the collision, for its own reason. An ALIAS
// tombstone counts only when its recorded claimant is the record that OWNS the
// alias (#3299 review id 3884615898) — an occupant-safe delete deliberately
// leaves an unattributed tombstone, and propagating that one reported the old
// project as deleted by someone else's delete, so its root stopped being
// healed.
//
// The cost of the stricter rule, stated rather than hidden: a delete whose
// registry read failed records no claimant, so if only its derived ID was
// tombstoned its root can be recreated for the rest of this run. The durable
// opt-in was already removed, so it cannot survive a restart — and the
// alternative is suppressing a live unrelated project, which is worse.
//
// A DIRECT tombstone at a canonical identity is released only by the round-15
// proven mismatch, which #3611 made conditional on that mismatch still being
// current — see deletedClaimDisproven. Two different repositories main-rooted at
// the same path hash to the same canonical id, and the residue of that is
// unchanged by either issue: an occupant arriving after re-attribution already
// removed the unresolved record has no record to be disproven by, so it has no
// release path at all. Identity REUSE at a reused path, filed under #3599
// option 2 (key the gate on something other than the first-resolved identity),
// and the conservative direction while it stands.
func (m *Manager) rootDeletionTombstoneApplies(layers *rootAgentSnapshot, repoID string) bool {
	if claimant, ok := m.deletedRootRepos[repoID]; ok && !deletedClaimDisproven(layers, repoID, claimant, m.rootHealPassSeq) {
		return true
	}
	return false
}

// rootIdentityProofPassTolerance is how many heal passes old a probe's identity
// verdict may be and still count as CURRENT evidence for RELEASING a deletion
// tombstone: the pass that established it, or the one before.
//
// One pass of slack rather than zero, and it is the publish order that needs it
// (#3611). A pass establishes its verdict in the consume phase and the caller
// publishes the healed snapshot afterwards, so the first readers to see a
// verdict from pass N are typically running in pass N+1 — the ensure sweep in
// the same tick reads it at N, a verdict request arriving between ticks reads it
// at N+1. Zero tolerance would discard evidence for being exactly as old as the
// snapshot carrying it.
//
// It is expressed in PASSES, not seconds, because that is the cadence the
// evidence is renewed on: one pass is one heal cadence, whatever the poll
// interval is set to and however the tests drive it.
const rootIdentityProofPassTolerance = 1

// identityProofIsCurrent reports whether this record's identity verdict was
// established by a probe result in heal pass `pass` or the one before it.
//
// An unestablished verdict (identityPass zero) is never current — it is the
// UNKNOWN state, and a consumer that needs current evidence gets "no", not a
// pass. A mark from the FUTURE is not current either: it cannot arise from the
// single writer, and the alternative on unsigned arithmetic is an underflow
// that would read as freshly proven forever.
func (r unresolvedProjectRecord) identityProofIsCurrent(pass uint64) bool {
	if r.identityPass == 0 || pass < r.identityPass {
		return false
	}
	return pass-r.identityPass <= rootIdentityProofPassTolerance
}

// deletedClaimDisproven reports whether the checkout that now owns repoID is
// PROVEN not to be the one whose delete left the tombstone — the only thing
// that may release it, since availability at an identity is not identity.
//
// "Proven" carries a freshness requirement since #3611, and the requirement is
// this consumer's alone. identityMismatch is the last OBSERVATION, not a
// standing fact, and for a main-root recording the recorded identity is also
// any occupant's real ID — so one repo id covers the deleted project, the clone
// that displaced it, and the original checkout coming back. A tombstone
// released on a mismatch nobody has re-proved acts on whichever of those is at
// the path now: the occupant leaves during the settled probe's backoff, the
// deleted project's own checkout returns, and the still-standing mismatch keeps
// releasing the tombstone that is the only thing suppressing its root.
//
// Re-prove before release, therefore, and hold when the proof cannot be
// re-established: an identity that is merely unknown right now is not a
// disproof, and holding a tombstone at worst delays a live occupant's root by a
// probe cadence, while releasing one wrongly recreates a project the user
// deleted. The other consumers of the same flag are deliberately untouched —
// preservation across an evidence-free retry (#3299 review id 3910519842) wants
// it kept until SUPERSEDED, which is a different tolerance on the same
// evidence, not a different answer.
//
// The cost of the freshness rule, stated rather than hidden: round 15's release
// now happens in the passes that follow a proof rather than throughout the
// probe's backoff. That is enough for the occupant's root, because the ensure
// sweep runs in the same tick as the pass that published the proof and a root,
// once created, is not torn down by a tombstone reapplying — but a create that
// FAILS may find the evidence stale when its own retry comes due, and then
// waits for the next proof. Both curves are rootEnsureBackoffFor, so the wait
// is bounded by the same cadence rather than by anything new.
func deletedClaimDisproven(layers *rootAgentSnapshot, repoID, claimant string, pass uint64) bool {
	if claimant == "" {
		return false
	}
	record, ok := layers.unresolvedRoots[repoID]
	if !ok || !record.identityMismatch || record.projectID != claimant {
		return false
	}
	return record.identityProofIsCurrent(pass)
}
