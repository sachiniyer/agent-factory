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
