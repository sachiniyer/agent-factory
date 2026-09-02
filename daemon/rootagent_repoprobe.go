package daemon

import (
	"fmt"

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
	if config.RepoProbeUnanswered(err) {
		// One sentence for every surface that has to say this, so the honest
		// half cannot drift between the daemon log and the CLI/TUI sites the
		// #3504 sweep fixed.
		return config.RepoProbeUnansweredClaim(subject, path)
	}
	return fmt.Sprintf("%s %q does not resolve to a git repository", subject, path)
}

// rootDeletionTombstoneApplies reports whether a root-agent deletion tombstone
// is about repoID. m.mu must be held; layers is the snapshot the caller already
// loaded, so the answer stays consistent with the rest of its resolution.
//
// One function because both the ensure sweep and the materialize verdict ask
// it, and a verdict that disagreed with the sweep would explain a root that is
// running, or fail to explain one that is not.
//
// The reason it needs care at all: RepoIDForRecordedRoot is a plain path hash
// and so is RepoIDFromRoot, so a re-attributed project's DERIVED alias equals,
// by construction, the real identity of whatever repository is later
// main-rooted at that recorded path. An ALIAS tombstone therefore counts only
// when its recorded claimant is the record that OWNS the alias (#3299 review id
// 3884615898) — an occupant-safe delete deliberately leaves an unattributed
// tombstone, and propagating that one reported the old project as deleted by
// someone else's delete, so its root stopped being healed.
//
// The cost of the stricter rule, stated rather than hidden: a delete whose
// registry read failed records no claimant, so if only its derived ID was
// tombstoned its root can be recreated for the rest of this run. The durable
// opt-in was already removed, so it cannot survive a restart — and the
// alternative is suppressing a live unrelated project, which is worse.
//
// A DIRECT tombstone at this identity is released only by the round-15 proven
// mismatch. #3530 tracks the residue: once re-attribution has removed the
// unresolved record, an occupant arriving LATER has no release path, because
// the snapshot's project view is boot-time and cannot see a checkout registered
// after start. Namespacing the derived fallback retires that whole class rather
// than guarding one more site.
func (m *Manager) rootDeletionTombstoneApplies(layers *rootAgentSnapshot, repoID string) bool {
	if claimant, ok := m.deletedRootRepos[repoID]; ok && !deletedClaimDisproven(layers, repoID, claimant) {
		return true
	}
	return false
}

// deletedClaimDisproven reports whether the checkout that now owns repoID is
// PROVEN not to be the one whose delete left the tombstone — the only thing
// that may release it, since availability at an identity is not identity.
func deletedClaimDisproven(layers *rootAgentSnapshot, repoID, claimant string) bool {
	if claimant == "" {
		return false
	}
	record, ok := layers.unresolvedRoots[repoID]
	return ok && record.identityMismatch && record.projectID == claimant
}
