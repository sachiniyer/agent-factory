package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The ways a provisional identity can be left half-transitioned (#3530 review
// ids 3915722486, 3915722493, 3916379565, 3916379577, 3916379586). They share
// one asymmetry: the derived id is what the deletion tombstone, the personal
// layer, the durable root_agents key and the registry row are addressed by
// until a VERIFIED promotion moves them — so releasing a gate, or following an
// identity, before that proof exists acts on the wrong project.

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

// TestDeleteRefusesWhileIdentityIsInTransition pins the answer this lane
// converged on for review ids 3915722493, 3916379586, 3916379577, 3916912942,
// 3917445659 and 3917445684 — one rule instead of six redirects.
//
// While a probe holds an unconsumed candidate, "which project does this id
// name" has two answers, and a delete that picks one archives sessions, tears
// down worktrees and deregisters a record — none of which the verdict arriving
// a moment later can undo. Worse, following the probe keys on an ID, and a
// repository at a reused path can legitimately own the same id: deleting such
// an occupant found a stale record's probe and aimed at a different project
// (3917445659). So the delete refuses and says when to retry; nothing acts
// across identities at all.
func TestDeleteRefusesWhileIdentityIsInTransition(t *testing.T) {
	refused := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("a delete whose target identity is mid-transition must refuse rather than pick one of the two projects it could name")
		}
		if !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("the refusal must state that nothing was mutated: %v", err)
		}
	}
	unchanged := func(t *testing.T, manager *Manager, ids ...string) {
		t.Helper()
		manager.mu.Lock()
		defer manager.mu.Unlock()
		for _, id := range ids {
			if _, suppressed := manager.deletedRootRepos[id]; suppressed {
				t.Fatalf("a refused delete must suppress nothing, but %s is tombstoned", id)
			}
		}
		if len(manager.projectDeletes) != 0 {
			t.Fatalf("a refused delete must leave no fence behind, got %v", manager.projectDeletes)
		}
	}

	// The verified candidate is refused exactly like the unverified one. It is
	// the same question — which identity is this project's — and the answer is
	// one ensure pass away, at which point the record itself carries it.
	for _, tc := range []struct {
		name   string
		finish probeFinisher
	}{
		{"verified but not yet promoted", verifiedProbe},
		{"still reading the marker", inFlightProbe},
	} {
		t.Run("by path, "+tc.name, func(t *testing.T) {
			manager, repoPath, derivedID, realID := pendingAttributionFixture(t, tc.finish)
			_, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
			refused(t, err)
			unchanged(t, manager, derivedID, realID)
		})
		t.Run("by both selectors, "+tc.name, func(t *testing.T) {
			// The TUI's own shape: the recorded path plus the identity it is
			// displaying, which for an unresolved project is the one the record
			// is filed under.
			manager, repoPath, derivedID, realID := pendingAttributionFixture(t, tc.finish)
			_, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath, RepoID: derivedID})
			refused(t, err)
			unchanged(t, manager, derivedID, realID)
		})
		t.Run("by the identity the probe resolved, "+tc.name, func(t *testing.T) {
			// The other direction: a client naming the identity a probe has
			// published, for which no registry row answers yet. Acting would
			// archive that id's sessions and skip DeregisterProject, leaving
			// the record to reappear on the next daemon start.
			manager, _, derivedID, realID := pendingAttributionFixture(t, tc.finish)
			_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: realID})
			refused(t, err)
			unchanged(t, manager, derivedID, realID)
		})
	}

	t.Run("a stale REAL recorded identity is not special", func(t *testing.T) {
		// A reconciled record whose recorded root is a linked workspace can be
		// filed under a real id its checkout no longer resolves to, so the
		// ambiguity is not limited to provisional ids (3917445684, 3917445659).
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		installOptionsRecordingBackend(t)
		repoPath := setupControlRepo(t)
		project := registerTestProject(t, repoPath)
		realID := repoID(t, repoPath)
		staleID := config.RepoIDFromRoot(filepath.Join(testguard.CanonicalTempDir(t), "former-identity-root"))
		if staleID == realID || config.IsDerivedRepoID(staleID) {
			t.Fatalf("fixture needs two distinct REAL identities, got %s and %s", staleID, realID)
		}
		setRecordedRepoID(t, project.ID, staleID)
		hidden := repoPath + ".hidden"
		if err := os.Rename(repoPath, hidden); err != nil {
			t.Fatalf("hide repo dir: %v", err)
		}
		manager, err := NewManager(config.DefaultConfig())
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		if err := os.Rename(hidden, repoPath); err != nil {
			t.Fatalf("restore repo dir: %v", err)
		}
		repo, err := config.RepoFromPath(repoPath)
		if err != nil {
			t.Fatalf("RepoFromPath: %v", err)
		}
		installVerifiedProbe(t, manager, staleID, repo)

		_, err = manager.DeleteProject(DeleteProjectRequest{RepoID: staleID})
		refused(t, err)
		unchanged(t, manager, staleID, realID)
		dir, dirErr := config.ProjectRegistryDir()
		if dirErr != nil {
			t.Fatalf("ProjectRegistryDir: %v", dirErr)
		}
		if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
			t.Fatalf("nor may a refused delete deregister the record: %v", statErr)
		}
	})

	t.Run("a gone checkout clears the ambiguity", func(t *testing.T) {
		// An unanswered re-resolution settles INCONCLUSIVE while keeping its
		// candidate, and every replacement probe inherits it — so with the path
		// absent nothing ever verifies, disproves or retires the transition.
		// Determinate absence is what says no verdict is coming, and without it
		// this refusal stood for the daemon's life while telling the user to
		// retry (#3530 review id 3917756769).
		manager, repoPath, _, _ := attributionFixture(t, inFlightProbe, rootGone)
		result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
		if err != nil {
			t.Fatalf("a provably absent recorded root leaves nothing to establish, so the delete must proceed under the identity the record is filed under: %v", err)
		}
		if result.RepoID == "" {
			t.Fatalf("and it must name the identity it acted under")
		}
	})

	t.Run("an unreadable marker is not a release", func(t *testing.T) {
		// A settled markerUnreadable (or vanished) outcome establishes nothing
		// about whether the candidate is this record's checkout, so it may not
		// release the gate the way a proven mismatch does (#3530 review id
		// 3917756777).
		manager, repoPath, derivedID, realID := pendingAttributionFixture(t, unreadableMarkerProbe)
		_, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
		refused(t, err)
		unchanged(t, manager, derivedID, realID)
	})

	t.Run("an ordinary delete is untouched", func(t *testing.T) {
		// The predicate must not widen: an identity no probe is mid-transition
		// on deletes exactly as it always did.
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		installOptionsRecordingBackend(t)
		repoPath := setupControlRepo(t)
		registerTestProject(t, repoPath)
		manager, err := NewManager(config.DefaultConfig())
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
		if err != nil {
			t.Fatalf("a live project with no probe must delete: %v", err)
		}
		if result.RepoID != repoID(t, repoPath) {
			t.Fatalf("and under its own identity, got %s", result.RepoID)
		}
	})
}

// TestProvisionalDeleteKeepsItsIdentityOnAProvenMismatch is the other side of
// that limit, so the refusal cannot quietly widen into "refuse whenever a probe
// exists". A marker read that SUCCEEDED and differed proves the checkout at the
// recorded path is not this record's, so it moves nothing and the recorded
// identity stands — the delete proceeds exactly as it did before #3530.
func TestProvisionalDeleteKeepsItsIdentityOnAProvenMismatch(t *testing.T) {
	manager, repoPath, derivedID, realID := pendingAttributionFixture(t, mismatchedProbe)

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("a proven mismatch establishes something, so the delete must proceed: %v", err)
	}
	if result.RepoID != derivedID {
		t.Fatalf("a disproven candidate must not move the delete: got %s, want the recorded identity %s (real %s)", result.RepoID, derivedID, realID)
	}
}

// TestReconciledDeleteSweepsTheRecordedPathOptIn pins review id 3916757161
// (P1), and it is the mirror of the redirect case above with no probe and no
// redirect in it at all.
//
// A project that has RECORDED its identity is addressed by that identity when
// its path stops resolving — which is the whole point of #3530 — but a
// root_agents key is still a PATH, and rootAgentKeyMatchesRepo falls back to
// hashing an unresolvable one. For a recorded root that is not its
// repository's identity root, that hash is not the recorded identity, so the
// durable opt-in survived a delete that reported success. Addressing the
// project by its own id is what made this possible: hashing the path, which is
// what master did, happened to sweep it.
func TestReconciledDeleteSweepsTheRecordedPathOptIn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	worktree, project, realID := worktreeRecordedProject(t)
	pathID := config.RepoIDFromRoot(filepath.Clean(worktree))
	if pathID == realID {
		t.Fatalf("fixture must use a recorded root that is not the repository's identity root, both %s", realID)
	}
	// The identity is RECORDED — this record has nothing provisional about it.
	if _, err := config.ReconcileProjectRepoID(project.ID, realID); err != nil {
		t.Fatalf("record the resolved identity: %v", err)
	}

	if err := os.Rename(worktree, worktree+".aside"); err != nil {
		t.Fatalf("hide worktree: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var swept []string
	original := deregisterRootAgents
	deregisterRootAgents = func(ids ...string) ([]string, error) {
		swept = append(swept, ids...)
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = original })

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if err != nil {
		t.Fatalf("DeleteProject by the recorded worktree path: %v", err)
	}
	if result.RepoID != realID {
		t.Fatalf("a recorded identity addresses the project as itself: got %s, want %s", result.RepoID, realID)
	}
	found := false
	for _, id := range swept {
		if id == pathID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the durable root_agents sweep got %v, without the recorded path's own hash %s — a stale key spelled %q is unresolvable, so it answers to that hash and to nothing else, and the opt-in survives to recreate the root", swept, pathID, worktree)
	}
}

// TestOccupiedRecordedRootIsNotSweptByItsHash is the limit of the sweep above
// (#3530 review id 3917445672). A repository appearing at the recorded root
// legitimately OWNS that path's hash, so supplying it would delete the
// occupant's own opt-in on behalf of a delete that targeted this project.
func TestOccupiedRecordedRootIsNotSweptByItsHash(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	worktree, project, realID := worktreeRecordedProject(t)
	pathID := config.RepoIDFromRoot(filepath.Clean(worktree))
	if _, err := config.ReconcileProjectRepoID(project.ID, realID); err != nil {
		t.Fatalf("record the resolved identity: %v", err)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded workspace: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The repository appears in the window the finding describes: after the
	// selectors and the claimant resolved against an ABSENT root, before the
	// locked config mutation reads it.
	deleteProjectPreSweepHookForTest = func() {
		if _, err := os.Stat(worktree); err == nil {
			return
		}
		if err := exec.Command("git", "init", worktree).Run(); err != nil {
			t.Fatalf("git init occupant: %v", err)
		}
		if occupant := repoID(t, worktree); occupant != pathID {
			t.Fatalf("fixture must main-root the occupant at the recorded path, got %s want %s", occupant, pathID)
		}
	}
	t.Cleanup(func() { deleteProjectPreSweepHookForTest = nil })

	var swept []string
	original := deregisterRootAgents
	deregisterRootAgents = func(ids ...string) ([]string, error) {
		swept = append(swept, ids...)
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = original })

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree}); err != nil {
		t.Fatalf("DeleteProject by the recorded path: %v", err)
	}
	if len(swept) == 0 {
		t.Fatalf("fixture must reach the durable sweep, or it pins nothing")
	}
	for _, id := range swept {
		if id == pathID {
			t.Fatalf("the sweep supplied %s, which the occupant now owns: deleting this project would remove a live repository's own root_agents opt-in", pathID)
		}
	}
}

// worktreeRecordedProject registers a project whose recorded root is a linked
// worktree, so the repository's identity root and the recorded root differ by
// construction — the shape where "which id does a stale root_agents key answer
// to" has an observable answer.
func worktreeRecordedProject(t *testing.T) (worktree string, project config.Project, realID string) {
	t.Helper()
	parent := testguard.CanonicalTempDir(t)
	repoPath := filepath.Join(parent, "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	for _, args := range [][]string{
		{"-C", repoPath, "config", "user.email", "t@t"},
		{"-C", repoPath, "config", "user.name", "t"},
		{"-C", repoPath, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	worktree = filepath.Join(parent, "wt")
	if err := exec.Command("git", "-C", repoPath, "worktree", "add", worktree).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	project = registerTestProject(t, repoPath)
	rewriteRecordRootForDeferral(t, project.ID, worktree)
	return worktree, project, repoID(t, worktree)
}

// probeFinisher puts a fixture's probe into the state the delete will see.
type probeFinisher func(*rootReattributionProbe, *config.RepoContext)

// verifiedProbe: the marker read succeeded and matched — positive proof that
// the checkout at the recorded path IS this record's.
func verifiedProbe(p *rootReattributionProbe, repo *config.RepoContext) {
	p.repo = repo
	p.matches = true
	p.completedAt = nowFunc()
	close(p.done)
}

// inFlightProbe: git resolved the path and published its candidate, and the
// marker read has not finished. Which project the path names is unknown.
func inFlightProbe(p *rootReattributionProbe, repo *config.RepoContext) {
	p.repo = repo
}

// unreadableMarkerProbe: the marker could not be READ, so the probe settles
// with a concrete negative that establishes nothing about the candidate.
func unreadableMarkerProbe(p *rootReattributionProbe, repo *config.RepoContext) {
	p.repo = repo
	p.markerUnreadable = true
	p.settled = true
	p.completedAt = nowFunc()
	close(p.done)
}

// mismatchedProbe: the marker read succeeded and differed — a proven different
// clone at the recorded path.
func mismatchedProbe(p *rootReattributionProbe, repo *config.RepoContext) {
	p.repo = repo
	p.mismatch = true
	p.completedAt = nowFunc()
	close(p.done)
}

// pendingAttributionFixture builds the state the delete findings describe: a
// legacy record with no identity, a probe holding a resolved candidate in the
// state `finish` leaves it in, and a recorded path that is unresolvable again
// by the time the delete arrives.
// recordedRootState is what the delete finds at the recorded path, which is
// what decides whether the identity is ambiguous at all.
type recordedRootState int

const (
	// rootUnresolvable: the directory is there and git will not call it a
	// repository — broken metadata, a half-removed checkout. Normalization
	// falls back to the identity the record is filed under while a probe still
	// holds a candidate, which is the ambiguous state the refusal is for.
	rootUnresolvable recordedRootState = iota
	// rootGone: provably nothing there, so no verdict about a checkout at that
	// path can arrive and the recorded identity is the answer (#3530 review id
	// 3917756769).
	rootGone
)

func pendingAttributionFixture(t *testing.T, finish probeFinisher) (manager *Manager, repoPath, derivedID, realID string) {
	return attributionFixture(t, finish, rootUnresolvable)
}

// attributionFixture builds the mid-transition state in the recorded root state
// the caller wants.
func attributionFixture(t *testing.T, finish probeFinisher, state recordedRootState) (manager *Manager, repoPath, derivedID, realID string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath = setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID = repoID(t, repoPath)
	derivedID = config.DerivedRepoIDForUnresolvedRoot(repoPath)
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

	// The checkout comes back and its probe resolves the identity.
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	pending := &rootReattributionProbe{done: make(chan struct{})}
	pending.candidate.Store(repo)
	finish(pending, repo)
	manager.mu.Lock()
	manager.rootHealProbes[derivedID] = pending
	manager.mu.Unlock()

	switch state {
	case rootGone:
		// os.Stat proves nothing is there, which is the evidence that no
		// verdict about this path can arrive.
		if err := os.RemoveAll(repoPath); err != nil {
			t.Fatalf("remove repo dir: %v", err)
		}
	case rootUnresolvable:
		// Present, and not a repository: git ANSWERS that it is not one, so the
		// delete falls back to the recorded identity — with the probe's
		// candidate still unconsumed, which is the ambiguity.
		if err := os.Rename(filepath.Join(repoPath, ".git"), repoPath+".git-aside"); err != nil {
			t.Fatalf("break the checkout metadata: %v", err)
		}
		if _, err := config.RepoFromPath(repoPath); err == nil {
			t.Fatalf("fixture must leave %s present but unresolvable", repoPath)
		}
	}
	return manager, repoPath, derivedID, realID
}

func installVerifiedProbe(t *testing.T, manager *Manager, derivedID string, repo *config.RepoContext) {
	t.Helper()
	probe := &rootReattributionProbe{done: make(chan struct{})}
	probe.candidate.Store(repo)
	verifiedProbe(probe, repo)
	manager.mu.Lock()
	manager.rootHealProbes[derivedID] = probe
	manager.mu.Unlock()
}

func onlyRegisteredProject(t *testing.T) config.Project {
	t.Helper()
	projects, err := config.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected exactly one registered project, got %d", len(projects))
	}
	return projects[0]
}

// TestRealToRealReattributionCarriesItsState pins review id 3916912953 (P1).
//
// The identity a project is FILED under is not always provisional. A
// reconciled record whose recorded root is a linked workspace keeps that path
// while its repository's identity root moves, so the same checkout verifies
// against its own marker and resolves to a different REAL id. Gating the state
// move on IsDerivedRepoID published the project under the new identity while
// its personal layer, unreadable latch and deletion tombstone stayed under the
// old one — so a global or legacy enable could start the new-ID root while the
// personal disable sat one identity away, unread.
func TestRealToRealReattributionCarriesItsState(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	realID := repoID(t, repoPath)
	// The identity the record wrote down is real and stale: the checkout has
	// not moved, its repository's identity root has. Nothing here is
	// provisional, which is the whole point.
	staleID := config.RepoIDFromRoot(filepath.Join(testguard.CanonicalTempDir(t), "former-identity-root"))
	if staleID == realID || config.IsDerivedRepoID(staleID) {
		t.Fatalf("fixture needs two distinct REAL identities, got %s and %s", staleID, realID)
	}
	setRecordedRepoID(t, project.ID, staleID)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	before := manager.rootAgentLayers.Load()
	if _, ok := before.unresolvedRoots[staleID]; !ok {
		t.Fatalf("fixture must file the record under its written identity %s, got %+v", staleID, before.unresolvedRoots)
	}
	if _, ok := before.personal[staleID]; !ok {
		t.Fatalf("fixture must file the personal layer under the same identity %s", staleID)
	}

	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}
	manager.EnsureRootAgents()

	layers := manager.rootAgentLayers.Load()
	if root, ok := layers.projectRoots[realID]; !ok || root != repoPath {
		t.Fatalf("the verified checkout must join projectRoots under the identity it resolves to (%s at %s), got %q (present=%v)", realID, repoPath, root, ok)
	}
	if _, stale := layers.personal[staleID]; stale {
		t.Fatalf("the personal layer stayed under the stale identity %s while the project was published under %s — a global or legacy enable now starts a root the user disabled", staleID, realID)
	}
	if _, moved := layers.personal[realID]; !moved {
		t.Fatalf("the personal layer must arrive under the identity the project was published as (%s)", realID)
	}
}

// TestStartupReconciliationRetriesUntilItSucceeds pins review id 3916912922
// (P2). A project that resolves at startup never joins unresolvedRoots, so
// when its durable backfill fails — here because an unrelated unreadable record
// makes the registry's strict load fail — nothing was left for the heal pass to
// do, and healRootAgentLayers returned before reaching any retry. The promised
// backfill could then never succeed for the life of the daemon.
func TestStartupReconciliationRetriesUntilItSucceeds(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)

	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	corrupt := filepath.Join(dir, "prj_ffffffffffffffffffffffffffffffff")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatalf("mkdir corrupt record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "project.json"), []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("write corrupt record: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must start with the backfill having FAILED, got %q", recorded)
	}
	owed := manager.rootAgentLayers.Load().reconcileOwed
	if entry := owed[project.ID]; entry.repoID != realID || !entry.proven {
		t.Fatalf("a proven-but-unwritten identity must be retained as work, and retained as PROVEN so the retry does not re-derive it; owed=%v", owed)
	}

	// The registry is repaired, and the ensure cadence completes what the boot
	// promised.
	if err := os.RemoveAll(corrupt); err != nil {
		t.Fatalf("repair registry: %v", err)
	}
	manager.EnsureRootAgents()

	if recorded := onlyIdentityFor(t, project.ID); recorded != realID {
		t.Fatalf("the backfill must complete once the registry is readable again: recorded %q, want %s", recorded, realID)
	}
	if left := manager.rootAgentLayers.Load().reconcileOwed; len(left) != 0 {
		t.Fatalf("a completed reconciliation must drop its latch, got %v", left)
	}
}

// setRecordedRepoID writes a specific identity into a record, which is the only
// way to build a record whose written identity is STALE: every writer in config
// records the identity it just resolved.
func setRecordedRepoID(t *testing.T, projectID, repoID string) {
	t.Helper()
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	path := filepath.Join(dir, projectID, "project.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	record["repo_id"] = repoID
	out, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func onlyIdentityFor(t *testing.T, projectID string) string {
	t.Helper()
	projects, _, _, _, err := config.ListProjectsDetailed()
	if err != nil {
		t.Fatalf("ListProjectsDetailed: %v", err)
	}
	for _, p := range projects {
		if p.ID == projectID {
			return p.RepoID
		}
	}
	t.Fatalf("project %s is missing from the registry", projectID)
	return ""
}

// TestUnresolvedProjectStillSeesItsPathKeyedOptIn closes the same class as the
// delete sweep and the switcher row, found by auditing it rather than by
// review: the verdict's legacy lookup.
//
// A root_agents key is a PATH, and LegacyRootAgentForRepo falls back to hashing
// one it cannot resolve. Master matched an unresolved project's opt-in by
// accident, because it addressed such a project BY that hash; addressing it by
// the identity it RECORDED makes the lookup miss, and the repo then resolves to
// "disabled" instead of "enabled, but its recorded path did not resolve" — the
// misreport #3264 exists to prevent, and it hides an opt-in that has been in
// root_agents the whole time.
func TestUnresolvedProjectStillSeesItsPathKeyedOptIn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	worktree, project, realID := worktreeRecordedProject(t)
	if _, err := config.ReconcileProjectRepoID(project.ID, realID); err != nil {
		t.Fatalf("record the resolved identity: %v", err)
	}
	if realID == config.RepoIDFromRoot(filepath.Clean(worktree)) {
		t.Fatalf("fixture must use a recorded root that is not the repository's identity root, both %s", realID)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded workspace: %v", err)
	}

	// The opt-in is spelled as the recorded path — the only spelling that
	// survives the workspace.
	manager, err := NewManager(rootTestConfig(worktree, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, ok := manager.rootAgentLayers.Load().unresolvedRoots[realID]; !ok {
		t.Fatalf("fixture must file the record under its recorded identity %s", realID)
	}

	verdict := manager.rootAgentMaterializeVerdictFor(realID)
	if verdict.reason == rootAgentDisabled {
		t.Fatalf("the project's own root_agents opt-in, spelled as its recorded root, is invisible under the identity the record wrote down: the verdict says disabled where nothing disabled it")
	}
	// The legacy entry's per-tick retry is what covers this repo — it creates
	// the root the moment the recorded path returns — so "will materialize" is
	// the accurate answer, and it is the answer master gave for this shape
	// before the identity moved out from under the lookup.
	if verdict.reason != rootAgentWillMaterialize {
		t.Fatalf("an opt-in whose per-tick retry covers the repo must report that, got reason %v", verdict.reason)
	}
}

// TestOccupiedRecordedRootDoesNotBorrowItsOptIn pins review id 3917294309 (P2)
// — the limit of the lookup the test above added.
//
// Hashing the recorded path and asking the generic resolver matches an OCCUPANT
// main-rooted there, because that repository resolves to exactly that hash. The
// verdict then promised a root under the original project's identity that the
// legacy sweep will only ever create for the occupant.
func TestOccupiedRecordedRootDoesNotBorrowItsOptIn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	worktree, project, realID := worktreeRecordedProject(t)
	if _, err := config.ReconcileProjectRepoID(project.ID, realID); err != nil {
		t.Fatalf("record the resolved identity: %v", err)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded workspace: %v", err)
	}

	manager, err := NewManager(rootTestConfig(worktree, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, ok := manager.rootAgentLayers.Load().unresolvedRoots[realID]; !ok {
		t.Fatalf("fixture must file the record under its recorded identity %s", realID)
	}
	if got := manager.rootAgentMaterializeVerdictFor(realID).reason; got != rootAgentWillMaterialize {
		t.Fatalf("fixture precondition: with the path gone, the opt-in is this project's own; got reason %v", got)
	}

	// An unrelated repository takes the recorded path. Its own identity IS the
	// path's hash, so the opt-in now belongs to it — and the legacy sweep will
	// create a root for the occupant, never for this project.
	if err := exec.Command("git", "init", worktree).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}
	occupantID := repoID(t, worktree)
	if occupantID != config.RepoIDFromRoot(filepath.Clean(worktree)) {
		t.Fatalf("fixture must main-root the occupant at the recorded path, got %s", occupantID)
	}

	if got := manager.rootAgentMaterializeVerdictFor(realID).reason; got == rootAgentWillMaterialize {
		t.Fatalf("the occupant's opt-in was borrowed for %s: the verdict promises a root the legacy sweep will only ever create for %s, which admits root-targeted tasks and delivery waits for a root that never appears", realID, occupantID)
	}
}

// TestDeleteBySymlinkSpelledPathFindsTheRecord pins #3530 review id 3918120745.
//
// A delete request may spell an unavailable checkout through a symlinked
// ancestor while the record stores what registration resolved. Comparing those
// lexically misses the record, so the delete invents an identity, reports
// success, and leaves the real id's sessions and the durable registration
// untouched.
func TestDeleteBySymlinkSpelledPathFindsTheRecord(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	base := testguard.CanonicalTempDir(t)
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	repoPath := filepath.Join(real, "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	realID := repoID(t, repoPath)
	if project.RepoID != realID {
		t.Fatalf("fixture must record the resolved identity, got %q", project.RepoID)
	}
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	spelled := filepath.Join(link, "repo")
	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: spelled})
	if err != nil {
		t.Fatalf("DeleteProject by a symlink-spelled recorded path: %v", err)
	}
	if result.RepoID != realID {
		t.Fatalf("the delete invented %s for a path that names the record at %s: it reports success while the project's sessions and registration are untouched", result.RepoID, project.Root)
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr == nil {
		t.Fatalf("and the durable record must be removed")
	}
}

// TestDeleteBySymlinkSpelledPathFindsALegacyRecord pins #3530 review id
// 3918379019 — the same symlink match, for a row that has recorded no identity
// yet.
//
// Such a row was skipped before it could be matched, so the delete fell through
// to an id invented from the REQUEST's spelling. DeregisterProject cannot
// reconcile that with the stored one — two missing paths cannot be SameFile —
// so the durable registration survives a delete that reported success.
func TestDeleteBySymlinkSpelledPathFindsALegacyRecord(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	base := testguard.CanonicalTempDir(t)
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	repoPath := filepath.Join(real, "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	// A record written before Project.RepoID existed.
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	spelled := filepath.Join(link, "repo")
	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: spelled})
	if err != nil {
		t.Fatalf("DeleteProject by a symlink-spelled recorded path: %v", err)
	}
	if want := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath)); result.RepoID != want {
		t.Fatalf("a legacy row's provisional identity must come from its STORED root: got %s, want %s", result.RepoID, want)
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr == nil {
		t.Fatalf("the durable record survived a delete that reported success")
	}
}

// TestUnreadableMarkerKeepsTheCandidateRepoClosed pins #3530 review id
// 3918120753.
//
// The root-agent attribution gate released on any settled non-inconclusive
// probe, which includes an unreadable marker — a state that establishes nothing
// about whether the checkout at the recorded path is this project's. On master
// the invented-to-real bridge held the candidate repository closed through it;
// this change removes that bridge, so the gate itself has to carry the rule, or
// the candidate resolves from the global and legacy layers without ever seeing
// the project's personal disable.
func TestUnreadableMarkerKeepsTheCandidateRepoClosed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	worktree, project, realID := worktreeRecordedProject(t)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	// A record written before Project.RepoID: filed under an invented id while
	// its path does not resolve, which is an identity other than the one that
	// path resolves to.
	clearRecordedRepoID(t, project.ID)
	recordedID := config.DerivedRepoIDForUnresolvedRoot(worktree)
	if recordedID == realID {
		t.Fatalf("fixture must file the record under an identity other than the one its path resolves to")
	}
	aside := worktree + ".aside"
	if err := os.Rename(worktree, aside); err != nil {
		t.Fatalf("hide the workspace: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, ok := manager.rootAgentLayers.Load().unresolvedRoots[recordedID]; !ok {
		t.Fatalf("fixture must file the record under %s", recordedID)
	}
	// The workspace is back and its marker could not be read: the probe
	// resolved the repository (the candidate) and established nothing else.
	if err := os.Rename(aside, worktree); err != nil {
		t.Fatalf("restore the workspace: %v", err)
	}
	repo, err := config.RepoFromPath(worktree)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	probe := &rootReattributionProbe{done: make(chan struct{})}
	probe.candidate.Store(repo)
	unreadableMarkerProbe(probe, repo)
	manager.mu.Lock()
	manager.rootHealProbes[recordedID] = probe
	manager.mu.Unlock()

	if !manager.rootAttributionPendingFor(realID) {
		t.Fatalf("an unreadable marker establishes nothing, so repository %s must stay attribution-pending: its project's personal disable sits under %s where resolution cannot see it", realID, recordedID)
	}
}

// TestReconcileRetryIsPacedByTheHealBackoff pins #3530 review id 3918379041.
//
// A reconciliation that keeps failing — an unrelated corrupt record makes the
// registry's strict load fail — would otherwise reacquire the registry lock and
// reread every record on every poll tick. Re-attribution may run unconditionally
// because its pacing is per entry; this is a whole-registry read, so it belongs
// on the healer's backoff clock.
func TestReconcileRetryIsPacedByTheHealBackoff(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)

	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	corrupt := filepath.Join(dir, "prj_ffffffffffffffffffffffffffffffff")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatalf("mkdir corrupt record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "project.json"), []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("write corrupt record: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if len(manager.rootAgentLayers.Load().reconcileOwed) != 1 {
		t.Fatalf("fixture must latch the failed backfill")
	}

	// The first pass attempts it and fails, which moves the retry clock out.
	manager.EnsureRootAgents()
	manager.mu.Lock()
	next := manager.rootHealNextAttempt
	manager.mu.Unlock()
	if !next.After(nowFunc()) {
		t.Fatalf("a failed reconciliation must advance the healer's backoff, got next attempt %v", next)
	}

	// A second pass on the same tick must not touch the registry again. The
	// repair proves it: with the backoff still out, the retry does not run.
	if err := os.RemoveAll(corrupt); err != nil {
		t.Fatalf("repair registry: %v", err)
	}
	manager.EnsureRootAgents()
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("the retry ran inside its own backoff window: recorded %q", recorded)
	}
}

// TestUnansweredProbeWithholdsTheRecordedPathSweep pins #3530 review id
// 3918379027. The sweep supplies the recorded path's hash only while that path
// does not resolve — but a FAILED probe is not the same as an answered one. A
// killed or unstartable git says nothing about what occupies the path, and a
// repository that is there owns that hash, so acting on the failure alone would
// delete a live occupant's own opt-in on behalf of this project.
func TestUnansweredProbeWithholdsTheRecordedPathSweep(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	worktree, project, realID := worktreeRecordedProject(t)
	pathID := config.RepoIDFromRoot(filepath.Clean(worktree))
	if _, err := config.ReconcileProjectRepoID(project.ID, realID); err != nil {
		t.Fatalf("record the resolved identity: %v", err)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded workspace: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var swept []string
	original := deregisterRootAgents
	deregisterRootAgents = func(ids ...string) ([]string, error) {
		swept = append(swept, ids...)
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = original })

	// Every git from here on dies on a signal: nothing it is asked can be
	// said to have been answered.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	binDir := t.TempDir()
	shim := "#!/bin/sh\nkill -9 $$\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoID: realID}); err != nil {
		t.Fatalf("DeleteProject by the recorded identity: %v", err)
	}
	if len(swept) == 0 {
		t.Fatalf("fixture must reach the durable sweep, or it pins nothing")
	}
	for _, id := range swept {
		if id == pathID {
			t.Fatalf("the sweep supplied %s on an UNANSWERED probe: a repository occupying that path owns the id, so this deletes a live occupant's opt-in", pathID)
		}
	}
}

// TestUnprovenLegacyRowIsRetried pins #3530 review id 3918535472.
//
// A legacy row whose path RESOLVES never joins unresolvedRoots, so when its
// checkout-marker proof fails — a read that times out or cannot be read — there
// was nothing left for the heal pass to do and the row stayed without repo_id
// for the daemon's life. The proof failing is as unfinished as the write
// failing; it is latched UNPROVEN, so the retry re-establishes it rather than
// inheriting a claim about a checkout it never verified.
func TestUnprovenLegacyRowIsRetried(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)

	// The marker cannot be read at startup, so the proof fails while the path
	// itself resolves perfectly well.
	marker := checkoutMarkerPathForTest(t, repoPath)
	if err := os.Chmod(marker, 0o000); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must start with the proof having FAILED, got %q", recorded)
	}
	owed := manager.rootAgentLayers.Load().reconcileOwed
	entry, ok := owed[project.ID]
	if !ok || entry.proven {
		t.Fatalf("an unproven row must be retained as work, and retained as UNPROVEN so the retry re-derives the proof; owed=%v", owed)
	}

	// The marker becomes readable, and the ensure cadence finishes what the
	// boot could not.
	if err := os.Chmod(marker, 0o644); err != nil {
		t.Fatalf("restore marker: %v", err)
	}
	manager.EnsureRootAgents()

	if recorded := onlyIdentityFor(t, project.ID); recorded != realID {
		t.Fatalf("the identity must be recorded once its checkout can be verified: recorded %q, want %s", recorded, realID)
	}
	if left := manager.rootAgentLayers.Load().reconcileOwed; len(left) != 0 {
		t.Fatalf("a completed reconciliation must drop its latch, got %v", left)
	}
}

// checkoutMarkerPathForTest finds the one checkout marker under a repo's git
// directory. The registry writes it there; tests that need to make identity
// UNPROVABLE break exactly that file.
func checkoutMarkerPathForTest(t *testing.T, repoPath string) string {
	t.Helper()
	candidates, err := filepath.Glob(filepath.Join(repoPath, ".git", "agent-factory", "checkout-id-*"))
	if err != nil {
		t.Fatalf("glob markers: %v", err)
	}
	var markers []string
	for _, candidate := range candidates {
		if !strings.HasSuffix(candidate, ".lock") {
			markers = append(markers, candidate)
		}
	}
	if len(markers) != 1 {
		t.Fatalf("expected exactly one checkout marker, got %v", candidates)
	}
	return markers[0]
}

// TestProvisionalDeleteRefusesWhileSessionsRemainUnderTheOldHash pins #3530
// review id 3918535480.
//
// A provisional identity reaches nothing, which is the design — but the row it
// would still deregister may have live sessions filed under the historical hash
// of its recorded path. Deregistering while archiving nothing leaves them
// orphaned behind a success, and which project that hash names cannot be
// established from here.
func TestProvisionalDeleteRefusesWhileSessionsRemainUnderTheOldHash(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	// A repository with no enclosing one, so a removed checkout resolves to
	// nothing rather than upward into a parent.
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	historical := config.RepoIDFromRoot(filepath.Clean(repoPath))

	// The checkout disappears BEFORE the daemon starts, which is what leaves
	// the row unreconciled: a path that resolves at startup is backfilled
	// there and then, and the state this finding is about never arises.
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	// A live session filed under the identity the recorded path used to have,
	// which is how every session created before the upgrade is keyed.
	inst, err := session.NewInstance(session.InstanceOptions{Title: "left-behind", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(historical, "left-behind")] = inst
	manager.mu.Unlock()

	_, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	err = delErr
	if err == nil {
		t.Fatalf("a delete that can archive nothing must not deregister the row out from under live sessions")
	}
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("the refusal must state that nothing was mutated: %v", err)
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("and the record must survive it: %v", statErr)
	}
}

// TestReappearingCheckoutRefusesTheTransitioningDelete pins #3530 review id
// 3917929613.
//
// The transition gate opens when the recorded root is provably gone, because
// no verdict about a checkout there can then arrive. But the claimant scan runs
// after it and READS that path: a checkout reappearing in between would be
// authorized by a marker match while the gate had already opened on the earlier
// absence, so a real-to-real transition archives only the recorded id's
// sessions and removes the row while the candidate id's remain.
func TestReappearingCheckoutRefusesTheTransitioningDelete(t *testing.T) {
	manager, repoPath, derivedID, realID := attributionFixture(t, verifiedProbe, rootGone)

	// A checkout comes back at the recorded path while the claimant scan is
	// reading it.
	restored := false
	deleteProjectPostClaimantHookForTest = func() {
		if restored {
			return
		}
		restored = true
		if err := exec.Command("git", "init", repoPath).Run(); err != nil {
			t.Fatalf("restore a checkout mid-delete: %v", err)
		}
	}
	t.Cleanup(func() { deleteProjectPostClaimantHookForTest = nil })

	_, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if !restored {
		t.Fatalf("fixture never reached the claimant scan, so it pins nothing")
	}
	if err == nil {
		t.Fatalf("a checkout that reappeared during the claimant scan makes the identity ambiguous again: the delete must refuse rather than act on the absence it observed earlier")
	}
	manager.mu.Lock()
	_, suppressedReal := manager.deletedRootRepos[realID]
	_, suppressedProvisional := manager.deletedRootRepos[derivedID]
	manager.mu.Unlock()
	if suppressedReal || suppressedProvisional {
		t.Fatalf("nothing may be suppressed by a refused delete")
	}
}

// TestContestedIdentityDefersThePromotion pins #3530 review id 3918535470.
//
// Real-to-real promotion moves the state filed under the identity being left
// behind — but the snapshot's maps are keyed by repo id, so if ANOTHER live
// project legitimately holds that id, the entry being moved may be its policy
// rather than this project's. Two real identities at one id is the case this
// change does not address (#3611), so the promotion defers rather than guess.
func TestContestedIdentityDefersThePromotion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	// An ordinary registered project, live and resolving: it owns its id.
	occupantPath := setupControlRepo(t)
	occupant := registerTestProject(t, occupantPath)
	writePersonalRootAgent(t, occupant.ID, "enabled = false")
	contestedID := repoID(t, occupantPath)

	// And a second project whose record is filed under that SAME id while its
	// own checkout resolves to a different one.
	worktree, project, realID := worktreeRecordedProject(t)
	if realID == contestedID {
		t.Fatalf("fixture must use two distinct identities")
	}
	setRecordedRepoID(t, project.ID, contestedID)
	aside := worktree + ".aside"
	if err := os.Rename(worktree, aside); err != nil {
		t.Fatalf("hide the workspace: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	layers := manager.rootAgentLayers.Load()
	if _, ok := layers.unresolvedRoots[contestedID]; !ok {
		t.Fatalf("fixture must file the second record under %s", contestedID)
	}
	if _, ok := layers.projectRoots[contestedID]; !ok {
		t.Fatalf("fixture must have the occupant holding %s in projectRoots", contestedID)
	}

	// The second project's workspace returns and verifies.
	if err := os.Rename(aside, worktree); err != nil {
		t.Fatalf("restore the workspace: %v", err)
	}
	manager.EnsureRootAgents()

	after := manager.rootAgentLayers.Load()
	if _, still := after.unresolvedRoots[contestedID]; !still {
		t.Fatalf("a contested identity must leave the record unresolved rather than move state that may not be its own")
	}
	if root, published := after.projectRoots[realID]; published {
		t.Fatalf("and nothing may be published under %s (root %q) on the strength of that move", realID, root)
	}
	if _, moved := after.personal[realID]; moved {
		t.Fatalf("the occupant's personal layer must not be handed to %s", realID)
	}
	if _, kept := after.personal[contestedID]; !kept {
		t.Fatalf("and it must stay where the occupant's own resolution finds it")
	}
}
