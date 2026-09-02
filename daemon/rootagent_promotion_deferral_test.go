package daemon

import (
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

// TestProvisionalDeleteFollowsPendingAttribution pins review id 3915722493
// (P1).
//
// A legacy record's returning checkout is verified before the heal pass has
// written the promotion down. A path that goes away again in that window leaves
// the delete normalizing to the invented id while the daemon already holds the
// real one — so the delete archives nothing under the identity this project's
// sessions are keyed by, deregisters the project, and reports success.
func TestProvisionalDeleteFollowsPendingAttribution(t *testing.T) {
	manager, repoPath, _, realID := pendingAttributionFixture(t, verifiedProbe)

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("DeleteProject by the recorded path: %v", err)
	}
	assertDeletedUnderRealIdentity(t, manager, result, realID)
}

// TestProvisionalDeleteWithBothSelectorsFollowsPendingAttribution is the same
// finding through the shape the TUI actually sends: RepoPath plus the RepoID it
// is displaying, which for an unresolved legacy project IS the provisional one.
//
// It also pins where the redirect may happen. Applied to either selector before
// they are checked against each other, this delete becomes a mismatch refusal —
// the caller's provisional id against a path redirected to the real one —
// which would turn a working delete into an error for the whole pending window.
func TestProvisionalDeleteWithBothSelectorsFollowsPendingAttribution(t *testing.T) {
	manager, repoPath, derivedID, realID := pendingAttributionFixture(t, verifiedProbe)

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath, RepoID: derivedID})
	if err != nil {
		t.Fatalf("DeleteProject by the recorded path and the identity the TUI is displaying: %v", err)
	}
	assertDeletedUnderRealIdentity(t, manager, result, realID)
}

// TestProvisionalDeleteRefusesAnUnverifiedCandidate pins review id 3916379586
// (P1) — the limit of the redirect above.
//
// A probe publishes its candidate the moment git resolves the recorded path,
// BEFORE reading the checkout marker, so a stranger occupying that path
// publishes its own real identity there. Following it would archive and
// suppress the occupant while deregistering the original project's record, and
// a mismatch arriving afterwards cannot undo either. Unknown is a state, not a
// "no": the delete refuses and changes nothing.
func TestProvisionalDeleteRefusesAnUnverifiedCandidate(t *testing.T) {
	manager, repoPath, derivedID, realID := pendingAttributionFixture(t, inFlightProbe)

	_, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err == nil {
		t.Fatalf("a delete whose provisional target has an UNVERIFIED candidate must refuse: the candidate may be a stranger at the recorded path, and archiving its sessions cannot be undone by the verdict that follows")
	}
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("the refusal must state that nothing was mutated: %v", err)
	}
	manager.mu.Lock()
	_, suppressedReal := manager.deletedRootRepos[realID]
	_, suppressedProvisional := manager.deletedRootRepos[derivedID]
	manager.mu.Unlock()
	if suppressedReal || suppressedProvisional {
		t.Fatalf("nothing may be suppressed by a refused delete (real=%v provisional=%v)", suppressedReal, suppressedProvisional)
	}
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

// TestVerifiedPendingAttributionDeregistersByRealID pins review id 3916379577
// (P1), the reverse direction. A client deleting by the REAL identity found no
// registry row — the record still answers to its provisional id — so the delete
// archived and suppressed the real id's sessions, skipped DeregisterProject,
// and reported success while the durable record survived to reappear on the
// next daemon start.
func TestVerifiedPendingAttributionDeregistersByRealID(t *testing.T) {
	manager, _, _, realID := pendingAttributionFixture(t, verifiedProbe)
	project := onlyRegisteredProject(t)

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoID: realID}); err != nil {
		t.Fatalf("DeleteProject by the real identity: %v", err)
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr == nil {
		t.Fatalf("the durable record survived a delete that reported success — it reappears on the next daemon start; the row is still keyed by its provisional identity and only a verified probe binds the two")
	}
}

// TestRedirectedDeleteSweepsTheRecordedPathOptIn pins review id 3916379565
// (P1). A root_agents key is a PATH, and a stale key for an unavailable
// recorded root falls back to hashing that path — the project's provisional
// identity, never the real id a verified probe redirects the delete to. Keying
// the durable sweep on the target alone left the opt-in in place to recreate
// the root when the checkout returned.
//
// The recorded root is a linked worktree, which is where the two genuinely
// differ: the repository's identity hashes the main root while the stale key
// hashes the worktree path.
func TestRedirectedDeleteSweepsTheRecordedPathOptIn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
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
	worktree := filepath.Join(parent, "wt")
	if err := exec.Command("git", "-C", repoPath, "worktree", "add", worktree).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	project := registerTestProject(t, repoPath)
	rewriteRecordRootForDeferral(t, project.ID, worktree)
	clearRecordedRepoID(t, project.ID)

	realID := repoID(t, worktree)
	derivedID := config.DerivedRepoIDForUnresolvedRoot(worktree)
	pathID := config.RepoIDFromRoot(filepath.Clean(worktree))
	if pathID == realID {
		t.Fatalf("fixture must use a recorded root that is not the repository's identity root, both %s", realID)
	}
	worktreeRepo, err := config.RepoFromPath(worktree)
	if err != nil {
		t.Fatalf("RepoFromPath(%s): %v", worktree, err)
	}

	aside := worktree + ".aside"
	if err := os.Rename(worktree, aside); err != nil {
		t.Fatalf("hide worktree: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, ok := manager.rootAgentLayers.Load().unresolvedRoots[derivedID]; !ok {
		t.Fatalf("fixture must start with the record unresolved under its provisional identity %s", derivedID)
	}
	installVerifiedProbe(t, manager, derivedID, worktreeRepo)

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
		t.Fatalf("the verified probe must move the delete onto %s, got %s", realID, result.RepoID)
	}
	found := false
	for _, id := range swept {
		if id == pathID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the durable root_agents sweep got %v, without the recorded path's own identity %s — a stale key spelled %q resolves to nothing else, so the opt-in survives to recreate the root when the checkout returns", swept, pathID, worktree)
	}
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

func assertDeletedUnderRealIdentity(t *testing.T, manager *Manager, result DeleteProjectResult, realID string) {
	t.Helper()
	if result.RepoID != realID {
		t.Fatalf("the delete stayed on the provisional identity %s while the daemon had already verified %s for this record — it archives nothing under the identity the project's sessions are keyed by and still deregisters the project", result.RepoID, realID)
	}
	manager.mu.Lock()
	_, suppressedReal := manager.deletedRootRepos[realID]
	manager.mu.Unlock()
	if !suppressedReal {
		t.Fatalf("the delete must suppress the identity it is actually deleting (%s)", realID)
	}
}
