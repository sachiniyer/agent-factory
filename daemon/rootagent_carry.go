package daemon

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// A heal reaps the dead root's record — the only pointer to the conversation
// (#2616) and tab roster (#2628) it carries — before its create runs, and that
// create can still fail on a transient after the reap: the in-place admission's
// `git worktree list` is bounded at 10s, the worktree admission wait is
// time-bounded, and the post-reap checkout re-prove reads git (#7088). The next
// pass then found no record to reap and created a blank root. So a heal keeps
// what it carried on the candidate's ensure state until a replacement launches:
// a create that failed after the reap holds it there, the next pass's create
// carries it, a root launched for the repository by any candidate retires it
// (retireRootCarries), and the candidate no longer wanting a root clears it
// (rootEnsureSucceeded).
//
// It is held in memory only: a daemon restart in that window loses it, as it
// lost the reaped record before.

// rootCarry is a held carry and what it is bound to: the repository and
// workspace the candidate resolved to and the checkout it was reaped from. A
// legacy root_agents entry has no checkout identity and its path can be
// retargeted at another repository, so the repository and workspace are part
// of the binding: a changed target drops the carry rather than handing one
// project's conversation to another (#7090 review).
type rootCarry struct {
	state     reapedRootState
	repoID    string
	workspace string
	checkout  string
}

func rootCarryBinding(repo *config.RepoContext, identity *resolvedProjectRoot) rootCarry {
	binding := rootCarry{repoID: repo.ID, workspace: repo.WorkspacePath()}
	if identity != nil {
		binding.checkout = identity.checkoutID
	}
	return binding
}

// heldRootCarry is the state a failed heal left on st for this repository,
// workspace and checkout; a carry bound to any other is dropped. It stays held
// until a launch retires it, so a pass that returns before its create keeps it
// for the next one.
func (m *Manager) heldRootCarry(st *rootEnsureState, repo *config.RepoContext, identity *resolvedProjectRoot) (reapedRootState, bool) {
	ref, state := m.heldRootCarryRef(st, repo, identity)
	return state, ref != nil
}

// heldRootCarryRef is heldRootCarry with the carry's identity, which is what a
// create taking it compares at its commit (rootCommitRefusalLocked).
func (m *Manager) heldRootCarryRef(st *rootEnsureState, repo *config.RepoContext, identity *resolvedProjectRoot) (*rootCarry, reapedRootState) {
	want := rootCarryBinding(repo, identity)
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.carry == nil {
		return nil, reapedRootState{}
	}
	if st.carry.repoID != want.repoID || st.carry.workspace != want.workspace || st.carry.checkout != want.checkout {
		st.carry = nil
		return nil, reapedRootState{}
	}
	return st.carry, st.carry.state
}

// A heal's carry is HELD on st from the moment its reap deletes the record
// until a root commits, and the create that took it never puts one back. That
// is what serializes it against DeleteProject (#7182, PR #7190): the only
// things that end a carry — a delete's suppressRootAgent, a root launch, the
// candidate no longer wanting a root — all run under m.mu, and so does the
// create's commit, where reserveCreate checks the delete fence and reserves the
// title (rootCommitRefusalLocked). Whichever comes first wins:
//
//   - A delete that suppresses before the commit has retired the carry, so the
//     commit finds it gone and refuses — whether the delete is still running,
//     finished long ago, or its tombstone has since been released by a
//     different checkout proven at the path. Nothing re-holds it afterwards.
//     (A delete still running is refused by reserveCreate's own fence check.)
//   - A commit before the delete's fence reserves the root's title, and the
//     delete's phase-1 gate refuses while that reservation stands, as it does
//     for every create; retried, it archives the root through its normal path.
//   - A delete that aborts after its fence never reaches suppressRootAgent, so
//     the carry is exactly as it was and the next pass resumes it.

// installRootCarryLocked holds the state a heal just reaped on st, in the same
// m.mu section that deletes the dead record (reapDeadRoot), so no delete can
// run between the reap and the hold and find nothing to retire. m.mu held.
func (m *Manager) installRootCarryLocked(st *rootEnsureState, repo *config.RepoContext, identity *resolvedProjectRoot, carried reapedRootState) *rootCarry {
	held := rootCarryBinding(repo, identity)
	held.state = carried
	st.carry = &held
	return st.carry
}

// errRootCommitWithdrawn says a root create reached its commit after the carry
// it was resuming had been retired.
var errRootCommitWithdrawn = errors.New("root create withdrawn at its commit")

// rootCommitRefusalLocked is the root create's commit check, run by
// reserveCreate under m.mu right after its delete-fence check, before it
// reserves the title (CreateSessionRequest.rootCommitGuard). ref is the carry
// the create is resuming, nil for one resuming nothing. m.mu held.
//
// It asks about the CARRY, not the deletion tombstone, on purpose. A tombstone
// released by a different checkout proven at the path is released only while
// that proof is current — the pass that made it and the next (#3611), about a
// poll interval each — and the sweep admits the occupant's create inside that
// window. Its commit lands later, on its own goroutine (#3721), so a tombstone
// check here would withdraw the occupant's legitimate create whenever it took
// longer than that, again on every re-proof. Every carry a delete could
// matter to is retired by suppressRootAgent under m.mu, so its absence is the
// delete's evidence, at no cost to anyone else.
func (m *Manager) rootCommitRefusalLocked(st *rootEnsureState, ref *rootCarry) error {
	if ref != nil && st.carry != ref {
		return fmt.Errorf("%w: the conversation and tabs it carried were retired while it was in flight (a project delete or a newer root)", errRootCommitWithdrawn)
	}
	return nil
}

// updateRootCarry records what a failed create settled about its carry — a
// substituted conversation, say — for the next pass, but only while st still
// holds that carry. It never puts back one that was retired. m.mu not held.
func (m *Manager) updateRootCarry(st *rootEnsureState, ref *rootCarry, carried reapedRootState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref != nil && st.carry == ref {
		ref.state = carried
	}
}

// retireRootCarries drops every candidate's carry for repoID once a root for
// it launched. Two root_agents spellings of one repository are two candidates
// with their own retry state: a root one of them launched is newer than what
// the other holds, and a later heal by the other must reap that root's state,
// not replace it with the older snapshot (#7090 review).
func (m *Manager) retireRootCarries(repoID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retireRootCarriesLocked(repoID)
}

// retireRootCarriesLocked is retireRootCarries for a caller holding m.mu:
// deleting the project retires them too (suppressRootAgent).
func (m *Manager) retireRootCarriesLocked(repoID string) {
	for _, st := range m.rootEnsureStates {
		if st.carry != nil && st.carry.repoID == repoID {
			st.carry = nil
		}
	}
}

// retireRootCarriesAtLocked drops every carry whose workspace is the deleted
// path, whatever identity it is bound to (PR #7190 review thread 4555053187).
// A legacy root_agents root reached through a linked worktree runs under its
// identity root's id, while a delete performed after that worktree vanished
// can only name the path's provisional ids — so retirement by identity never
// reaches its carry, and a replacement checkout of the same repository there
// would resume the deleted project's conversation. The path is what the two
// share. workspace is already resolved for comparison (pathutil.ResolveForCompare,
// taken before any lock, since it touches the filesystem); "" retires nothing.
//
// The cost, stated: an unrelated root's held carry at that same, now-absent
// path loses its continuity too. That is the conservative direction — a blank
// root over another project's conversation. m.mu held.
func (m *Manager) retireRootCarriesAtLocked(workspace string) {
	if workspace == "" {
		return
	}
	for _, st := range m.rootEnsureStates {
		if st.carry != nil && filepath.Clean(st.carry.workspace) == workspace {
			st.carry = nil
		}
	}
}
