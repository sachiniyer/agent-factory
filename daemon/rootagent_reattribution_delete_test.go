package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// DeleteProject's interactions with #3299 re-attribution: identity
// translation, tombstone/alias suppression, fences, and probe lifecycles.
// Fixtures (rewriteRecordRoot, setupBareCloneWorktree) live in
// rootagent_reattribution_test.go.

// TestDisproofReleasesSamePathTombstone pins the #3299 review's round-15 P2:
// a main-root project deleted while its path was absent tombstones the
// derived ID — which IS any later occupant's real ID. Once the occupant
// PROVES to be a different clone than the deleted claimant, the tombstone
// must release, or the occupant's legitimate legacy opt-in is suppressed for
// the daemon's lifetime by a dead third party.
func TestDisproofReleasesSamePathTombstone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The project is deleted while its path is absent…
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject while absent: %v", err)
	}
	// …and an unrelated clone occupies the path; the legacy entry is ITS
	// opt-in (in-memory: the delete's durable sweep already ran).
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}

	manager.ensureRootAgentsAndWait()
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("the proven-different occupant's legacy opt-in must be released from the dead claimant's tombstone, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got == rootAgentProjectDeleted {
		t.Fatalf("the occupant's verdict must not report a dead third party's deletion")
	}
}

// TestOccupantDeleteKeepsItsOwnTombstone pins review finding 3908517190 (P1).
// A path-only delete of the checkout now occupying a main-root project's old
// path matched that stale registry row by PATHNAME and recorded the OLD
// project as the tombstone's claimant. rootDeletionTombstoneApplies then read
// the record's own identityMismatch as disproof of the OCCUPANT's tombstone
// and released it, so the immutable in-memory legacy entry recreated the root
// this delete had just torn down.
//
// No legacy root_agents entry here on purpose: with one, the ensure pass
// creates a root for the occupant before the delete, and the delete then fails
// tearing it down through the recording backend — which would fail this test
// for a reason that has nothing to do with the tombstone.
func TestOccupantDeleteKeepsItsOwnTombstone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// An unrelated clone takes the path, and the probe proves the mismatch.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}
	manager.ensureRootAgentsAndWait()

	occupantID := repoID(t, repoPath)
	record, ok := manager.rootAgentLayers.Load().unresolvedRoots[occupantID]
	if !ok || !record.identityMismatch || record.projectID != project.ID {
		t.Fatalf("fixture must record a PROVEN mismatch at %s owned by %s, got %+v (present=%v)", occupantID, project.ID, record, ok)
	}

	// Now delete the OCCUPANT by its own path.
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject of the occupant: %v", err)
	}

	manager.mu.Lock()
	claimant, tombstoned := manager.deletedRootRepos[occupantID]
	applies := manager.rootDeletionTombstoneApplies(manager.rootAgentLayers.Load(), occupantID)
	manager.mu.Unlock()
	if !tombstoned {
		t.Fatalf("the occupant's delete must tombstone its own identity %s", occupantID)
	}
	if claimant == project.ID {
		t.Fatalf("the tombstone must not claim the disproven record's project %s — that same mismatch is what would release it again", project.ID)
	}
	if !applies {
		t.Fatalf("the occupant's own deletion must keep suppressing it; a delete that undoes itself lets the in-memory legacy entry recreate the root")
	}
}

// TestUnprovenOccupantIsNotClaimed pins review finding 3908667983 (P1). The
// previous guard excluded only a mismatch the snapshot had ALREADY published,
// which is absence of evidence standing in for evidence: an unrelated clone
// path-deleted before its probe reports anything still claimed the stale row,
// and the retained probe's later identityMismatch then read as disproof of the
// tombstone this delete had just installed.
func TestUnprovenOccupantIsNotClaimed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The occupant appears and is deleted IMMEDIATELY — no ensure pass has
	// run, so the snapshot carries no mismatch for it yet. That is the window.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}
	occupantID := repoID(t, repoPath)
	if record, ok := manager.rootAgentLayers.Load().unresolvedRoots[occupantID]; ok && record.identityMismatch {
		t.Fatalf("fixture must delete BEFORE any mismatch is published, got %+v", record)
	}

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject of the fresh occupant: %v", err)
	}

	manager.mu.Lock()
	claimant := manager.deletedRootRepos[occupantID]
	manager.mu.Unlock()
	if claimant == project.ID {
		t.Fatalf("an unproven occupant must not claim the stale record %s — its later mismatch would then release this very tombstone", project.ID)
	}

	// Let the probe publish the mismatch it was always going to publish, and
	// confirm the occupant's own deletion survives it.
	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	applies := manager.rootDeletionTombstoneApplies(manager.rootAgentLayers.Load(), occupantID)
	manager.mu.Unlock()
	if !applies {
		t.Fatalf("the occupant's deletion must survive the stale record's mismatch; otherwise the in-memory legacy entry recreates the root the delete removed")
	}
}

// TestUnprovenOccupantDeleteKeepsTheStaleRecord pins review finding 3910519845
// (P1). A path delete of an unrelated clone at an unresolved project's old path
// matches that stale registry row by PATHNAME. The claimant is correctly
// refused — nothing proves ownership — but repoPath still carried the row's
// path into DeregisterProject, which removed the ORIGINAL project's registry
// directory and its personal configuration on behalf of a delete that never
// targeted it.
func TestUnprovenOccupantDeleteKeepsTheStaleRecord(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// An unrelated clone takes the path, and is deleted by that path.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}
	occupantID := repoID(t, repoPath)

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject of the occupant: %v", err)
	}

	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("deleting an unproven occupant must not destroy the original project's registry record and personal config: %v", statErr)
	}
	manager.mu.Lock()
	_, tombstoned := manager.deletedRootRepos[occupantID]
	manager.mu.Unlock()
	if !tombstoned {
		t.Fatalf("the occupant's own deletion must still take effect at %s", occupantID)
	}
}

// TestDeleteByUnresolvablePathAddressesTheRecordedProject closes #3363 and is
// the behavioural half of #3530.
//
// A delete whose recorded path no longer resolves used to hash that path — so
// it addressed whatever repository was there, which for a reused path is a
// stranger, and for an empty path was a value that happened to equal the
// project's own id only by coincidence. It now addresses the identity the
// project WROTE DOWN when it last resolved, which is the only thing that can
// still be known once the path is gone.
func TestDeleteByUnresolvablePathAddressesTheRecordedProject(t *testing.T) {
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
	// Recorded at a linked worktree: the recorded path and the repository
	// identity differ, which is exactly where hashing the path went wrong.
	worktree := filepath.Join(parent, "wt")
	if err := exec.Command("git", "-C", repoPath, "worktree", "add", worktree).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	project := registerTestProject(t, repoPath)
	realID := repoID(t, repoPath)
	rewriteRecordRootForDeferral(t, project.ID, worktree)
	if _, err := config.ReconcileProjectRepoID(project.ID, realID, nil); err != nil {
		t.Fatalf("record the resolved identity: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The recorded path stops resolving, and an UNRELATED repository takes it.
	if err := exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktree).Run(); err != nil {
		t.Fatalf("git worktree remove: %v", err)
	}
	if err := exec.Command("git", "init", worktree).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}
	occupantID := repoID(t, worktree)
	if occupantID == realID {
		t.Fatalf("fixture must make the occupant a DIFFERENT repository, both %s", realID)
	}

	// Delete by the recorded path while it is occupied by the stranger.
	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if err != nil {
		t.Fatalf("DeleteProject by recorded path: %v", err)
	}
	if result.RepoID != occupantID {
		t.Fatalf("a path that RESOLVES addresses the repository actually there (%s), got %s", occupantID, result.RepoID)
	}
	manager.mu.Lock()
	_, projectSuppressed := manager.deletedRootRepos[realID]
	manager.mu.Unlock()
	if projectSuppressed {
		t.Fatalf("deleting the occupant must not reach the recorded project's identity %s", realID)
	}

	// Now the path is gone entirely: the delete must still reach the recorded
	// project, through the identity it wrote down.
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove path: %v", err)
	}
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree}); err != nil {
		t.Fatalf("DeleteProject by an unresolvable recorded path: %v", err)
	}
	manager.mu.Lock()
	_, nowSuppressed := manager.deletedRootRepos[realID]
	manager.mu.Unlock()
	if !nowSuppressed {
		t.Fatalf("a delete by an unresolvable recorded path must address the recorded project's own identity %s — that is what the written-down id is for (#3363)", realID)
	}
}

// TestInventedIDReachesNothing is what each of the seven collision guards on
// PR #3334 becomes once the namespace is disjoint: instead of detecting that a
// derived id had reached state it should not, there is no such id to reach it.
func TestInventedIDReachesNothing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	invented := config.DerivedRepoIDForUnresolvedRoot(repoPath)
	realID := repoID(t, repoPath)
	if invented == realID {
		t.Fatalf("an invented id must not equal the repository's own, both %s", invented)
	}

	// Deleting the invented identity must be the clean no-op the delete path
	// always claimed to be — it can name no real repository at all.
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoID: invented}); err != nil {
		t.Fatalf("deleting an invented identity must be an idempotent no-op: %v", err)
	}
	manager.mu.Lock()
	_, reached := manager.deletedRootRepos[realID]
	manager.mu.Unlock()
	if reached {
		t.Fatalf("an invented id must not reach the real repository's state — that reach is the collision every #3334 guard existed to catch")
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("nor may it deregister the project's record: %v", statErr)
	}
}

// TestStaleMismatchDoesNotReleaseTombstoneAfterTheOriginalReturns is #3611's
// headline regression. `deletedClaimDisproven` released a deletion tombstone on
// the record's identityMismatch, which is the LAST OBSERVATION rather than a
// current fact. For a main-root recording the recorded identity is also any
// occupant's real ID, so the whole sequence lands at one repo id: the project
// is deleted while its path is absent (claimant = itself), an unrelated clone
// takes the path and proves the mismatch (round 15 releases the tombstone,
// correctly), and then the occupant leaves and the deleted project's ORIGINAL
// checkout comes back. During the settled probe's backoff nothing re-proves
// anything, so the record still says identityMismatch and the tombstone keeps
// being released — for a checkout that IS the deleted project's.
func TestStaleMismatchDoesNotReleaseTombstoneAfterTheOriginalReturns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	// No legacy root_agents entry: this test inspects the tombstone predicate
	// itself, and a legacy opt-in would have the ensure pass create a root
	// through the recording backend for reasons unrelated to the release rule.
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Deleted while the path is absent, so the record claims its own tombstone
	// (claimantForRecord's determinate-absence arm) — the shape a later
	// occupant's proven mismatch is allowed to release.
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject while absent: %v", err)
	}
	// An unrelated clone occupies the path and the probe proves the mismatch.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}
	manager.ensureRootAgentsAndWait()

	sharedID := repoID(t, repoPath)
	manager.mu.Lock()
	claimant, tombstoned := manager.deletedRootRepos[sharedID]
	releasedOnFreshProof := !manager.rootDeletionTombstoneApplies(manager.rootAgentLayers.Load(), sharedID)
	manager.mu.Unlock()
	if !tombstoned || claimant != project.ID {
		t.Fatalf("fixture must tombstone %s claimed by %s, got %q (present=%v)", sharedID, project.ID, claimant, tombstoned)
	}
	record, ok := manager.rootAgentLayers.Load().unresolvedRoots[sharedID]
	if !ok || !record.identityMismatch || record.projectID != project.ID {
		t.Fatalf("fixture must publish a PROVEN mismatch at %s owned by %s, got %+v (present=%v)", sharedID, project.ID, record, ok)
	}
	if !releasedOnFreshProof {
		t.Fatalf("fixture must first reach round 15's release on a FRESH mismatch, or the assertion below proves nothing")
	}

	// The occupant leaves; the deleted project's ORIGINAL checkout returns to
	// its own path. The probe settled onto its backoff one pass ago, so no
	// probe re-checks the path in the passes below — the record's mismatch is
	// now evidence about a checkout that is no longer there.
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove occupant: %v", err)
	}
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore the original checkout: %v", err)
	}
	manager.ensureRootAgentsAndWait()
	manager.ensureRootAgentsAndWait()

	manager.mu.Lock()
	stillReleased := !manager.rootDeletionTombstoneApplies(manager.rootAgentLayers.Load(), sharedID)
	manager.mu.Unlock()
	if stillReleased {
		t.Fatalf("a mismatch nothing has re-proved since the checkout changed released the tombstone; the deleted project's own root can be recreated at its own path (#3611)")
	}
	if got := manager.rootAgentMaterializeVerdictFor(sharedID).reason; got != rootAgentProjectDeleted {
		t.Fatalf("the verdict must still report the deletion once the release evidence is stale, got %v", got)
	}
}

// TestRefreshedMismatchKeepsReleasingTheTombstone is #3611's over-correction
// guard, and it is the reason the freshness mark has to be REWRITTEN by a probe
// result that confirms what the record already said. The negative-outcome arm
// of the consume phase used to write only when a FLAG changed, so a mismatch
// re-proved pass after pass would keep its original mark, go stale, and hold
// the tombstone for the daemon's life — suppressing the live occupant's own
// root, which is exactly what #3299 review round 15 exists to prevent.
func TestRefreshedMismatchKeepsReleasingTheTombstone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	// Zero backoff so every pass re-probes and re-proves the same mismatch;
	// with the default curve the entry would rest instead, which is the
	// staleness the test above is about.
	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })

	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject while absent: %v", err)
	}
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}

	sharedID := repoID(t, repoPath)
	// The occupant STAYS. Every pass re-establishes the same mismatch, so the
	// release must survive every one of them.
	for pass := 1; pass <= 5; pass++ {
		manager.ensureRootAgentsAndWait()
		manager.mu.Lock()
		applies := manager.rootDeletionTombstoneApplies(manager.rootAgentLayers.Load(), sharedID)
		manager.mu.Unlock()
		if pass == 1 {
			record, ok := manager.rootAgentLayers.Load().unresolvedRoots[sharedID]
			if !ok || !record.identityMismatch {
				t.Fatalf("fixture must prove the mismatch on the first pass, got %+v (present=%v)", record, ok)
			}
		}
		if applies {
			t.Fatalf("pass %d: a mismatch the probe keeps re-proving must keep releasing the dead claimant's tombstone, or the live occupant's own root stays suppressed (#3299 review round 15)", pass)
		}
	}
}
