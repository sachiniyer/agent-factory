package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These tests close the #3247 arm-2 residue (#3299): a recorded project root
// that did not resolve at daemon start is re-attempted on the ensure cadence,
// and a successful git resolution — content-bearing evidence a mount flap
// cannot fabricate — moves the project's layers onto the repo's REAL identity
// and lets the singleton sweep create the root this run. That covers both the
// plain absent-mount case and the recorded roots whose derived-path identity
// can never match (a linked worktree, a subdirectory registration).

// rewriteRecordRoot hand-edits a registered project's record to point at a
// different recorded root — the shape a bare-clone linked-worktree
// registration produces, which the public API deliberately never writes.
func rewriteRecordRoot(t *testing.T, projectID, newRoot string) {
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
	record["root"] = newRoot
	record["checkout_root"] = newRoot
	out, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

// TestEnsureRootAgentsReattributesResolvedRootMidRun: the plain arm-2 case —
// same recorded path, absent at boot, back before the next ensure pass. The
// singleton sweep must create the root THIS run; before #3299 the project
// stayed frozen out of projectRoots until a daemon restart.
func TestEnsureRootAgentsReattributesResolvedRootMidRun(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/reattributed\"")

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

	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a recorded root that resolves again must be ensured this run, got %d creates — restart-to-recover is the residue #3299 closes", len(*seen))
	}
	if (*seen)[0].Program != "/opt/reattributed" {
		t.Fatalf("the re-attributed personal program must reach the create verbatim, got %q", (*seen)[0].Program)
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentWillMaterialize {
		t.Fatalf("the healed project must report will-materialize, got reason %d", got)
	}
}

// TestEnsureRootAgentsKeepsSwappedCloneUnresolved pins the #3299 review's
// identity rule: a DIFFERENT clone reusing the recorded path resolves in git
// but carries no checkout marker for the project, so re-attribution must
// refuse — availability is not identity. Before the fix the swap inherited
// the project's enabled personal layer and an autonomous root.
func TestEnsureRootAgentsKeepsSwappedCloneUnresolved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/hijacked\"")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A stranger's clone appears at the recorded path: a real git repo, but
	// without the registered checkout's marker.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init swapped clone: %v", err)
	}

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("a marker-less checkout at the recorded path must not inherit the project's root agent, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentProjectUnresolved {
		t.Fatalf("an unverified checkout must keep the project unresolved (fail closed), got reason %d", got)
	}
}

// TestEnsureRootAgentsReattributesWorktreeRecordedRoot: the identity residue —
// the record names a linked worktree, whose derived path hash can never equal
// the repo's main-root identity. Resolution must move the personal disable
// onto the REAL repo ID before the legacy sweep resolves the same repo, or
// the disable is bypassed exactly as in the original #3247 report.
func TestEnsureRootAgentsReattributesWorktreeRecordedRoot(t *testing.T) {
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
	writePersonalRootAgent(t, project.ID, "enabled = false")
	rewriteRecordRoot(t, project.ID, worktree)

	// Both paths vanish at daemon start (one mount), and return before the
	// next ensure pass.
	aside := parent + ".aside"
	if err := os.Rename(parent, aside); err != nil {
		t.Fatalf("hide parent: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(aside, parent); err != nil {
		t.Fatalf("restore parent: %v", err)
	}

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("the re-attributed personal disable must suppress the legacy root, got %d creates — the worktree-recorded identity residue reopened the #3247 fail-open", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentDisabled {
		t.Fatalf("the healed project must report the true disable, got reason %d", got)
	}
}

// setupBareCloneWorktree builds the #3299 identity residue in its sharpest
// shape: a linked worktree of a BARE clone, where RepoFromPath resolves to
// the parent of the bare common directory — a non-repository.
func setupBareCloneWorktree(t *testing.T) (parent, worktree string) {
	t.Helper()
	parent = testguard.CanonicalTempDir(t)
	src := filepath.Join(parent, "src")
	if err := exec.Command("git", "init", src).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	for _, args := range [][]string{
		{"-C", src, "config", "user.email", "t@t"},
		{"-C", src, "config", "user.name", "t"},
		{"-C", src, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	bare := filepath.Join(parent, "bare.git")
	if err := exec.Command("git", "clone", "--bare", src, bare).Run(); err != nil {
		t.Fatalf("git clone --bare: %v", err)
	}
	worktree = filepath.Join(parent, "wt")
	if err := exec.Command("git", "-C", bare, "worktree", "add", worktree).Run(); err != nil {
		t.Fatalf("git worktree add from bare: %v", err)
	}
	return parent, worktree
}

// TestEnsureRootAgentsReattributesBareCloneWorktree pins the #3299 review's
// round-2 P1: probing the checkout marker at repo.Root fails forever for a
// bare-clone linked worktree; the RECORDED path binds to the common dir the
// marker lives in, exactly how Register/Rebind probe it.
func TestEnsureRootAgentsReattributesBareCloneWorktree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	parent, worktree := setupBareCloneWorktree(t)

	project := registerTestProject(t, worktree)
	writePersonalRootAgent(t, project.ID, "enabled = false")

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
		t.Fatalf("a personal disable must hold through re-attribution, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, worktree)).reason; got != rootAgentDisabled {
		t.Fatalf("a bare-clone linked-worktree registration must re-attribute once its path resolves — the marker lives in the common dir the RECORDED path binds to — got reason %d", got)
	}
}

// TestEnsureRootAgentsCreatesRootAtBareCloneWorktree pins the #3299 review's
// round-3 P1: re-attribution must publish the RECORDED root as the create
// path. repo.Root for a bare-clone linked worktree is the parent of the bare
// common directory — a non-repository — so a root enabled there could never
// actually be created; the disable-only sibling test above cannot see that.
func TestEnsureRootAgentsCreatesRootAtBareCloneWorktree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	parent, worktree := setupBareCloneWorktree(t)

	project := registerTestProject(t, worktree)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/bare-root\"")

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

	if len(*seen) != 1 {
		t.Fatalf("an enabled bare-clone worktree project must get its root once its mount returns, got %d creates — a non-repository create path fails before the backend", len(*seen))
	}
	// The created workspace follows af's repo identity, which for a bare
	// clone's linked worktree is the parent of the bare dir — a pre-existing,
	// af-wide limitation (#3358) shared by every create in such a repo. What
	// this PR owns is PARITY: re-attribution must produce exactly the create
	// a boot-time resolution of the same project produces.
	bootRepo, err := config.RepoFromPath(worktree)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if got := (*seen)[0].Path; got != bootRepo.Root {
		t.Fatalf("the re-attributed create must land where a boot-resolved one would (%q), got %q", bootRepo.Root, got)
	}
	if got := (*seen)[0].Program; got != "/opt/bare-root" {
		t.Fatalf("the personal program must reach the create verbatim, got %q", got)
	}
}

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

// TestReattributionBoundsStalledProbes pins the #3299 review's round-4 P1:
// time.After delivers exactly one value, so with two stalled probes the first
// consumed the shared deadline and the second blocked the poll goroutine
// forever. Two never-completing probes must cost one grace, not a wedge.
func TestReattributionBoundsStalledProbes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoA := setupControlRepo(t)
	repoB := setupControlRepo(t)
	registerTestProject(t, repoA)
	registerTestProject(t, repoB)

	for _, p := range []string{repoA, repoB} {
		if err := os.Rename(p, p+".hidden"); err != nil {
			t.Fatalf("hide repo dir: %v", err)
		}
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Both probes are permanently stalled (their done channels never close),
	// standing in for recorded roots on unresponsive mounts.
	manager.mu.Lock()
	manager.rootHealProbes[config.RepoIDForRecordedRoot(repoA)] = &rootReattributionProbe{done: make(chan struct{})}
	manager.rootHealProbes[config.RepoIDForRecordedRoot(repoB)] = &rootReattributionProbe{done: make(chan struct{})}
	manager.mu.Unlock()

	finished := make(chan struct{})
	go func() {
		manager.EnsureRootAgents()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("EnsureRootAgents wedged on the second stalled probe — the shared per-pass deadline fires once and must not be received twice")
	}
}

// TestReattributionDiscardsStaleProbeResult pins the #3299 review's round-4
// P1: a probe that finishes after its pass's grace expired describes a
// filesystem from a previous cadence. Consuming it later would re-attribute a
// checkout nobody has re-verified — here the verified clone was replaced by a
// marker-less stranger in the interim. And with the mismatch established, the
// consumer-facing verdict must name the rebind remedy, not "bring the path
// back" (round-4 P2): the path is present.
func TestReattributionDiscardsStaleProbeResult(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/stale\"")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The checkout at the recorded path is now a DIFFERENT clone…
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init swapped clone: %v", err)
	}
	// …but a cached probe from an earlier pass still says the original
	// verified checkout is there.
	stale := &rootReattributionProbe{
		done:    make(chan struct{}),
		matches: true,
		repo:    &config.RepoContext{Root: repoPath, ID: config.RepoIDFromRoot(repoPath)},
	}
	close(stale.done)
	manager.mu.Lock()
	manager.rootHealProbes[config.RepoIDForRecordedRoot(repoPath)] = stale
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("a stale probe result must be discarded, not published — the checkout it verified is gone; got %d creates", len(*seen))
	}
	verdict := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath))
	if verdict.reason != rootAgentProjectUnresolved {
		t.Fatalf("the swapped clone must stay unresolved after the fresh re-check, got reason %d", verdict.reason)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "rebind") || !strings.Contains(detail, project.ID) {
		t.Fatalf("an identity mismatch must prescribe the rebind (naming the project), not \"bring the path back\"; got: %s", detail)
	}
	if strings.Contains(detail, "bring the path back") {
		t.Fatalf("the path is present — the detail must not claim it is absent; got: %s", detail)
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

// TestMarkerReadFailureIsNotAMismatch pins the #3299 review's round-5 P2: a
// marker that cannot be READ (permissions, I/O) leaves identity unknowable —
// prescribing a rebind there could destroy a transiently unreadable original
// checkout. The verdict must say the marker is unreadable, not that a
// different clone occupies the path.
func TestMarkerReadFailureIsNotAMismatch(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/unreadable\"")

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
	// The checkout is back — the ORIGINAL — but its marker file is
	// unreadable.
	candidates, err := filepath.Glob(filepath.Join(repoPath, ".git", "agent-factory", "checkout-id-*"))
	if err != nil {
		t.Fatalf("glob markers: %v", err)
	}
	var markers []string
	for _, m := range candidates {
		if !strings.HasSuffix(m, ".lock") {
			markers = append(markers, m)
		}
	}
	if len(markers) != 1 {
		t.Fatalf("expected exactly one checkout marker, got %v", candidates)
	}
	if err := os.Chmod(markers[0], 0o000); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(markers[0], 0o644) })

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("an unverifiable checkout must not be re-attributed, got %d creates", len(*seen))
	}
	verdict := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath))
	if verdict.reason != rootAgentProjectUnresolved {
		t.Fatalf("an unverifiable checkout must stay unresolved, got reason %d", verdict.reason)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if strings.Contains(detail, "rebind") {
		t.Fatalf("an unreadable marker is not a proven mismatch — the detail must not prescribe a rebind; got: %s", detail)
	}
	if !strings.Contains(detail, "cannot be read") {
		t.Fatalf("the detail must name the unreadable marker; got: %s", detail)
	}
}

// TestWorktreeMismatchVisibleUnderResolvedID pins the #3299 review's round-5
// P2: when the recorded root is occupied by a checkout of a DIFFERENT
// repository, consumers query by the identity that checkout resolves to. The
// mismatch record lives under the derived ID; without the resolved-ID bridge
// the verdict reads "not configured" and the rebind remedy never surfaces.
func TestWorktreeMismatchVisibleUnderResolvedID(t *testing.T) {
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
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/occupied\"")
	rewriteRecordRoot(t, project.ID, recorded)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A worktree of a DIFFERENT repository now occupies the recorded path.
	if err := exec.Command("git", "-C", other, "worktree", "add", recorded).Run(); err != nil {
		t.Fatalf("git worktree add occupant: %v", err)
	}

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("a foreign occupant must not inherit the project's root agent, got %d creates", len(*seen))
	}
	verdict := manager.rootAgentMaterializeVerdictFor(repoID(t, other))
	if verdict.reason != rootAgentProjectUnresolved {
		t.Fatalf("the identity the occupant resolves to must surface the unresolved mismatch through the bridge, got reason %d", verdict.reason)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "rebind") || !strings.Contains(detail, project.ID) {
		t.Fatalf("the bridged verdict must carry the rebind remedy naming the project; got: %s", detail)
	}
}

// TestPendingProbeKeepsHealCadenceClose pins the #3299 review's round-5 P1:
// an in-flight (or freshly landed) probe is progress, not a failed read. If
// it fed the failure backoff, a responsive-but-slow mount would wait minutes
// between passes and its completed results would expire before consumption —
// unresolved forever.
func TestPendingProbeKeepsHealCadenceClose(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)

	if err := os.Rename(repoPath, repoPath+".hidden"); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The entry's probe is permanently in flight (a stalled mount).
	manager.mu.Lock()
	manager.rootHealProbes[config.RepoIDForRecordedRoot(repoPath)] = &rootReattributionProbe{done: make(chan struct{})}
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	manager.mu.Lock()
	next := manager.rootHealNextAttempt
	manager.mu.Unlock()
	if next.After(time.Now().Add(time.Second)) {
		t.Fatalf("a pending probe must keep the next heal pass one tick away, not on the failure backoff curve; next attempt is %v away", time.Until(next))
	}
}
