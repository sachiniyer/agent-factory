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
	if _, err := config.ReconcileProjectRepoID(project.ID, realID, nil); err != nil {
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
	if _, err := config.ReconcileProjectRepoID(project.ID, realID, nil); err != nil {
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

// TestUnprovenLatchSurvivesAnUnreadableRegistry pins #3530 review id
// 3919195000. A failed registry LIST is not evidence that a project is gone;
// dropping unproven entries on it leaves the healer with no work for a path
// that resolves, so repairing the registry could never complete the proof for
// the rest of the daemon run — the very defect the latch exists to prevent.
func TestUnprovenLatchSurvivesAnUnreadableRegistry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)

	marker := checkoutMarkerPathForTest(t, repoPath)
	if err := os.Chmod(marker, 0o000); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, ok := manager.rootAgentLayers.Load().reconcileOwed[project.ID]; !ok {
		t.Fatalf("fixture must latch the unproven row")
	}

	// The marker becomes readable, but an unrelated record cannot be read, so
	// the retry's registry list fails.
	if err := os.Chmod(marker, 0o644); err != nil {
		t.Fatalf("restore marker: %v", err)
	}
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
	manager.EnsureRootAgents()

	if _, kept := manager.rootAgentLayers.Load().reconcileOwed[project.ID]; !kept {
		t.Fatalf("an unreadable registry must not drop the unproven latch: the row resolves, so nothing else would ever retry it")
	}

	// Repaired, and the next due pass completes it.
	if err := os.RemoveAll(corrupt); err != nil {
		t.Fatalf("repair registry: %v", err)
	}
	manager.mu.Lock()
	manager.rootHealNextAttempt = nowFunc()
	manager.mu.Unlock()
	manager.EnsureRootAgents()

	if recorded := onlyIdentityFor(t, project.ID); recorded != realID {
		t.Fatalf("the retained latch must complete once the registry reads again: recorded %q, want %s", recorded, realID)
	}
}

// TestReprovedIdentityCarriesTheProjectWithIt pins #3530 review id 3919346220.
//
// A legacy row can resolve to one identity at boot and be PROVEN under another
// on retry — the same marked checkout, its repository's identity root moved.
// Dropping the latch there left the snapshot keyed under the identity the boot
// resolved with nothing to re-derive the new one, so a legacy opt-in resolving
// it could start without the project's personal disable.
func TestReprovedIdentityCarriesTheProjectWithIt(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	realID := repoID(t, repoPath)

	marker := checkoutMarkerPathForTest(t, repoPath)
	if err := os.Chmod(marker, 0o000); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	owed := manager.rootAgentLayers.Load().reconcileOwed[project.ID]
	if owed.proven || owed.repoID != realID {
		t.Fatalf("fixture must latch the row UNPROVEN under the identity the boot resolved, got %+v", owed)
	}

	// The boot's identity is rewritten to a stale one, so the retry's proof
	// names a different id than the latch carries — the shape a moved identity
	// root produces.
	stale := config.RepoIDFromRoot(filepath.Join(testguard.CanonicalTempDir(t), "former-identity-root"))
	healed := *manager.rootAgentLayers.Load()
	healed.reconcileOwed = map[string]reconcileOwedEntry{project.ID: {repoID: stale}}
	healed.projectRoots = cloneStringMap(healed.projectRoots)
	healed.personal = cloneLayerMap(healed.personal)
	delete(healed.projectRoots, realID)
	healed.projectRoots[stale] = repoPath
	if layer, ok := healed.personal[realID]; ok {
		healed.personal[stale] = layer
		delete(healed.personal, realID)
	}
	manager.rootAgentLayers.Store(&healed)
	if err := os.Chmod(marker, 0o644); err != nil {
		t.Fatalf("restore marker: %v", err)
	}

	manager.EnsureRootAgents()

	after := manager.rootAgentLayers.Load()
	if _, stillStale := after.projectRoots[stale]; stillStale {
		t.Fatalf("the project must not stay published under the identity the boot resolved once its checkout is proven under another")
	}
	if root, ok := after.projectRoots[realID]; !ok || root != repoPath {
		t.Fatalf("it must be published under the identity just proven (%s at %s), got %q (present=%v)", realID, repoPath, root, ok)
	}
	if _, moved := after.personal[realID]; !moved {
		t.Fatalf("and its personal layer must arrive with it, or a legacy opt-in starts the root the user disabled")
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != realID {
		t.Fatalf("the identity just proven must also be recorded: got %q", recorded)
	}
}

// TestStartupProofUnderANewIdentityPublishesIt pins #3530 review id 3919604357.
//
// The startup switch handled a proof that AGREED and a proof that failed, and
// dropped the one that named a different identity — the same MARKED checkout
// whose repository's common directory moved between the resolution and the
// proof. The project was then published under the stale identity with nothing
// latched, and a resolved project never enters unresolvedRoots, so no pass
// would ever revisit it.
func TestStartupProofUnderANewIdentityPublishesIt(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	base := testguard.CanonicalTempDir(t)
	source := filepath.Join(base, "source")
	if err := exec.Command("git", "init", source).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	for _, args := range [][]string{
		{"-C", source, "config", "user.email", "t@t"},
		{"-C", source, "config", "user.name", "t"},
		{"-C", source, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	bareA := filepath.Join(base, "A.git")
	bareB := filepath.Join(base, "B.git")
	for _, bare := range []string{bareA, bareB} {
		if err := exec.Command("git", "clone", "--quiet", "--bare", source, bare).Run(); err != nil {
			t.Fatalf("git clone --bare: %v", err)
		}
	}
	workspace := filepath.Join(base, "workspace")
	if err := exec.Command("git", "-C", bareA, "worktree", "add", "--detach", workspace).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	project := registerTestProject(t, workspace)
	clearRecordedRepoID(t, project.ID)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	idA, idB := config.RepoIDFromRoot(bareA), config.RepoIDFromRoot(bareB)
	if idA == idB {
		t.Fatalf("fixture needs two distinct bare identities")
	}

	// Between the resolution and the proof, the SAME marked checkout comes to
	// resolve under B: its common directory moved, and the marker moved with
	// it — which is what makes the proof succeed while naming another identity.
	moved := false
	config.SetRegisteredProjectProofRaceHookForTest(t, func() {
		if moved {
			return
		}
		moved = true
		markers, err := filepath.Glob(filepath.Join(bareA, "agent-factory", "checkout-id-*"))
		if err != nil || len(markers) == 0 {
			t.Fatalf("find marker under %s: %v (%v)", bareA, err, markers)
		}
		data, err := os.ReadFile(markers[0])
		if err != nil {
			t.Fatalf("read marker: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(bareB, "agent-factory"), 0o755); err != nil {
			t.Fatalf("mkdir marker dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(bareB, "agent-factory", filepath.Base(markers[0])), data, 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		if err := exec.Command("git", "-C", bareA, "worktree", "remove", "--force", workspace).Run(); err != nil {
			t.Fatalf("detach the workspace from A: %v", err)
		}
		if err := exec.Command("git", "-C", bareB, "worktree", "add", "--detach", workspace).Run(); err != nil {
			t.Fatalf("attach the workspace to B: %v", err)
		}
	})

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !moved {
		t.Fatalf("fixture never reached the proof, so it pins nothing")
	}
	layers := manager.rootAgentLayers.Load()
	if _, stale := layers.projectRoots[idA]; stale {
		t.Fatalf("the project must not be published under the identity its resolution named (%s) once the proof names another", idA)
	}
	if root, ok := layers.projectRoots[idB]; !ok || root != workspace {
		t.Fatalf("it must be published under the identity its checkout PROVES (%s at %s), got %q (present=%v)", idB, workspace, root, ok)
	}
	if _, ok := layers.personal[idB]; !ok {
		t.Fatalf("its personal layer must be filed there too, or a legacy opt-in starts the root the user disabled")
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != idB {
		t.Fatalf("and the proven identity must be recorded: got %q, want %s", recorded, idB)
	}
}

// TestDestinationFenceDefersThePromotion pins #3530 review id 3919604386.
//
// A delete aimed at the candidate identity installs its fence there and
// resolves no registry row — the row still answers to the identity it is filed
// under. A promotion that checked only the SOURCE fence would move the durable
// project into the identity being torn down: archived and suppressed, never
// deregistered, and back after a restart.
func TestDestinationFenceDefersThePromotion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	writePersonalRootAgent(t, project.ID, "enabled = true")
	realID := repoID(t, repoPath)
	derivedID := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))

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

	// A delete holds the DESTINATION identity, not the one the record is filed
	// under.
	manager.mu.Lock()
	if manager.projectDeletes == nil {
		manager.projectDeletes = make(map[string]struct{})
	}
	manager.projectDeletes[realID] = struct{}{}
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	// The DURABLE write is what this pins beyond the in-memory deferral
	// (#3530 review id 3919824579): a delete holding the destination resolved
	// no registry row, so it cannot deregister a row that gains that identity
	// here — the project would come back on the next start with its sessions
	// archived and its identity suppressed.
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("nothing may be written to the record while a delete holds the identity it would gain, got %q", recorded)
	}
	layers := manager.rootAgentLayers.Load()
	if _, still := layers.unresolvedRoots[derivedID]; !still {
		t.Fatalf("a promotion into an identity a delete holds must be deferred, leaving the record where it is")
	}
	if root, published := layers.projectRoots[realID]; published {
		t.Fatalf("and nothing may be published under %s (root %q) while that delete runs", realID, root)
	}
	manager.mu.Lock()
	probe := manager.rootHealProbes[derivedID]
	manager.mu.Unlock()
	if probe == nil {
		t.Fatalf("the probe must be kept for the pass that completes the transition")
	}
}

// TestSuccessfulProofSurvivesAFailedWrite pins #3530 review id 3919604378.
//
// A retry that PROVES the checkout and then fails to write recorded that proof
// only in its local map, and returned without publishing when nothing else
// changed — so the next pass reverted to the unproven latch. If the checkout
// disappears after the proof but before the write recovers, every later
// re-proof fails and the established identity is never written.
func TestSuccessfulProofSurvivesAFailedWrite(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)

	marker := checkoutMarkerPathForTest(t, repoPath)
	if err := os.Chmod(marker, 0o000); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if entry := manager.rootAgentLayers.Load().reconcileOwed[project.ID]; entry.proven {
		t.Fatalf("fixture must start UNPROVEN")
	}

	// The marker becomes readable, so the proof succeeds — while the record's
	// own directory is read-only, so only the WRITE fails. (A corrupt sibling
	// record would fail the registry LIST too, which is a different branch:
	// see TestUnprovenLatchSurvivesAnUnreadableRegistry.)
	if err := os.Chmod(marker, 0o644); err != nil {
		t.Fatalf("restore marker: %v", err)
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	recordDir := filepath.Join(dir, project.ID)
	if err := os.Chmod(recordDir, 0o555); err != nil {
		t.Fatalf("chmod record dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(recordDir, 0o755) })
	if err := os.WriteFile(filepath.Join(recordDir, "probe"), nil, 0o644); err == nil {
		t.Skip("this test needs an unwritable directory to be unwritable; running as a user that ignores it")
	}
	manager.EnsureRootAgents()

	entry, kept := manager.rootAgentLayers.Load().reconcileOwed[project.ID]
	if !kept {
		t.Fatalf("the latch must survive a failed write")
	}
	if !entry.proven || entry.repoID != realID {
		t.Fatalf("and must carry the proof it established, so a checkout that disappears afterwards cannot make it unwritable: got %+v", entry)
	}
}

// TestDeclinedWriteLeavesTheRecordUnresolved pins #3530 review id 3920258558.
//
// A write DECLINED under the registry lock and an identity that was already
// recorded both report "did not write". Treating them alike published the
// transition whose durable half never happened — and if the delete cleared its
// fence in between, the promotion succeeded too, retiring the probe and leaving
// the record to revert to its provisional identity on the next start.
func TestDeclinedWriteLeavesTheRecordUnresolved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)
	derivedID := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))

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

	// A delete holds the identity the write would record, and clears its fence
	// the moment the write has been declined — the interleaving that made a
	// decline indistinguishable from a completed write.
	manager.mu.Lock()
	if manager.projectDeletes == nil {
		manager.projectDeletes = make(map[string]struct{})
	}
	manager.projectDeletes[realID] = struct{}{}
	manager.mu.Unlock()
	config.SetIdentityWriteDeclinedHookForTest(t, func() {
		manager.mu.Lock()
		delete(manager.projectDeletes, realID)
		manager.mu.Unlock()
	})

	manager.EnsureRootAgents()

	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("the write was declined, so nothing may reach the record, got %q", recorded)
	}
	layers := manager.rootAgentLayers.Load()
	if _, still := layers.unresolvedRoots[derivedID]; !still {
		t.Fatalf("and the record must stay unresolved: publishing %s without the durable half reverts on the next start", realID)
	}
	if _, published := layers.projectRoots[realID]; published {
		t.Fatalf("nothing may be published under %s on the strength of a write that did not happen", realID)
	}
}

// TestRegistryRecoveryReconciliationRespectsTheFence pins #3530 review id
// 3920258554.
//
// projectRootAgentLayers is the BOOT builder and also the registry-recovery
// rebuild, and only the first is safe with no fence: at boot nothing is serving,
// so no delete can hold an identity. Recovery runs while the daemon serves, and
// passing nil there let a proof land a durable write into an identity a delete
// was holding.
func TestRegistryRecoveryReconciliationRespectsTheFence(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)

	// The registry is unreadable at boot, which is the state whose recovery
	// re-enters the builder at runtime.
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod registry: %v", err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			_ = os.Chmod(dir, 0o755)
		}
	})
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !manager.rootAgentLayers.Load().registryUnreadable {
		t.Skip("this test needs an unreadable registry to be unreadable; running as a user that ignores it")
	}

	// A delete holds the identity the recovery would record.
	manager.mu.Lock()
	if manager.projectDeletes == nil {
		manager.projectDeletes = make(map[string]struct{})
	}
	manager.projectDeletes[realID] = struct{}{}
	manager.mu.Unlock()

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("repair registry: %v", err)
	}
	restored = true
	// Recovery publishes only on a second consecutive matching read, so the
	// cadence is driven twice.
	for range 4 {
		manager.mu.Lock()
		manager.rootHealNextAttempt = nowFunc()
		manager.mu.Unlock()
		manager.EnsureRootAgents()
	}

	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("the recovery rebuild must respect the delete fence: it recorded %q into an identity a delete is holding", recorded)
	}
}
