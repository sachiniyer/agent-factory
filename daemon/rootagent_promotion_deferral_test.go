package daemon

import (
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The two ways a provisional identity can be left half-transitioned (#3530
// review ids 3915722486, 3915722493). Both are about the same asymmetry: the
// derived id is what the deletion tombstone, the personal layer and the
// attribution gate are keyed by until a promotion moves them, so anything that
// releases a gate before the move completes fails the real repository OPEN.

// TestDeferredPromotionKeepsItsProbe pins review id 3915722486 (P1).
//
// The consume phase checked the delete fence, queued the probe's retirement,
// and only then attempted the promotion — which re-checks that fence and
// declines when a delete has landed in between. The retirement ran anyway, so
// the probe that was failing the real repository closed disappeared while the
// deletion tombstone and the personal layer were still filed under the
// provisional id, and the same pass's legacy sweep could create the real-ID
// root in the middle of the derived-ID delete.
func TestDeferredPromotionKeepsItsProbe(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	// A record written before Project.RepoID existed: it is addressed by an
	// invented id until its path first resolves.
	clearRecordedRepoID(t, project.ID)
	writePersonalRootAgent(t, project.ID, "enabled = true")
	realID := repoID(t, repoPath)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	derivedID := config.DerivedRepoIDForUnresolvedRoot(repoPath)
	if _, ok := manager.rootAgentLayers.Load().unresolvedRoots[derivedID]; !ok {
		t.Fatalf("fixture must start with the record unresolved under its provisional identity %s", derivedID)
	}

	// The project's own checkout returns, marker intact, so the probe verifies.
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}

	// A delete fences the provisional identity in the window between the
	// pass's fence check and the promotion's.
	fenced := false
	rootPromotionFenceHookForTest = func(id string) {
		if fenced || id != derivedID {
			return
		}
		fenced = true
		manager.mu.Lock()
		if manager.projectDeletes == nil {
			manager.projectDeletes = make(map[string]struct{})
		}
		manager.projectDeletes[derivedID] = struct{}{}
		manager.mu.Unlock()
	}
	t.Cleanup(func() { rootPromotionFenceHookForTest = nil })

	manager.EnsureRootAgents()

	if !fenced {
		t.Fatalf("fixture never reached the promotion, so it pins nothing")
	}
	layers := manager.rootAgentLayers.Load()
	if _, still := layers.unresolvedRoots[derivedID]; !still {
		t.Fatalf("a deferred promotion must leave the record on its provisional identity %s", derivedID)
	}
	if root, attributed := layers.projectRoots[realID]; attributed {
		t.Fatalf("nothing may be published under the real identity %s (root %q) while the promotion is deferred", realID, root)
	}
	manager.mu.Lock()
	probe := manager.rootHealProbes[derivedID]
	manager.mu.Unlock()
	if probe == nil {
		t.Fatalf("the matched probe was retired although its promotion was deferred: nothing fails repository %s closed any more, while its tombstone and personal layer are still keyed by %s — the same pass's legacy sweep can recreate the real-ID root mid-delete", realID, derivedID)
	}
	if !manager.rootAttributionPendingFor(realID) {
		t.Fatalf("the retained probe must keep %s attribution-pending; that gate is the whole reason the probe is kept", realID)
	}
}

// TestProvisionalDeleteFollowsPendingAttribution pins review id 3915722493
// (P1).
//
// A legacy record's returning checkout publishes its real-ID candidate as soon
// as git answers, and the marker verification that would promote it takes
// longer still. A path that comes back and goes away again therefore leaves a
// delete normalizing to the invented id while the daemon is already holding the
// real one — so the delete archives nothing under the identity this project's
// sessions are keyed by, deregisters the project, and reports success.
func TestProvisionalDeleteFollowsPendingAttribution(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)
	derivedID := config.DerivedRepoIDForUnresolvedRoot(repoPath)
	if derivedID == realID {
		t.Fatalf("fixture must produce disjoint identities, both %s", realID)
	}

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, ok := manager.rootAgentLayers.Load().unresolvedRoots[derivedID]; !ok {
		t.Fatalf("fixture must start with the record unresolved under its provisional identity %s", derivedID)
	}

	// The checkout comes back and its probe resolves the identity — a
	// candidate, published before the marker verdict that promotes it.
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	pending := &rootReattributionProbe{done: make(chan struct{})}
	pending.candidate.Store(repo)
	manager.mu.Lock()
	manager.rootHealProbes[derivedID] = pending
	manager.mu.Unlock()

	// …and the path goes away again before the delete arrives, so
	// normalization sees an unresolvable path and a record with no identity.
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir again: %v", err)
	}

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("DeleteProject by the recorded path: %v", err)
	}
	if result.RepoID != realID {
		t.Fatalf("the delete stayed on the provisional identity %s while the daemon had already resolved %s for this record — it archives nothing under the identity the project's sessions are keyed by and still deregisters the project", result.RepoID, realID)
	}
	manager.mu.Lock()
	_, suppressedReal := manager.deletedRootRepos[realID]
	manager.mu.Unlock()
	if !suppressedReal {
		t.Fatalf("the delete must suppress the identity it is actually deleting (%s)", realID)
	}
}
