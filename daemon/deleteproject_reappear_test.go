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

// ---------------------------------------------------------------------------
// #7925: a root that materializes between selection and the absence probe is
// a reappearance, not an "all clear". The row was selected BECAUSE its root
// was absent, so a present answer at probe time gets the #7829 predicate
// re-applied to the fresh observation: provably non-git WITH the recorded
// identity already disproven releases; every other answer — a resolving
// checkout, an unexplained plain dir, a git failure — refuses.
// ---------------------------------------------------------------------------

// materializeAtRoot swaps recordRootAbsentProbe for an observation that first
// materializes shape at the recorded root — the reappearance the probe-time
// re-check exists to catch — then answers like the real probe. The verdict
// wrapper captures the package var once per call, so every DeleteProject in
// the test sees exactly this window: after the #7829 check, inside the
// bounded observation itself.
func materializeAtRoot(t *testing.T, shape func(root string)) {
	t.Helper()
	previousProbe := recordRootAbsentProbe
	recordRootAbsentProbe = func(root string) (bool, error) {
		shape(root)
		return previousProbe(root)
	}
	t.Cleanup(func() { recordRootAbsentProbe = previousProbe })
}

// materializeAtPostClaimant installs shape on the post-claimant hook — the
// window between the claimant scan and the identity gates — so the
// materialization completes in the caller's own goroutine BEFORE the bounded
// root observation starts. Shapes that need a subprocess (a git init) must
// live here: inside the bounded observation they spend its budget, and a
// slow -race runner would time the verdict out and pin a different refusal
// (#7948 review R5).
func materializeAtPostClaimant(t *testing.T, shape func()) {
	t.Helper()
	previous := deleteProjectPostClaimantHookForTest
	deleteProjectPostClaimantHookForTest = shape
	t.Cleanup(func() { deleteProjectPostClaimantHookForTest = previous })
}

// removeReappearance undoes whatever a probe-time materialization left at the
// recorded root so the next request form selects on absence again — including
// the mode-000 shape, which RemoveAll cannot descend until its mode is
// repaired.
func removeReappearance(t *testing.T, root string) {
	t.Helper()
	if err := os.Chmod(root, 0o755); err != nil && !os.IsNotExist(err) {
		t.Fatalf("repair the reappeared root's mode: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove the reappeared root: %v", err)
	}
}

// TestAbsentRootDeleteRefusesWhenItsRootResolvesAgainBeforeTheOwnershipScan
// pins the #7925 fail-open closed at its newest edge: the selector proved the
// recorded root determinately free, then a fresh checkout initialized at the
// recorded path between the claimant scan and the identity gates. Falling
// through runs the durable removal over an occupant the absent-root scan
// never described. The refusal names the PATH form as the retry — for a
// request by the derived id "delete again" re-selects nothing once the root
// resolves and reports a zero-count success over the surviving row (review
// R7). The materialization lives on the post-claimant hook so it finishes
// before the bounded observation starts — a `git init` inside it spends its
// budget and can outlast it on a slow -race runner (#7948 review R5).
func TestAbsentRootDeleteRefusesWhenItsRootResolvesAgainBeforeTheOwnershipScan(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)

	materializeAtPostClaimant(t, func() {
		if err := exec.Command("git", "init", worktree).Run(); err != nil {
			t.Fatalf("materializing the reappeared checkout: %v", err)
		}
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if delErr == nil {
			t.Fatalf("request %+v: the root reappeared as a checkout before the ownership scan — falling through deregisters the row over sessions that were never vetted", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "resolved as a git checkout", "delete the project by its path " + worktree} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteRefusesWhenAPlainDirectoryAppearsBeforeTheOwnershipScan
// pins the Codex P1 on #7934 closed: a plain directory materializing at the
// recorded root cannot answer whether it is a checkout, and
// RepoFromPath-reporting-not-git alone is not the release. The only release
// is the #7829 predicate re-applied — provably non-git AND the recorded
// identity already disproven — and an unexplained dir meets only the first
// half, so the delete refuses and names both honest remedies.
func TestAbsentRootDeleteRefusesWhenAPlainDirectoryAppearsBeforeTheOwnershipScan(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)

	var shapeErr error
	materializeAtRoot(t, func(root string) {
		if err := os.MkdirAll(root, 0o755); err != nil && shapeErr == nil {
			shapeErr = err
		}
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if shapeErr != nil {
			t.Fatalf("request %+v: materializing the reappeared dir: %v", req, shapeErr)
		}
		if delErr == nil {
			t.Fatalf("request %+v: an unexplained dir at the recorded root is not the release — it proves nothing about who owns it", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "no longer a git checkout", "restore the checkout's git metadata", "delete the project by its path " + worktree, "remove the directory"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteRefusesWhenItsRootReappearsWithADanglingGitfile pins the
// git-failure half of the present answer: a .git pointer whose gitdir target
// is gone is a directory git cannot give a verdict about, and every
// non-answer refuses — nothing falls through to the deregister (review R3).
func TestAbsentRootDeleteRefusesWhenItsRootReappearsWithADanglingGitfile(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)

	var shapeErr error
	materializeAtRoot(t, func(root string) {
		if shapeErr != nil {
			return
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			shapeErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+filepath.Join(parent, "moved-away")+"\n"), 0o644); err != nil {
			shapeErr = err
		}
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if shapeErr != nil {
			t.Fatalf("request %+v: materializing the dangling gitfile: %v", req, shapeErr)
		}
		if delErr == nil {
			t.Fatalf("request %+v: a gitfile pointing at a vanished gitdir is a non-answer, and non-answers refuse", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "git cannot read it", "delete the project by its path " + worktree, "remove the directory"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteRefusesWhenItsRootReappearsUnreadable pins the
// permission half of the git-failure class: a directory the process cannot
// enter gives git no verdict either — the same non-answer, refused the same
// way.
func TestAbsentRootDeleteRefusesWhenItsRootReappearsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based unreadability is meaningless as root")
	}
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)

	var shapeErr error
	materializeAtRoot(t, func(root string) {
		if err := os.MkdirAll(root, 0o000); err != nil && shapeErr == nil {
			shapeErr = err
		}
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if shapeErr != nil {
			t.Fatalf("request %+v: materializing the unreadable dir: %v", req, shapeErr)
		}
		if delErr == nil {
			t.Fatalf("request %+v: a root git cannot enter is a non-answer, and non-answers refuse", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "git cannot read it", "delete the project by its path " + worktree, "remove the directory"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// installSettledMismatchProbe records the one observation that releases the
// #7829 refusal: a heal probe whose marker read at the recorded path already
// succeeded and differed — the checkout there was provably another
// project's. It settles WHICH identity the row's own sessions were filed
// under; it does not answer whether another live identity's sessions could
// belong to the root, which is why the released path still runs the
// belonging scan (#7948 review R1).
func installSettledMismatchProbe(t *testing.T, manager *Manager, repoID string) {
	t.Helper()
	disproven := &rootReattributionProbe{done: make(chan struct{}), mismatch: true}
	close(disproven.done)
	manager.mu.Lock()
	manager.rootHealProbes[repoID] = disproven
	manager.mu.Unlock()
}

// TestAbsentRootDeleteStillScansBelongingWhenTheReappearedRootIsProvablyNotItsCheckout
// pins the release's scope (#7948 review R1): the proven-mismatch observation
// releases the #7829 refusal ONLY — the #7854 belonging scan still runs, so
// a sibling-worktree session living under the bare clone's identity keeps
// refusing the delete. The scan is what answers who else could own the root;
// the disproof alone never did.
func TestAbsentRootDeleteStillScansBelongingWhenTheReappearedRootIsProvablyNotItsCheckout(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	inst := liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)
	installSettledMismatchProbe(t, manager, derived)

	var shapeErr error
	materializeAtRoot(t, func(root string) {
		if err := os.MkdirAll(root, 0o755); err != nil && shapeErr == nil {
			shapeErr = err
		}
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if shapeErr != nil {
			t.Fatalf("request %+v: materializing the reappeared dir: %v", req, shapeErr)
		}
		if delErr == nil {
			t.Fatalf("request %+v: a disproof releases the #7829 check, not the belonging scan — the delete must refuse over the sibling it never vetted", req)
		}
		for _, want := range []string{"nothing was changed", "sibling-session", bare, "checkout marker", inst.ID, sibling} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteProceedsWhenTheReappearedRootIsProvablyNotItsCheckout
// pins the one present answer that releases, with the belonging scan run and
// clean: the #7829 predicate re-applied to the fresh observation, over a
// daemon holding no live session that could belong. The row deregisters and
// the post-deregister re-scan the scan result carries has nothing unvetted
// to refuse.
func TestAbsentRootDeleteProceedsWhenTheReappearedRootIsProvablyNotItsCheckout(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	_, worktree, _ := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	// Deliberately no live session: with nothing the scan cannot clear, the
	// released delete proceeds and the row deregisters.
	installSettledMismatchProbe(t, manager, derived)

	var shapeErr error
	materializeAtRoot(t, func(root string) {
		if err := os.MkdirAll(root, 0o755); err != nil && shapeErr == nil {
			shapeErr = err
		}
	})

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if shapeErr != nil {
		t.Fatalf("materializing the reappeared dir: %v", shapeErr)
	}
	if delErr != nil {
		t.Fatalf("a proven-mismatched reappearance over a clean scan is the one present answer that releases: %v", delErr)
	}
	if !result.Deregistered {
		t.Fatalf("the released delete must deregister the row")
	}
}

// TestAbsentRootDeleteRefusesWhenTheReappearedRootResolvesDespiteADisproof
// pins the non-git half of the release predicate (#7948 review R3): a
// disproof must not release a reappearance that resolves as a checkout —
// the mutation `case m.recordedRootIdentityDisproven(repoID):` alone would
// let exactly this shape through. The refusal is the resolved-checkout one,
// so the scan-refusal copy cannot stand in for it.
func TestAbsentRootDeleteRefusesWhenTheReappearedRootResolvesDespiteADisproof(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)
	installSettledMismatchProbe(t, manager, derived)

	materializeAtPostClaimant(t, func() {
		if err := exec.Command("git", "init", worktree).Run(); err != nil {
			t.Fatalf("materializing the reappeared checkout: %v", err)
		}
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if delErr == nil {
			t.Fatalf("request %+v: a disproof releases only a provable non-git answer — a resolving checkout must refuse", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "resolved as a git checkout", "delete the project by its path " + worktree} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteRefusesWhenTheReappearedRootFailsGitDespiteADisproof
// pins the other half of the same predicate: a disproof must not release a
// git failure either — a dangling .git at the recorded root is still a
// non-answer, and it refuses with the path-form retry.
func TestAbsentRootDeleteRefusesWhenTheReappearedRootFailsGitDespiteADisproof(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)
	installSettledMismatchProbe(t, manager, derived)

	var shapeErr error
	materializeAtRoot(t, func(root string) {
		if shapeErr != nil {
			return
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			shapeErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+filepath.Join(parent, "moved-away")+"\n"), 0o644); err != nil {
			shapeErr = err
		}
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if shapeErr != nil {
			t.Fatalf("request %+v: materializing the dangling gitfile: %v", req, shapeErr)
		}
		if delErr == nil {
			t.Fatalf("request %+v: a disproof releases only a provable non-git answer — a git failure must refuse", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "git cannot read it", "delete the project by its path " + worktree, "remove the directory"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteRefusesWhenTheReappearedRootResolvesElsewhere pins the
// remedy's scope (#7948 review R4): "delete the project by its path" settles
// only for a checkout rooted at the recorded root. A path that resolves into
// an enclosing repository names what actually rooted there and gets the one
// remedy that settles — move it away, then delete as absent — because the
// path form would aim at the enclosing repository's project.
func TestAbsentRootDeleteRefusesWhenTheReappearedRootResolvesElsewhere(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	// The occupant: an enclosing repository the recorded path resolves INTO —
	// a directory inside its tree, linked from the recorded root, so git
	// names the enclosing root rather than the recorded one.
	enclosing := filepath.Join(parent, "enclosing")
	if err := exec.Command("git", "init", enclosing).Run(); err != nil {
		t.Fatalf("init the enclosing repository: %v", err)
	}
	inner := filepath.Join(enclosing, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("create the directory inside the enclosing repo: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)

	var shapeErr error
	materializeAtRoot(t, func(root string) {
		if shapeErr != nil {
			return
		}
		shapeErr = os.Symlink(inner, root)
	})

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if shapeErr != nil {
			t.Fatalf("request %+v: materializing the elsewhere-resolving link: %v", req, shapeErr)
		}
		if delErr == nil {
			t.Fatalf("request %+v: the root reappeared resolving elsewhere — falling through deregisters the row over sessions that were never vetted", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "resolved again", enclosing, "move it away", "as absent"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		// The occupant is rooted elsewhere, so the path form is no retry:
		// naming it would aim the delete at the enclosing repository's own
		// project. The remedy that settles is removing the path.
		if strings.Contains(delErr.Error(), "by its path") {
			t.Fatalf("request %+v: a root resolving elsewhere must not name the path form — it would delete the enclosing repo's project: %v", req, delErr)
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
		removeReappearance(t, worktree)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}
