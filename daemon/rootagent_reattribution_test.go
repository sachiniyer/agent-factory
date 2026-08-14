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
	verdict := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath))
	if verdict.reason != rootAgentDisabled || !verdict.rootIdentityMismatch {
		// With the dead claim's enable no longer honored for a PROVEN
		// mismatch (round 13), the truthful verdict is disabled-with-the-
		// mismatch-clause: nothing legitimate enables the occupant.
		t.Fatalf("a proven mismatch must read as disabled with the mismatch shape, got reason %d (mismatch=%v)", verdict.reason, verdict.rootIdentityMismatch)
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
	if verdict.reason != rootAgentDisabled || !verdict.rootIdentityMismatch {
		t.Fatalf("the swapped clone must stay rejected after the fresh re-check (disabled + mismatch since round 13 stopped honoring the dead claim's enable), got reason %d (mismatch=%v)", verdict.reason, verdict.rootIdentityMismatch)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "rebind") || !strings.Contains(detail, project.ID) {
		t.Fatalf("an identity mismatch must prescribe the rebind (naming the project), not \"bring the path back\"; got: %s", detail)
	}
	if strings.Contains(detail, "bring the path back") {
		t.Fatalf("the path is present — the detail must not claim it is absent; got: %s", detail)
	}
}

// TestMarkerReadFailureIsNotAMismatch pins the #3299 review's round-5 P2: a
// marker that cannot be READ (permissions, I/O) leaves identity unknowable —
// prescribing a rebind there could destroy a transiently unreadable original
// checkout. The verdict must say the marker is unreadable, not that a
// different clone occupies the path.
func TestMarkerReadFailureIsNotAMismatch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not make a file unreadable for root")
	}
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
	if verdict.reason != rootAgentDisabled || !verdict.rootIdentityMismatch {
		t.Fatalf("the identity the occupant resolves to must surface the mismatch through the bridge, got reason %d (mismatch=%v)", verdict.reason, verdict.rootIdentityMismatch)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "rebind") || !strings.Contains(detail, project.ID) {
		t.Fatalf("the bridged verdict must carry the rebind remedy naming the project; got: %s", detail)
	}
}

// TestProbeConsumedDespitePersonalBackoff pins the #3299 review's rounds 5+8
// invariant pair: re-attribution runs on every pass, ungated by
// rootHealNextAttempt, so a responsive-but-slow mount's completed probe is
// consumed the tick after it lands even while the registry/personal clock is
// minutes deep in backoff.
func TestProbeConsumedDespitePersonalBackoff(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/slowmount\"")

	if err := os.Rename(repoPath, repoPath+".hidden"); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(repoPath+".hidden", repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}
	// The registry/personal clock is deep in backoff — re-attribution must
	// not care.
	manager.mu.Lock()
	manager.rootHealNextAttempt = time.Now().Add(5 * time.Minute)
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("re-attribution must run ungated by the registry/personal backoff clock, got %d creates", len(*seen))
	}
}

// TestInflightProbeLeavesPersonalCadenceAlone pins the #3299 review's round-8
// P1: a permanently in-flight probe must not pull the personal-config retry
// onto the per-tick cadence — its two-strike ENOENT observations are only
// meaningful when SPACED by the failure backoff (#3315), and consecutive
// one-second ticks could release a fail-closed latch during a mount flap.
func TestInflightProbeLeavesPersonalCadenceAlone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	unresolvedRepo := setupControlRepo(t)
	brokenRepo := setupControlRepo(t)
	registerTestProject(t, unresolvedRepo)
	broken := registerTestProject(t, brokenRepo)
	writePersonalRootAgent(t, broken.ID, "enabled = tr\nue = nonsense")

	if err := os.Rename(unresolvedRepo, unresolvedRepo+".hidden"); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.mu.Lock()
	manager.rootHealProbes[config.RepoIDForRecordedRoot(unresolvedRepo)] = &rootReattributionProbe{done: make(chan struct{})}
	manager.mu.Unlock()

	// Pass 1 attempts the still-broken personal config and must land its
	// NEXT attempt on the failure backoff, stalled sibling or not.
	manager.EnsureRootAgents()

	manager.mu.Lock()
	next := manager.rootHealNextAttempt
	manager.mu.Unlock()
	if !next.After(time.Now().Add(5 * time.Second)) {
		t.Fatalf("an in-flight probe must not drag the personal-config retry onto the per-tick cadence; next attempt is only %v away", time.Until(next))
	}
}

// TestLegacyFailsClosedWhileAttributionPending pins the #3299 review's
// round-8 P1: a probe that has RESOLVED a repo as some unresolved project's
// real identity but not yet delivered its marker verdict leaves that repo's
// decision unknowable — the project's personal layer still sits under the
// derived ID. The legacy sweep must fail closed for exactly that window
// instead of starting the root off global layers.
func TestLegacyFailsClosedWhileAttributionPending(t *testing.T) {
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
	realID := repoID(t, repoPath)

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
	// The probe has resolved the real identity but its marker verdict is
	// still in flight (a slow marker read on a flaky mount).
	stuck := &rootReattributionProbe{done: make(chan struct{})}
	stuck.candidate.Store(&config.RepoContext{Root: repoPath, ID: realID})
	manager.mu.Lock()
	manager.rootHealProbes[config.RepoIDForRecordedRoot(worktree)] = stuck
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("the legacy sweep must fail closed while identity verification is in flight, got %d creates — the personal disable sits under the derived ID", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(realID).reason; got != rootAgentAttributionPending {
		t.Fatalf("the verdict must name the pending verification, got reason %d", got)
	}
}

// TestNegativeProbeFeedsBackoffNotHotLoop pins the #3299 review's round-6 P2:
// a COMPLETED negative probe (path still absent, marker mismatch) is a normal
// failed read and must advance the failure backoff. Treating it as "pending"
// kept the heal on the poll-tick cadence, forking a git process per
// unavailable root per second, forever.
func TestNegativeProbeFeedsBackoffNotHotLoop(t *testing.T) {
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
	// The path stays absent: the probe completes negative within the pass.
	manager.EnsureRootAgents()

	manager.mu.Lock()
	probe := manager.rootHealProbes[config.RepoIDForRecordedRoot(repoPath)]
	manager.mu.Unlock()
	if probe == nil || !probe.settled {
		t.Fatalf("a completed negative probe must settle in place under its own backoff, got %+v", probe)
	}
	if !probe.retryAt.After(time.Now().Add(5 * time.Second)) {
		t.Fatalf("the settled negative's retry must sit on the backoff curve, not the per-tick cadence; retry is only %v away", time.Until(probe.retryAt))
	}
}

// TestUnreadableMarkerFailsLegacyClosed pins the #3299 review's round-6 P1:
// with the checkout's identity unknowable (marker unreadable), the project's
// personal layer — possibly enabled=false — sits under the derived ID where
// the legacy sweep's resolution cannot see it. The one fail-closed predicate
// must cover the bridged identity, or a legacy root_agents entry starts the
// root off global layers alone.
func TestUnreadableMarkerFailsLegacyClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not make a file unreadable for root")
	}
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
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/gated\"")
	rewriteRecordRoot(t, project.ID, worktree)

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
		t.Fatalf("an unverifiable checkout must fail the legacy entry closed, got %d creates — the personal layer under the derived ID may hold enabled=false", len(*seen))
	}
	verdict := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath))
	if verdict.reason != rootAgentProjectUnresolved || !verdict.rootMarkerUnreadable {
		t.Fatalf("the bridged verdict must report the unverifiable checkout, got reason %d (markerUnreadable=%v)", verdict.reason, verdict.rootMarkerUnreadable)
	}
}

// TestDisabledDetailNamesIdentityFailures pins the #3299 review's round-6 P2:
// the disabled renderer's path clause must match the actual failure — a
// present-but-unverified checkout cannot be "brought back".
func TestDisabledDetailNamesIdentityFailures(t *testing.T) {
	mismatch := rootAgentUnavailableDetail(rootAgentMaterializeVerdict{
		reason: rootAgentDisabled, enabledSource: config.RootAgentSourcePersonal,
		rootUnresolved: true, rootIdentityMismatch: true, projectID: "prj_x",
	})
	if !strings.Contains(mismatch, "rebind") || !strings.Contains(mismatch, "prj_x") {
		t.Fatalf("a disabled project behind a swapped checkout must name the rebind remedy; got: %s", mismatch)
	}
	if strings.Contains(mismatch, "bring that path back") {
		t.Fatalf("the path is present — the mismatch clause must not claim absence; got: %s", mismatch)
	}
	unreadable := rootAgentUnavailableDetail(rootAgentMaterializeVerdict{
		reason: rootAgentDisabled, enabledSource: config.RootAgentSourcePersonal,
		rootUnresolved: true, rootMarkerUnreadable: true, projectID: "prj_x",
	})
	if !strings.Contains(unreadable, "marker") || strings.Contains(unreadable, "rebind") {
		t.Fatalf("an unreadable marker is not a mismatch — name readability, never a rebind; got: %s", unreadable)
	}
}

// TestStaleBridgeRetired pins the #3299 review's round-6 P2: when a later
// probe finds the recorded path absent (or resolving elsewhere), the bridge
// from the previously observed repository must be retired, or that unrelated
// repo keeps answering with this project's remedies forever.
func TestStaleBridgeRetired(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
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
	if err := exec.Command("git", "-C", other, "worktree", "add", recorded).Run(); err != nil {
		t.Fatalf("git worktree add occupant: %v", err)
	}
	manager.EnsureRootAgents()
	if v := manager.rootAgentMaterializeVerdictFor(repoID(t, other)); !v.rootIdentityMismatch {
		t.Fatalf("setup: the occupant's identity must be bridged with the mismatch shape, got reason %d", v.reason)
	}

	// The occupant leaves; the bridge must go with it. The settled negative
	// rests under its own retry backoff (round 7), so expire it — the test
	// stands in for the backoff elapsing.
	if err := os.RemoveAll(recorded); err != nil {
		t.Fatalf("remove occupant: %v", err)
	}
	manager.mu.Lock()
	if p := manager.rootHealProbes[config.RepoIDForRecordedRoot(recorded)]; p != nil {
		p.retryAt = time.Time{}
	}
	manager.mu.Unlock()
	manager.EnsureRootAgents()

	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, other)).reason; got != rootAgentNotConfigured {
		t.Fatalf("the departed occupant's repository must stop answering for this project — got reason %d", got)
	}
}

// TestStalledSiblingDoesNotHotLoopNegatives pins the #3299 review's round-7
// P2: a permanently in-flight sibling keeps the heal pass on the per-tick
// cadence, and a completed NEGATIVE entry must rest under its own retry
// backoff on those hot passes — not be deleted and respawned as a fresh git
// probe every tick.
func TestStalledSiblingDoesNotHotLoopNegatives(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	stalledRepo := setupControlRepo(t)
	absentRepo := setupControlRepo(t)
	registerTestProject(t, stalledRepo)
	registerTestProject(t, absentRepo)
	for _, p := range []string{stalledRepo, absentRepo} {
		if err := os.Rename(p, p+".hidden"); err != nil {
			t.Fatalf("hide repo dir: %v", err)
		}
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	stalledID := config.RepoIDForRecordedRoot(stalledRepo)
	absentID := config.RepoIDForRecordedRoot(absentRepo)
	manager.mu.Lock()
	manager.rootHealProbes[stalledID] = &rootReattributionProbe{done: make(chan struct{})}
	manager.mu.Unlock()

	// Pass 1: the absent sibling's probe completes negative and must settle.
	manager.EnsureRootAgents()
	manager.mu.Lock()
	probe := manager.rootHealProbes[absentID]
	failures := manager.rootHealProbeFailures[absentID]
	manager.mu.Unlock()
	if probe == nil || !probe.settled || failures != 1 {
		t.Fatalf("a completed negative must settle in place under its own backoff (probe=%v failures=%d)", probe, failures)
	}

	// Pass 2 arrives on the stalled sibling's hot cadence: the settled entry
	// must rest, not respawn another git probe.
	manager.mu.Lock()
	manager.rootHealNextAttempt = time.Time{}
	manager.mu.Unlock()
	manager.EnsureRootAgents()
	manager.mu.Lock()
	same := manager.rootHealProbes[absentID] == probe
	failures = manager.rootHealProbeFailures[absentID]
	manager.mu.Unlock()
	if !same || failures != 1 {
		t.Fatalf("the settled negative must rest until its retryAt, got respawn (same=%v failures=%d)", same, failures)
	}
}

// TestAbsenceStrikesStaySpacedAcrossSiblingHeals pins the #3299 review's
// round-9 P1: a sibling healing in the same pass resets the shared retry
// clock to now, and the ENOENT two-strike release must still require its own
// strikes to be SPACED — two observations one tick apart are one mount flap,
// not two independent confirmations.
func TestAbsenceStrikesStaySpacedAcrossSiblingHeals(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoA := setupControlRepo(t)
	repoB := setupControlRepo(t)
	projectA := registerTestProject(t, repoA)
	projectB := registerTestProject(t, repoB)
	writePersonalRootAgent(t, projectA.ID, "enabled = tr")
	writePersonalRootAgent(t, projectB.ID, "enabled = tr")
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// After boot: A's config vanishes entirely (the ENOENT strike path), B's
	// becomes readable (it will heal and reset the shared clock).
	pathA, err := config.ProjectConfigTomlPath(projectA.ID)
	if err != nil {
		t.Fatalf("ProjectConfigTomlPath: %v", err)
	}
	if err := os.Remove(pathA); err != nil {
		t.Fatalf("remove A config: %v", err)
	}
	writePersonalRootAgent(t, projectB.ID, "enabled = false")

	manager.EnsureRootAgents() // pass 1: A strike 1, B heals, clock resets to now
	manager.EnsureRootAgents() // pass 2, one tick later: A's observation must be IGNORED

	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoA)).reason; got != rootAgentPersonalUnreadable {
		t.Fatalf("two unspaced ENOENT observations are one flap — the fail-closed latch must hold, got reason %d", got)
	}
}

// TestVanishedMidVerificationReportsPathRemedy pins the #3299 review's
// round-12 P2: a path that vanished between git resolution and the marker
// read is unknowable-BECAUSE-ABSENT — the verdict must send the user after
// the path, not after a marker that does not exist to be made readable.
func TestVanishedMidVerificationReportsPathRemedy(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	if err := os.Rename(repoPath, repoPath+".hidden"); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	realID := config.RepoIDFromRoot(repoPath)
	vanished := &rootReattributionProbe{
		done:             make(chan struct{}),
		markerUnreadable: true,
		vanished:         true,
		repo:             &config.RepoContext{Root: repoPath, ID: realID},
		completedAt:      time.Now(),
	}
	close(vanished.done)
	manager.mu.Lock()
	manager.rootHealProbes[config.RepoIDForRecordedRoot(repoPath)] = vanished
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	verdict := manager.rootAgentMaterializeVerdictFor(realID)
	if verdict.reason != rootAgentProjectUnresolved || !verdict.rootPathVanished {
		t.Fatalf("a vanished-mid-verification path must report unresolved with the vanished shape, got reason %d (vanished=%v)", verdict.reason, verdict.rootPathVanished)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "vanished") || !strings.Contains(detail, "bring the path back") {
		t.Fatalf("the remedy is the path, not the marker; got: %s", detail)
	}
	if strings.Contains(detail, "make the marker readable") {
		t.Fatalf("there is no marker to make readable — the path is gone; got: %s", detail)
	}
}

// TestSwappedCloneDoesNotInheritPersonalLayer pins the #3299 review's
// round-13 P1: for a main-root recorded project, the derived hash IS the
// occupant's real ID, so after a PROVEN mismatch the rejected project's
// personal layer sat exactly where the legacy sweep resolves — its enable
// (and program) would style a root inside a stranger's checkout. A disproven
// claim must not govern the occupant in either direction.
func TestSwappedCloneDoesNotInheritPersonalLayer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/dead-claim\"")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A different clone occupies the recorded path; the legacy root_agents
	// entry for that path is the OCCUPANT's legitimate opt-in.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init swapped clone: %v", err)
	}

	manager.EnsureRootAgents()
	manager.EnsureRootAgents()

	for _, opts := range *seen {
		if opts.Program == "/opt/dead-claim" {
			t.Fatalf("the rejected project's personal program must not style the occupant's root: %+v", opts)
		}
	}
}

// TestMismatchReleasesDeadClaimsUnreadableLatch pins the #3299 review's
// round-14 P2: a project whose personal config failed to load at boot latches
// its (derived == occupant-real, for main-root recordings) ID fail-closed —
// but once the checkout at the path PROVES to be a different clone, the dead
// claim's unreadable config cannot govern the occupant, whose own legacy
// opt-in must proceed.
func TestMismatchReleasesDeadClaimsUnreadableLatch(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = tr")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A different clone occupies the path; the legacy entry is ITS opt-in.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init swapped clone: %v", err)
	}

	manager.EnsureRootAgents()
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a proven mismatch must release the dead claim's unreadable latch for the occupant's legacy opt-in, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got == rootAgentPersonalUnreadable {
		t.Fatalf("the occupant's verdict must not send users to repair a dead project's config")
	}
}

// TestSameIDUnreadableMarkerFailsClosed pins the #3299 review's round-15 P1:
// for a main-root recording no verdict bridge is ever recorded (the derived
// hash IS the occupant's real ID), so the fail-closed predicate must check
// the DIRECT unresolved record's unreadable state — or a readable personal
// enabled=true survives an unverifiable marker and a legacy entry starts the
// claimant's program inside an unverified checkout.
func TestSameIDUnreadableMarkerFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not make a file unreadable for root")
	}
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/unverified\"")

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}
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
	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("an unverifiable same-ID checkout must fail closed against the legacy entry, got %d creates", len(*seen))
	}
}
