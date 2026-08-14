package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// DeleteProject's interactions with #3299 re-attribution: identity
// translation, tombstone/alias suppression, fences, and probe lifecycles.
// Fixtures (rewriteRecordRoot, setupBareCloneWorktree) live in
// rootagent_reattribution_test.go.

// TestReattributionRespectsActiveDeleteFence pins the #3299 review's round-2
// P1: DeleteProject installs its admission fence (projectDeletes) under the
// derived ID BEFORE it records the deletion tombstone. Re-attribution inside
// that window would publish the project under a real ID the in-flight delete
// has never heard of, and the singleton sweep would create the root mid-
// delete. A fenced entry must stay unresolved.
func TestReattributionRespectsActiveDeleteFence(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
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
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/fenced\"")
	rewriteRecordRoot(t, project.ID, worktree)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A delete is mid-flight: the fence is installed under the derived ID,
	// the tombstone not yet.
	manager.mu.Lock()
	manager.projectDeletes[config.RepoIDForRecordedRoot(worktree)] = struct{}{}
	manager.mu.Unlock()
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("re-attribution during an active delete fence must not publish the project under a real ID the delete cannot fence, got %d creates", len(*seen))
	}
	if layers := manager.rootAgentLayers.Load(); len(layers.unresolvedRoots) != 1 {
		t.Fatalf("a fenced entry must stay unresolved so the finished delete's tombstone keeps its derived-ID target, got %d unresolved entries", len(layers.unresolvedRoots))
	}
	// The fenced probe must stay in place — retiring it would release the
	// pending gate without ever publishing the alias (#3299 review round
	// 12) — and once the fence clears it must still carry the transition.
	derivedID := config.RepoIDForRecordedRoot(worktree)
	manager.mu.Lock()
	_, probeKept := manager.rootHealProbes[derivedID]
	delete(manager.projectDeletes, derivedID)
	manager.mu.Unlock()
	if !probeKept {
		t.Fatalf("the fenced probe must not be retired — its presence is the pending gate and its result is the alias-bearing transition")
	}
	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("once the fence clears, the retained probe must complete the transition, got %d creates", len(*seen))
	}
}

// TestReattributionCarriesDeletionTombstone pins the #3299 review's deletion
// rule: DeleteProject on an unresolved project can only key its suppression by
// the derived recorded-path ID, so re-attribution onto the repo's real ID must
// carry the tombstone with it — or the explicitly deleted project's root is
// recreated the moment its mount returns.
func TestReattributionCarriesDeletionTombstone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
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
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/deleted\"")
	rewriteRecordRoot(t, project.ID, worktree)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The user deletes the project while its checkout is unavailable: the only
	// identity DeleteProject can record is the derived recorded-path ID.
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree}); err != nil {
		t.Fatalf("DeleteProject while unresolved: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("a deleted project's root must stay suppressed across re-attribution, got %d creates — the tombstone was lost moving to the real repo ID", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentProjectDeleted {
		t.Fatalf("the re-attributed identity must report the deletion, got reason %d", got)
	}
}

// TestDeleteAfterReattributionStillSuppresses pins the #3299 review's round-4
// P1: a delete that starts AFTER the identity transition normalizes the (again
// unavailable) recorded path to the derived ID, and no carry-at-transition can
// see it. The snapshot's reattributedFrom alias must bridge the identities for
// every deletion-state consumer.
func TestDeleteAfterReattributionStillSuppresses(t *testing.T) {
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
	writePersonalRootAgent(t, project.ID, "enabled = false")
	rewriteRecordRoot(t, project.ID, worktree)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}
	// The mount returns and the project re-attributes onto its real identity…
	manager.EnsureRootAgents()

	// …then the mount vanishes again and the user deletes the project: the
	// unavailable path normalizes to the derived recorded-path ID.
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent again: %v", err)
	}
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree}); err != nil {
		t.Fatalf("DeleteProject after re-attribution: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent again: %v", err)
	}

	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentProjectDeleted {
		t.Fatalf("a derived-ID delete landing after the identity transition must suppress the real identity through the snapshot alias, got reason %d", got)
	}
}

// TestDeleteTranslatesReattributedIdentity pins the #3299 review's round-5
// P1: after re-attribution, every piece of live state sits under the REAL
// repo ID. A path-only delete of the again-unavailable recorded path hashes
// to the derived ID — the delete must translate through the snapshot alias
// and tear down the real identity, not deregister the record while the
// real-ID root keeps running.
func TestDeleteTranslatesReattributedIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
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
	// Disabled on purpose: the pin is the identity translation, and a live
	// fake-backend root would drag in archive-vs-kill classification that
	// depends on launch state the recording backend never reaches. The
	// re-attribution alias is recorded regardless of the enable decision.
	writePersonalRootAgent(t, project.ID, "enabled = false")
	rewriteRecordRoot(t, project.ID, worktree)
	realID := repoID(t, repoPath)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 0 {
		t.Fatalf("setup: the disabled project must re-attribute without a create, got %d creates", len(*seen))
	}
	if layers := manager.rootAgentLayers.Load(); layers.reattributedFrom[realID] == "" {
		t.Fatalf("setup: re-attribution must record the identity alias")
	}

	// The mount vanishes again, and the user deletes by the recorded path.
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent again: %v", err)
	}
	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if err != nil {
		t.Fatalf("DeleteProject by unavailable recorded path: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent again: %v", err)
	}
	if result.RepoID != realID {
		t.Fatalf("the delete must translate to the re-attributed real identity %s, got %s — live state lives there", realID, result.RepoID)
	}
	manager.mu.Lock()
	_, suppressed := manager.deletedRootRepos[realID]
	manager.mu.Unlock()
	if !suppressed {
		t.Fatalf("the ensure loop must be suppressed under the REAL identity after the translated delete")
	}
}

// TestRepoIDOnlyDeleteDeregistersReattributedRecord pins the #3299 review's
// round-6 P2: a real-ID-only delete of a re-attributed project must find the
// registry record through the alias — the record's lookup key is the derived
// recorded-path ID — or the delete reports success while the durable
// registration survives a restart.
func TestRepoIDOnlyDeleteDeregistersReattributedRecord(t *testing.T) {
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
	writePersonalRootAgent(t, project.ID, "enabled = false")
	rewriteRecordRoot(t, project.ID, worktree)
	realID := repoID(t, repoPath)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}
	manager.EnsureRootAgents()

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoID: realID}); err != nil {
		t.Fatalf("DeleteProject by real repo id: %v", err)
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); !os.IsNotExist(statErr) {
		t.Fatalf("the durable record must be deregistered through the alias, not silently survive (stat err: %v)", statErr)
	}
}

// TestBothSelectorsAcceptReattributedPair pins the #3299 review's round-7 P2:
// a documented request naming the real repo_id AND the (again unavailable)
// recorded path describes ONE re-attributed project; the selector match must
// consult the alias instead of rejecting the pair.
func TestBothSelectorsAcceptReattributedPair(t *testing.T) {
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
	writePersonalRootAgent(t, project.ID, "enabled = false")
	rewriteRecordRoot(t, project.ID, worktree)
	realID := repoID(t, repoPath)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}
	manager.EnsureRootAgents()

	// The mount vanishes again; the client supplies BOTH selectors it knows.
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent again: %v", err)
	}
	t.Cleanup(func() { _ = os.Rename(aside, parent) })
	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: realID, RepoPath: worktree})
	if err != nil {
		t.Fatalf("both-selector delete of a re-attributed project must be accepted through the alias: %v", err)
	}
	if result.RepoID != realID {
		t.Fatalf("the delete must land on the real identity, got %s", result.RepoID)
	}
}

// TestDeleteSuppressionSurvivesLateProbeCompletion pins the converged
// rounds 9-11 stance on deleting a project whose identity verification is
// stalled: the probe keeps running and its pending gate keeps the candidate
// repo fail-closed (which doubles as the delete's suppression), and when the
// probe finally completes, the re-attribution publishes the alias that lets
// the derived-ID tombstone suppress — and report — the deletion under the
// real identity. No window in that sequence may create the deleted root.
func TestDeleteSuppressionSurvivesLateProbeCompletion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
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
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/latecomer\"")
	rewriteRecordRoot(t, project.ID, worktree)
	realID := repoID(t, repoPath)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	stuck := &rootReattributionProbe{done: make(chan struct{})}
	stuck.candidate.Store(&config.RepoContext{Root: repoPath, ID: realID})
	manager.mu.Lock()
	manager.rootHealProbes[config.RepoIDForRecordedRoot(worktree)] = stuck
	manager.mu.Unlock()

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree}); err != nil {
		t.Fatalf("DeleteProject while verification is stalled: %v", err)
	}
	// While the marker read stalls, fail-closed doubles as suppression.
	if got := manager.rootAgentMaterializeVerdictFor(realID).reason; got != rootAgentAttributionPending {
		t.Fatalf("a stalled verification must keep the candidate repo fail-closed, got reason %d", got)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 0 {
		t.Fatalf("no window during the stall may create the deleted root, got %d creates", len(*seen))
	}

	// The probe finally completes with a verified match: the alias must
	// carry the derived-ID tombstone to the real identity.
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}
	stuck.repo = &config.RepoContext{Root: repoPath, ID: realID}
	stuck.matches = true
	stuck.completedAt = time.Now()
	close(stuck.done)
	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("the late completion must re-attribute into suppression, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(realID).reason; got != rootAgentProjectDeleted {
		t.Fatalf("the re-attributed identity must report the deletion through the alias, got reason %d", got)
	}
}

// TestDeletedClaimantMismatchReadsNotConfigured pins the #3299 review's
// round-12 P2: when the project claiming a recorded path was DELETED and the
// checkout now there is a PROVEN different clone, the occupant's repo is
// simply unconfigured — neither the dead project's deletion guidance nor a
// rebind of a dead project applies.
func TestDeletedClaimantMismatchReadsNotConfigured(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	repoPath := filepath.Join(parent, "repo")
	other := filepath.Join(parent, "other")
	for _, r := range []string{repoPath, other} {
		if err := exec.Command("git", "init", r).Run(); err != nil {
			t.Fatalf("git init: %v", err)
		}
		for _, args := range [][]string{
			{"-C", r, "config", "user.email", "t@t"},
			{"-C", r, "config", "user.name", "t"},
			{"-C", r, "commit", "--allow-empty", "-m", "init"},
		} {
			if err := exec.Command("git", args...).Run(); err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}
	}
	recorded := filepath.Join(parent, "occupied")
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")
	rewriteRecordRoot(t, project.ID, recorded)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The user deletes the project while its recorded path is absent…
	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: recorded}); err != nil {
		t.Fatalf("DeleteProject while unresolved: %v", err)
	}
	// …then an unrelated repository's worktree occupies the path.
	if err := exec.Command("git", "-C", other, "worktree", "add", recorded).Run(); err != nil {
		t.Fatalf("git worktree add occupant: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("neither the dead project nor the occupant may gain a root here, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, other)).reason; got != rootAgentNotConfigured {
		t.Fatalf("a proven-unrelated occupant of a deleted project's path is simply unconfigured, got reason %d", got)
	}
}

// TestReusedPathDeleteTargetsOccupant pins the #3299 review's round-14 P1:
// when a re-attributed project's old recorded path is later reused as the
// MAIN ROOT of a different repository, that occupant's real ID equals the
// alias's derived ID. Deleting the occupant by its path must not be
// reverse-translated into tearing down the old project.
func TestReusedPathDeleteTargetsOccupant(t *testing.T) {
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
	writePersonalRootAgent(t, project.ID, "enabled = false")
	rewriteRecordRoot(t, project.ID, worktree)
	oldRealID := repoID(t, repoPath)

	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}
	manager.EnsureRootAgents() // re-attributes, alias oldReal→derived

	// The worktree is replaced by an unrelated repository MAIN-ROOTED at the
	// old recorded path: its real ID is the alias's derived ID.
	if err := exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktree).Run(); err != nil {
		t.Fatalf("git worktree remove: %v", err)
	}
	if err := exec.Command("git", "init", worktree).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if err != nil {
		t.Fatalf("DeleteProject of the occupant: %v", err)
	}
	if result.RepoID == oldRealID {
		t.Fatalf("deleting the occupant must not be reverse-translated into the old project's identity %s", oldRealID)
	}
	manager.mu.Lock()
	_, oldSuppressed := manager.deletedRootRepos[oldRealID]
	manager.mu.Unlock()
	if oldSuppressed {
		t.Fatalf("the old project must be untouched by the occupant's delete")
	}
}
