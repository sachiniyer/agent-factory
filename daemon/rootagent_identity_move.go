package daemon

import (
	"context"
	"errors"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// Moving a project between identities, and finishing the durable write that
// records the one it has (#3530). The heal pass in rootagent_heal.go decides
// WHEN a transition is warranted; this file is what a transition does — which
// state travels with it, which fences defer it, and what a retry may skip.

// retryReconcileOwed re-attempts the durable identity writes a snapshot build
// proved but could not persist (#3530 review id 3916912922).
//
// Only the WRITE is retried. The proof — exact-workspace resolution plus the
// record's own checkout marker — was established when the entry was latched and
// is not re-derived here, so this cannot bind a project to a replacement
// checkout that has taken the path since. An entry is dropped when the write
// succeeds or when the writer reports that nothing was owed (an identity is
// already recorded, or the record is gone); anything else keeps it for the next
// pass.
//
// Runs on the poll goroutine, on every heal pass, and touches no filesystem
// unless something is owed.
func (m *Manager) retryReconcileOwed(healed *rootAgentSnapshot) bool {
	if len(healed.reconcileOwed) == 0 {
		return false
	}
	// The registry is read ONCE for the whole pass, and only when an unproven
	// entry needs it: a proven entry's retry is the write alone.
	var projects []config.Project
	projectsRead := true
	for _, owed := range healed.reconcileOwed {
		if owed.proven {
			continue
		}
		listed, err := config.ListProjects()
		// A failed LIST is not evidence that a project is gone (#3530 review id
		// 3919195000). Dropping unproven entries on it would leave the healer
		// with no work for a path that resolves — so repairing the registry
		// could never complete the proof for the rest of this daemon run,
		// which is the defect the latch exists to prevent.
		projectsRead = err == nil
		projects = listed
		break
	}
	remaining := make(map[string]reconcileOwedEntry, len(healed.reconcileOwed))
	// changed covers BOTH kinds of progress: an entry leaving the latch, and an
	// entry whose content advanced (#3530 review id 3919604378). A proof that
	// succeeded while the write failed only records that in `remaining`, so
	// returning false there would discard it and make the next pass re-derive a
	// proof the checkout may no longer be able to give.
	changed := false
	for projectID, owed := range healed.reconcileOwed {
		if !owed.proven {
			if !projectsRead {
				remaining[projectID] = owed
				continue
			}
			// Re-establish the proof before writing anything. Absence of a
			// record, or a proof that now names a DIFFERENT identity, drops the
			// entry: the first has nothing to write to, and the second means
			// the checkout at that path is not what the boot resolved, which a
			// later pass or a restart re-derives from scratch.
			project, found := projectByID(projects, projectID)
			if !found {
				changed = true
				continue
			}
			proven, ok := config.ResolveRegisteredProjectRepoID(context.Background(), project)
			if !ok {
				remaining[projectID] = owed
				continue
			}
			if proven != owed.repoID {
				// The proof named a DIFFERENT identity than the boot resolved
				// — the checkout is this project's, but its repository's
				// identity root has moved (#3530 review id 3919346220).
				// Dropping the entry here left the snapshot keyed under the
				// stale one with nothing to re-derive it: a legacy opt-in
				// resolving the new identity would start without the project's
				// personal disable. Carry the work to the identity just
				// proven, and move the snapshot with it.
				if !moveResolvedIdentity(m, healed, owed.repoID, proven, project.Root) {
					remaining[projectID] = owed
					continue
				}
				log.InfoLog.Printf("root agent snapshot: project %s's checkout is verified under %s rather than the %s its boot resolved; moving its layers and recording the identity it actually has", projectID, proven, owed.repoID)
				owed.repoID = proven
			}
			owed.proven = true
			changed = true
		}
		wrote, err := config.ReconcileProjectRepoID(projectID, owed.repoID, m.identityTransitionUnfenced(owed.repoID, owed.repoID))
		switch {
		case errors.Is(err, config.ErrIdentityWriteDeclined):
			// CONSUMED rather than retried (#3530 review id 3920441888): a
			// delete holds this identity, and keeping the entry would write it
			// the moment that fence cleared — on a proof taken before the
			// delete ran. A later pass re-derives one if the project is still
			// there to prove it.
			changed = true
			log.InfoLog.Printf("root agent snapshot: project %s's identity %s is held by a delete; abandoning the pending write rather than retrying it after the delete finishes", projectID, owed.repoID)
			continue
		case err != nil:
			remaining[projectID] = owed
			continue
		}
		changed = true
		if wrote {
			log.InfoLog.Printf("root agent snapshot: project %s's identity %s is recorded after all; an absent path now addresses this project as itself", projectID, owed.repoID)
		}
	}
	if !changed {
		return false
	}
	healed.reconcileOwed = remaining
	return true
}

func projectByID(projects []config.Project, id string) (config.Project, bool) {
	for _, project := range projects {
		if project.ID == id {
			return project, true
		}
	}
	return config.Project{}, false
}

// identityTransitionUnfenced is identityTransitionFenced as a predicate to be
// re-asked later — by config, under the registry lock, immediately before a
// durable identity write (#3530 review id 3920131413). A fence read before the
// lock only orders the two calls; this makes the decision itself ordered
// against the write.
func (m *Manager) identityTransitionUnfenced(from, to string) func() bool {
	return func() bool { return !m.identityTransitionFenced(from, to) }
}

// identityTransitionFenced reports that a delete holds either end of an
// identity transition (#3530 review ids 3915518792, 3919604386, 3919824579).
//
// Both ends matter, and so does WHERE it is asked. A delete on the SOURCE owns
// the identity the record is filed under right now. A delete on the
// DESTINATION resolved no registry row — the row still answers to the source —
// so it cannot deregister a project that acquires that identity mid-delete:
// the project would be archived and suppressed under it and come back on the
// next start. Neither fence is ever COPIED across the transition, because a
// copy at an identity its delete does not own would never be cleared; they are
// only read.
func (m *Manager) identityTransitionFenced(from, to string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, fenced := m.projectDeletes[from]; fenced {
		return true
	}
	_, fenced := m.projectDeletes[to]
	return fenced
}

// moveResolvedIdentity carries a RESOLVED project's snapshot state from the
// identity its boot recorded to the one its checkout has just been proven to
// have (#3530 review id 3919346220).
//
// It is promoteRecordedIdentity's counterpart for a project that is IN
// projectRoots rather than unresolved: the layers, latches and tombstones move
// the same way, and the published root moves with them. Refuses for the same
// reasons — a delete holding either identity, or another live project holding
// the one being left behind, which is #3611's case rather than this one's.
func moveResolvedIdentity(m *Manager, healed *rootAgentSnapshot, from, to, root string) bool {
	if from == to {
		return true
	}
	// The published entry must be THIS project's — same root — or it belongs
	// to another claimant at that identity and is not ours to move (#3611's
	// case, as in promoteRecordedIdentity's own guard).
	published, ok := healed.projectRoots[from]
	if !ok || published.root != root {
		return false
	}
	// Copy-on-write before touching anything: these maps are shared with the
	// published snapshot until they are replaced.
	healed.projectRoots = cloneResolvedRootMap(healed.projectRoots)
	healed.personal = cloneLayerMap(healed.personal)
	healed.personalUnreadable = cloneStringMap(healed.personalUnreadable)
	// Removed BEFORE the promotion, because promoteRecordedIdentity refuses
	// while another live project holds the identity being left behind — and
	// this project's own entry would look exactly like one.
	delete(healed.projectRoots, from)
	if !promoteRecordedIdentity(m, healed, from, to) {
		healed.projectRoots[from] = published
		return false
	}
	healed.projectRoots[to] = published
	return true
}

// promoteRecordedIdentity moves every piece of state the identity a project was
// FILED under is holding onto the identity its checkout has just been proven to
// have (#3530).
//
// Usually that is a record written before Project.RepoID: it is keyed by an
// invented id until its path first resolves, and that id is what the snapshot's
// layers, latches and any deletion tombstone were filed under. But it is not
// only that case — a reconciled record whose recorded root is a linked
// workspace keeps that path while its repository's identity root moves, so the
// id left behind is a REAL one (review id 3916912953). Either way, a promotion
// that moved only the project root would leave a personal enabled=false, an
// unreadable-config latch, or a delete invisible to resolution under the new
// id — each of which starts a root the user did not ask for.
//
// Deliberately NOT an alias. The old design kept both identities alive and
// taught every consumer to check the other one, which is the collision class
// this change removes; this moves the state once and leaves nothing behind to
// reconcile later.
// Returns false when the promotion must not happen yet, in which case the
// caller leaves the project on its provisional identity for another pass.
func promoteRecordedIdentity(m *Manager, healed *rootAgentSnapshot, recordedID, realID string) bool {
	if recordedID == realID {
		return true
	}
	if rootPromotionFenceHookForTest != nil {
		rootPromotionFenceHookForTest(recordedID)
	}
	// A delete that fenced this identity between the pass's fence check and
	// here must not have its fence copied forward: DeleteProject's defer
	// removes only the id IT fenced, so a copy at the real id would never be
	// cleared and would reject every later create and restore for that
	// repository until the daemon restarts (#3530 review id 3915518792).
	// Refusing the promotion is the honest answer — the delete owns this
	// identity right now, and the next pass promotes once it settles.
	if m.identityTransitionFenced(recordedID, realID) {
		return false
	}
	// And refuse when the identity being left behind is one ANOTHER live
	// project legitimately holds (#3530 review id 3918535470). The snapshot's
	// maps are keyed by repo id, so two projects at one identity collapse into
	// one entry — which is exactly the two-real-identities-at-one-path case
	// this change does not address (#3611). Moving that entry would hand the
	// occupant's personal policy to the promoted identity, or discard it. A
	// provisional id can never be contested this way (nothing resolves to one),
	// so this only ever fires for a real→real transition; the record stays
	// unresolved, which is the fail-closed direction, until one of the two
	// projects is rebound or removed.
	if _, contested := healed.projectRoots[recordedID]; contested {
		return false
	}
	if layer, ok := healed.personal[recordedID]; ok {
		healed.personal[realID] = layer
		delete(healed.personal, recordedID)
	}
	if projectID, ok := healed.personalUnreadable[recordedID]; ok {
		healed.personalUnreadable[realID] = projectID
		delete(healed.personalUnreadable, recordedID)
	}
	// A deletion recorded against the identity this project was filed under is
	// a deletion of this project, so it has to reach the identity it now has.
	m.mu.Lock()
	if claimant, ok := m.deletedRootRepos[recordedID]; ok {
		if _, already := m.deletedRootRepos[realID]; !already {
			m.deletedRootRepos[realID] = claimant
		}
		delete(m.deletedRootRepos, recordedID)
	}
	m.mu.Unlock()
	return true
}
