package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// DeleteProject against the identity boundaries #3530 draws: which identity a
// request names while one is being decided, which one its durable sweep may
// remove an opt-in under, and which one a record filed before Project.RepoID
// can still be addressed by. Fixtures live in
// rootagent_promotion_deferral_test.go, next to the heal-path tests that share
// them.

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

// TestProvisionalDeleteFencesTheHistoricalIdentity pins #3530 review id
// 3919194996. The historical-session preflight and the fence installation used
// to be separate acquisitions of m.mu, and the fence went on the provisional id
// alone — so a checkout reappearing in between let a create reserve under the
// historical id and commit after the registry row was removed.
func TestProvisionalDeleteFencesTheHistoricalIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	historical := config.RepoIDFromRoot(filepath.Clean(repoPath))
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	provisional := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))

	// Observed while the delete runs — the durable sweep is the one call that
	// happens with the fences installed.
	fencedMidDelete := map[string]bool{}
	original := deregisterRootAgents
	deregisterRootAgents = func(ids ...string) ([]string, error) {
		manager.mu.Lock()
		for id := range manager.projectDeletes {
			fencedMidDelete[id] = true
		}
		manager.mu.Unlock()
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = original })

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject by the recorded path: %v", err)
	}
	if !fencedMidDelete[provisional] || !fencedMidDelete[historical] {
		t.Fatalf("a provisional delete must fence the historical identity too, or a create can reserve under it and commit after the row is gone; fenced %v", fencedMidDelete)
	}
	manager.mu.Lock()
	left := len(manager.projectDeletes)
	manager.mu.Unlock()
	if left != 0 {
		t.Fatalf("and it must remove exactly the fences it installed; %d left behind", left)
	}
}

// TestDeleteRefusesAnOperationalGitFailure pins #3530 review id 3919346198.
// A live checkout whose metadata git will not read exits normally, and reading
// that as "unresolved" sent the delete into the registry fallback — where it
// would archive and suppress a STALE row's project instead of the checkout the
// user selected.
func TestDeleteRefusesAnOperationalGitFailure(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	realID := repoID(t, repoPath)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// git COMPLETES with an operational failure for this path: a verdict about
	// the subprocess, not about the repository.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	binDir := t.TempDir()
	shim := "#!/bin/sh\ncase \" $* \" in\n  *\"" + repoPath + "\"*) echo 'fatal: detected dubious ownership in repository' >&2; exit 128 ;;\nesac\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err = manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err == nil {
		t.Fatalf("git failing to read a live checkout is not evidence that the path is unresolved; the delete must refuse rather than fall back to a registry row")
	}
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("the refusal must state that nothing was mutated: %v", err)
	}
	manager.mu.Lock()
	_, suppressed := manager.deletedRootRepos[realID]
	manager.mu.Unlock()
	if suppressed {
		t.Fatalf("and nothing may be suppressed")
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("nor may the record be removed: %v", statErr)
	}
}

// TestUnregisteredMissingProjectDeletesByItsHistoricalID pins #3530 review id
// 3919346222 — a regression this change introduced.
//
// The picker names an unregistered project whose root no longer resolves by the
// historical hash of that path and sends both selectors. Normalization invents
// d-H for it, and rejecting H against d-H made the row undeletable, where
// master (which used the same value for both) deleted it.
func TestUnregisteredMissingProjectDeletesByItsHistoricalID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	gone := filepath.Join(testguard.CanonicalTempDir(t), "removed-repo")
	historical := config.RepoIDFromRoot(filepath.Clean(gone))
	// A root_agents opt-in is all that names it — the not-yet-cloned shape the
	// picker also shows under the historical hash.
	manager, err := NewManager(rootTestConfig(gone, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: gone, RepoID: historical})
	if err != nil {
		t.Fatalf("the picker's own id and path for an unregistered missing project must still delete it: %v", err)
	}
	if result.RepoID != historical {
		t.Fatalf("and under the identity the user is looking at: got %s, want %s", result.RepoID, historical)
	}
}

// TestSweepRequiresADeterminateVerdictAboutTheRecordedPath pins #3530 review
// ids 3918379027 and 3919490145 together, because they are one rule: the
// recorded path's hash may be swept only when it is ESTABLISHED that no
// repository owns it.
//
// Absence establishes that, and TestReconciledDeleteSweepsTheRecordedPathOptIn
// covers it. A failed probe does not: a repository sitting at that path owns
// the hash whether git was killed (nothing was asked) or exited with an
// operational error (something else was answered), and sweeping on either
// deletes that repository's own opt-in on behalf of this project.
func TestSweepRequiresADeterminateVerdictAboutTheRecordedPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		shim string
	}{
		{"git never answers", "#!/bin/sh\nkill -9 $$\n"},
		{"git exits with an operational error", "#!/bin/sh\necho 'fatal: detected dubious ownership in repository' >&2\nexit 128\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			// A repository appears at the recorded root, and git will not give
			// a verdict about it — the window the sweep reads.
			binDir := t.TempDir()
			deleteProjectPreSweepHookForTest = func() {
				if _, statErr := os.Stat(worktree); statErr == nil {
					return
				}
				if err := exec.Command("git", "init", worktree).Run(); err != nil {
					t.Fatalf("git init occupant: %v", err)
				}
				if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(tc.shim), 0o755); err != nil {
					t.Fatalf("write git shim: %v", err)
				}
				t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			t.Cleanup(func() { deleteProjectPreSweepHookForTest = nil })

			if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree}); err != nil {
				t.Fatalf("DeleteProject by the recorded path: %v", err)
			}
			if len(swept) == 0 {
				t.Fatalf("fixture must reach the durable sweep, or it pins nothing")
			}
			for _, id := range swept {
				if id == pathID {
					t.Fatalf("the sweep supplied %s without a verdict about the path: a repository is there and owns that id, so this deletes its own opt-in", pathID)
				}
			}
		})
	}
}

// TestAbandonedRowClearsTheSelectionFlag pins #3530 review id 3919604370.
//
// A pathname match sets "a row was selected", but when claimantForRecord finds
// no proof the row is abandoned and its path dropped — so nothing will be
// deregistered. Leaving the flag set told the transition gate a project had
// been selected and skipped the reverse check, letting the delete act on the
// candidate identity while the unresolved row survived to reappear.
func TestAbandonedRowClearsTheSelectionFlag(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	derivedID := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// An unrelated checkout takes the path: present, so the record's claim is
	// unproven and its row will be abandoned, while a probe holds the candidate
	// identity it resolved.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init occupant: %v", err)
	}
	occupantID := repoID(t, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	pending := &rootReattributionProbe{done: make(chan struct{})}
	pending.candidate.Store(repo)
	manager.mu.Lock()
	manager.rootHealProbes[derivedID] = pending
	manager.mu.Unlock()

	// By PATH, so the claimant scan runs against the matched row and abandons
	// it — a RepoID-only request never selects the row at all and so cannot
	// exercise this branch.
	_, err = manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err == nil {
		t.Fatalf("the row was abandoned, so no project was selected: the reverse transition check must run and refuse rather than act on %s", occupantID)
	}
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("the refusal must state that nothing was mutated: %v", err)
	}
}

// TestAbsentPathDeletesEvenWhenGitCannotRun pins #3530 review id 3919749878.
//
// Absence is independent evidence: a path that provably holds nothing cannot be
// hiding a checkout, whichever way git failed. Asking the unanswered-probe
// question first made a sessionless missing project undeletable for as long as
// the git executable was unavailable.
func TestAbsentPathDeletesEvenWhenGitCannotRun(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	realID := repoID(t, repoPath)
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Every git from here on dies on a signal: nothing it is asked is answered.
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

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("a provably absent path is evidence in its own right; the delete must proceed on the record's identity: %v", err)
	}
	if result.RepoID != realID {
		t.Fatalf("and under the identity the record wrote down: got %s, want %s", result.RepoID, realID)
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr == nil {
		t.Fatalf("and the durable record must be removed")
	}
}

// TestReconciliationDuringADeleteStillDeregisters pins #3530 review id
// 3919900658.
//
// The selectors are resolved BEFORE the delete's fence, because they run Git
// probes and #3361 forbids holding taskTargetMu while one waits on a stalled
// mount. A reconciliation landing in that window writes an identity onto a row
// this delete had already failed to find — permanently — so without a second
// look the delete archives and suppresses the identity while the row brings the
// project back on the next start.
func TestReconciliationDuringADeleteStillDeregisters(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	realID := repoID(t, repoPath)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// With the row still filed under its provisional identity, a delete by the
	// REAL one finds no root.
	if root, err := registeredProjectRootForRepoID(realID); err != nil || root != "" {
		t.Fatalf("fixture must start with no row answering to %s, got %q (%v)", realID, root, err)
	}

	// The healer reconciles the row while the delete is in flight — after its
	// selectors resolved, after its fence went in.
	reconciled := false
	deleteProjectPreSweepHookForTest = func() {
		if reconciled {
			return
		}
		reconciled = true
		if _, err := config.ReconcileProjectRepoID(project.ID, realID); err != nil {
			t.Fatalf("reconcile mid-delete: %v", err)
		}
	}
	t.Cleanup(func() { deleteProjectPreSweepHookForTest = nil })

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoID: realID}); err != nil {
		t.Fatalf("DeleteProject by the real identity: %v", err)
	}
	if !reconciled {
		t.Fatalf("fixture never reached the window, so it pins nothing")
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr == nil {
		t.Fatalf("the row gained this delete's identity while it ran, so the delete must take it too — otherwise the project returns on the next start with its sessions archived")
	}
}
