package daemon

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
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
// (rootEnsureSucceeded). A tick on which the deletion tombstone applies does
// not: the deleted project's carries were retired by its delete, and one held
// now belongs to whoever owns the path since (rootEnsureSuppressed).
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
	// compare is workspace resolved for comparison with a deleted path
	// (retireRootCarriesAtLocked), resolved before the carry was held because
	// resolving touches the filesystem.
	compare string
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

// A root create runs on its own goroutine (#3721), and a project delete can
// run at any point of it — including inside the heal's reap, whose teardown
// runs with m.mu released and which DeleteProject's gate does not count (PR
// #7190 review F1). Two things serialize the create against the delete, both
// under m.mu, and neither is a delete-generation counter:
//
//   - Every create holds a TICKET from its launch to its end
//     (rootCreateTicket, stored as its in-flight mark). suppressRootAgent
//     withdraws the ticket of every create for the deleted repository, by id
//     and by path. A withdrawn ticket installs no carry from the reap and
//     commits nothing, so a create that began before a delete never commits
//     after it — whether or not it carries anything (review F2, #7181).
//   - A heal's carry is HELD on st from the moment its reap deletes the record
//     until a root commits, and nothing puts one back. suppressRootAgent
//     retires it, so a carry the create took never outlives the delete.
//
// The create's commit is where reserveCreate checks the delete fence and
// reserves the title (rootCommitRefusalLocked), and whichever comes first wins:
//
//   - A delete that suppresses first has withdrawn the ticket and retired the
//     carry, so the commit refuses — whether the delete is still running,
//     finished long ago, or its tombstone has since been released by a
//     different checkout proven at the path. A delete still running refuses it
//     at reserveCreate's own fence check, which the heal treats as a refusal,
//     not a failed resume (errProjectDeleteFenced, review F3).
//   - A commit first reserves the root's title, and the delete's phase-1 gate
//     refuses while that reservation stands, as it does for every create;
//     retried, it archives the root through its normal path.
//   - A delete that aborts after its fence never reaches suppressRootAgent, so
//     the ticket and the carry are exactly as they were and the next pass
//     resumes the carry.
//
// An occupant's create launched after its tombstone released holds a ticket
// minted after the delete, which nothing withdraws — no tombstone is read at
// the commit (see rootCommitRefusalLocked).

// rootCreateTicket is one root create's claim to commit. launchRootCreate
// mints it in the same m.mu section that marks the create in flight, and stores
// it as that mark (rootCreatesInFlight), so it lives exactly as long as the
// create does. Guarded by m.mu.
type rootCreateTicket struct {
	repoID    string
	workspace string // the create's workspace as resolved by git or recorded
	compare   string // workspace in pathutil.ResolveForCompare form; "" until the create sets it
	withdrawn bool
}

// resolveRootCreateWorkspace resolves workspace for comparison with a deleted
// path — off m.mu, since it touches the filesystem — and records it on the
// create's ticket so a delete from here on can withdraw it by path too.
func (m *Manager) resolveRootCreateWorkspace(ticket *rootCreateTicket, workspace string) string {
	compare := pathutil.ResolveForCompare(filepath.Clean(workspace))
	m.mu.Lock()
	defer m.mu.Unlock()
	if ticket != nil {
		ticket.compare = compare
	}
	return compare
}

// withdrawRootCreateTicketsLocked withdraws every in-flight root create for
// repoID, or whose workspace is the deleted path (resolved for comparison,
// "" for none): the same reach as the carry retirement beside it, so a delete
// that can only name a vanished linked worktree's provisional ids still
// withdraws the create running under its identity root's id. m.mu held.
func (m *Manager) withdrawRootCreateTicketsLocked(repoID, workspace string) {
	for _, ticket := range m.rootCreatesInFlight {
		if ticket.repoID == repoID || (workspace != "" && (ticket.compare == workspace || filepath.Clean(ticket.workspace) == workspace)) {
			ticket.withdrawn = true
		}
	}
}

// rootEnsureSuppressed is rootEnsureSucceeded for a tick on which the deletion
// tombstone applies. It resets the retry state and leaves the carry alone: the
// deleted project's own carries were retired by suppressRootAgent, and clearing
// one here would be a tombstone check by proxy, which an occupant's heal whose
// release lapsed for a tick cannot afford (PR #7190 review F5).
func (m *Manager) rootEnsureSuppressed(st *rootEnsureState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resetRootEnsureLocked(st)
}

// installRootCarryLocked holds the state a heal just reaped on st, in the same
// m.mu section that deletes the dead record (reapDeadRoot). A delete that ran
// during the reap's teardown has withdrawn the create's ticket instead, and the
// caller holds nothing then. compare is the workspace's resolved form. m.mu
// held.
func (m *Manager) installRootCarryLocked(st *rootEnsureState, repo *config.RepoContext, identity *resolvedProjectRoot, compare string, carried reapedRootState) *rootCarry {
	held := rootCarryBinding(repo, identity)
	held.state = carried
	held.compare = compare
	st.carry = &held
	return st.carry
}

// errRootCommitWithdrawn says a root create reached its commit after a delete
// withdrew its ticket, or after the carry it was resuming had been retired.
var errRootCommitWithdrawn = errors.New("root create withdrawn at its commit")

// rootCreateWithdrawn reports whether a root create's error is a refusal it
// must not answer with a fresh agent or count as a failure: its commit was
// withdrawn, or a delete's fence refused it. Either way the carry stays where
// it is and the next pass decides.
func rootCreateWithdrawn(err error) bool {
	return errors.Is(err, errRootCommitWithdrawn) || errors.Is(err, errProjectDeleteFenced)
}

// rootCommitRefusalLocked is the root create's commit check, run by
// reserveCreate under m.mu right after its delete-fence check, before it
// reserves the title (CreateSessionRequest.rootCommitGuard). ticket is the
// create's own; ref is the carry it is resuming, nil for one resuming nothing.
// m.mu held.
//
// It asks about the ticket and the carry, not the deletion tombstone, on
// purpose. A tombstone
// released by a different checkout proven at the path is released only while
// that proof is current — the pass that made it and the next (#3611), about a
// poll interval each — and the sweep admits the occupant's create inside that
// window. Its commit lands later, on its own goroutine (#3721), so a tombstone
// check here would withdraw the occupant's legitimate create whenever it took
// longer than that, again on every re-proof. suppressRootAgent withdraws the
// ticket of every create a delete could matter to, under m.mu, so the
// withdrawal is the delete's evidence, at no cost to a create launched after
// it.
func (m *Manager) rootCommitRefusalLocked(st *rootEnsureState, ticket *rootCreateTicket, ref *rootCarry) error {
	if ticket != nil && ticket.withdrawn {
		return fmt.Errorf("%w: its project was deleted while it was in flight", errRootCommitWithdrawn)
	}
	if ref != nil && st.carry != ref {
		return fmt.Errorf("%w: the conversation and tabs it carried were retired while it was in flight (a project delete or a newer root)", errRootCommitWithdrawn)
	}
	return nil
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
// Both sides are compared resolved: the carry's form was taken before it was
// held (review F4), and its workspace as written still matches too.
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
		if st.carry != nil && (st.carry.compare == workspace || filepath.Clean(st.carry.workspace) == workspace) {
			st.carry = nil
		}
	}
}
