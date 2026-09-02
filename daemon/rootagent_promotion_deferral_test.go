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
func pendingAttributionFixture(t *testing.T, finish probeFinisher) (manager *Manager, repoPath, derivedID, realID string) {
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

	// …and the path goes away again before the delete arrives, so
	// normalization sees an unresolvable path and a record with no identity.
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir again: %v", err)
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
	if owed[project.ID] != realID {
		t.Fatalf("a proven-but-unwritten identity must be retained as work; owed=%v", owed)
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
