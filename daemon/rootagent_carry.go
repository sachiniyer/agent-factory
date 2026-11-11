package daemon

import "github.com/sachiniyer/agent-factory/config"

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

// holdRootCarry keeps carried on st for the next pass's create.
func (m *Manager) holdRootCarry(st *rootEnsureState, repo *config.RepoContext, identity *resolvedProjectRoot, carried reapedRootState) {
	held := rootCarryBinding(repo, identity)
	held.state = carried
	m.mu.Lock()
	defer m.mu.Unlock()
	st.carry = &held
}

// heldRootCarry is the state a failed heal left on st for this repository,
// workspace and checkout; a carry bound to any other is dropped. It stays held
// until a launch retires it, so a pass that returns before its create keeps it
// for the next one.
func (m *Manager) heldRootCarry(st *rootEnsureState, repo *config.RepoContext, identity *resolvedProjectRoot) (reapedRootState, bool) {
	want := rootCarryBinding(repo, identity)
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.carry == nil {
		return reapedRootState{}, false
	}
	if st.carry.repoID != want.repoID || st.carry.workspace != want.workspace || st.carry.checkout != want.checkout {
		st.carry = nil
		return reapedRootState{}, false
	}
	return st.carry.state, true
}

// retireRootCarries drops every candidate's carry for repoID once a root for
// it launched. Two root_agents spellings of one repository are two candidates
// with their own retry state: a root one of them launched is newer than what
// the other holds, and a later heal by the other must reap that root's state,
// not replace it with the older snapshot (#7090 review).
func (m *Manager) retireRootCarries(repoID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.rootEnsureStates {
		if st.carry != nil && st.carry.repoID == repoID {
			st.carry = nil
		}
	}
}
