package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The #7829 fail-closed frames: deletes that name a legacy registry row whose
// recorded root cannot say which identity it was filed under. The absent
// branch keeps every gate master already had plus the task preflight across
// every fenced identity (R1) and the determinately-free selector (R5); the
// present-but-not-git branch is undeletable — it refuses at target
// resolution, with "nothing was changed" literally true.

// TestProvisionalDeleteRefusesWhileATaskBoundToTheHistoricalIdTargetsTheSweptRoot
// pins #7829 R1: the task preflight must cover EVERY identity the delete
// fences, not only the one the request was addressed by. A legacy row whose
// recorded root is absent is addressed by the derived d-<hex> id, yet its
// root_agents opt-in and its tasks sit under the historical path hash — the
// id everything recorded while the path resolved. A preflight that asks only
// the derived id sees no task, sweeps the opt-in the historical root
// materializes through, and strands the enabled task behind a successful
// delete. Both the swept opt-in and the stranded task name the SAME root,
// which is exactly what the preflight exists to refuse.
func TestProvisionalDeleteRefusesWhileATaskBoundToTheHistoricalIdTargetsTheSweptRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	historical := config.RepoIDFromRoot(filepath.Clean(repoPath))
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))
	if derived == historical || !config.IsDerivedRepoID(derived) {
		t.Fatalf("fixture needs disjoint derived/historical identities, got derived=%s historical=%s", derived, historical)
	}
	// The task binds while the checkout still resolves, which is how every
	// task created before the upgrade is keyed: its recorded RepoID is the
	// historical path hash, invisible to a preflight that only asks the
	// derived id.
	if err := task.AddTask(task.Task{
		ID: "historical-root-task", Name: "nightly-root", Prompt: "go",
		CronExpr: "0 3 * * *", Program: "claude",
		ProjectPath: repoPath, Enabled: true,
		TargetSession: session.RootSessionTitle,
	}); err != nil {
		t.Fatalf("seed root-targeted task: %v", err)
	}
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}
	// The durable opt-in is keyed by the recorded PATH, so it answers to the
	// historical hash through the sweep's path fallback — the identity the
	// enabled task expects its root under.
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}

	// The durable root_agents sweep must not run: the refusal claims nothing
	// was changed, so it must precede the durable mutation.
	swept := false
	previousSweep := deregisterRootAgents
	deregisterRootAgents = func(...string) ([]string, error) {
		swept = true
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = previousSweep })

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err = delErr; err == nil {
		t.Fatalf("a delete that sweeps the root_agents opt-in an enabled task's root materializes through must refuse, not strand the task behind success")
	}
	for _, want := range []string{"nothing was changed", "nightly-root", session.RootSessionTitle, "disable or retarget"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, err)
		}
	}
	if swept {
		t.Fatalf("the refusal must precede the durable root_agents sweep — it claims nothing was changed")
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("the registry row must survive the refused delete: %v", statErr)
	}
	manager.mu.Lock()
	_, suppressedDerived := manager.deletedRootRepos[derived]
	_, suppressedHistorical := manager.deletedRootRepos[historical]
	manager.mu.Unlock()
	if suppressedDerived || suppressedHistorical {
		t.Fatalf("a refused delete must suppress neither the derived nor the historical identity")
	}
}

// TestProvisionalDeleteRefusesWhileSessionsMayKeyUnderAnUncomputableIdentity
// pins #7829's final rule (PR #7853 review, third round): a legacy row whose
// recorded root is present but no longer a git checkout is UNDELETABLE — it
// refuses rather than deregisters. A recorded root that was a linked worktree
// of a BARE repository keyed its sessions under the bare common directory's
// id — an identity neither the derived id nor the recorded path's hash can
// recompute — and git's worktree ledger is lossy enough (a routine `git
// worktree prune` erases it) that no scan can establish which live sessions
// still belong to the root. So the delete refuses at target resolution with
// the root named and both remedies, before any gate or durable write, on
// either request form — and this session stays live to prove the refusal
// genuinely changed nothing.
func TestProvisionalDeleteRefusesWhileSessionsMayKeyUnderAnUncomputableIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	seed := filepath.Join(parent, "seed")
	if err := exec.Command("git", "init", seed).Run(); err != nil {
		t.Fatalf("git init seed: %v", err)
	}
	for _, args := range [][]string{
		{"-C", seed, "config", "user.email", "t@t"},
		{"-C", seed, "config", "user.name", "t"},
		{"-C", seed, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	bare := filepath.Join(parent, "bare.git")
	if err := exec.Command("git", "clone", "--bare", seed, bare).Run(); err != nil {
		t.Fatalf("git clone --bare: %v", err)
	}
	worktree := filepath.Join(parent, "wt")
	if err := exec.Command("git", "-C", bare, "worktree", "add", worktree).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	project := registerTestProject(t, worktree)
	// The recorded root is the linked worktree, but every session is keyed
	// under the BARE common directory — the identity the recorded path can no
	// longer recompute once .git is gone.
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	// The linked worktree's .git entry is a FILE pointing into the bare
	// common dir; removing it leaves a present directory that resolves to
	// nothing.
	if err := os.Remove(filepath.Join(worktree, ".git")); err != nil {
		t.Fatalf("remove worktree .git file: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	// A live session keyed under the bare repo's id — exactly how a session
	// created while the worktree resolved is filed — with its workspace at
	// the recorded root.
	inst, err := session.NewInstance(session.InstanceOptions{Title: "bare-session", Path: worktree, RepoIdentityPath: bare, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(bareID, "bare-session")] = inst
	manager.mu.Unlock()

	// The durable root_agents sweep must not run: the refusal claims nothing
	// was changed, so it must precede the durable mutation.
	swept := false
	previousSweep := deregisterRootAgents
	deregisterRootAgents = func(...string) ([]string, error) {
		swept = true
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = previousSweep })

	// Both forms must refuse: the path selector, and the derived RepoID the
	// record is filed under (#7829 R5 — it names the same row).
	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if err = delErr; err == nil {
			t.Fatalf("a legacy row whose recorded root is present but no longer a git checkout must refuse, not deregister (req %+v)", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "no longer a git checkout", "restore the checkout's git metadata", "remove the directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal must name %q (req %+v): %v", want, req, err)
			}
		}
		if swept {
			t.Fatalf("the refusal must precede the durable root_agents sweep — it claims nothing was changed (req %+v)", req)
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("a refused delete must archive, kill and deregister nothing (req %+v): archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
	}
	// The live session keyed under the uncomputable identity must survive —
	// the stranded gate cannot see it, so only the path evidence kept it.
	manager.mu.Lock()
	live := manager.instances[daemonInstanceKey(bareID, "bare-session")]
	manager.mu.Unlock()
	if live == nil || live.GetStatus() != session.Running {
		t.Fatalf("the live session under the bare identity must survive the refused delete")
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("the registry row must survive the refused delete: %v", statErr)
	}
	manager.mu.Lock()
	_, suppressedDerived := manager.deletedRootRepos[derived]
	_, suppressedBare := manager.deletedRootRepos[bareID]
	manager.mu.Unlock()
	if suppressedDerived || suppressedBare {
		t.Fatalf("a refused delete must suppress neither the derived nor the bare identity")
	}
}

// TestStaleDerivedIDDoesNotSelectARowWhoseRootResolvesAgain pins the boundary
// the derived-id match must hold (PR #7853 review R5 setup A, R13): the
// invented namespace exists for roots that cannot answer. Once the recorded
// root resolves again the row has a knowable real identity, and honoring the
// stale derived id would deregister the row while the opt-in sweep skips the
// path id — which a resolving checkout does not own under its path hash —
// leaving root_agents behind to recreate the project after restart. The
// request stays the no-op it was before the derived match existed: the row
// answers to its real id and its recorded path, not to this selector.
//
// The row must stay unreconciled THROUGH startup, which is why the recorded
// root is present-but-not-git while NewManager runs its snapshot —
// rootagent_snapshot reconciles a resolving root into the row's RepoID before
// the fixture can exercise the selector. The checkout is restored only after
// startup, without the marker proof reconciliation needs — exactly how
// production reaches the stale selector: .git returns before the row's
// RepoID is backfilled.
func TestStaleDerivedIDDoesNotSelectARowWhoseRootResolvesAgain(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))
	historical := config.RepoIDFromRoot(filepath.Clean(repoPath))
	realID := repoID(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(filepath.Join(repoPath, ".git")); err != nil {
		t.Fatalf("remove .git so startup cannot reconcile the row: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	// The checkout reappears now that startup is over: a fresh .git with no
	// checkout marker, so nothing backfills the row's identity — the derived
	// id it was filed under is simply stale.
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("restore the checkout after startup: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("the restore carries no marker proof, so the row must still be unreconciled, got %q", recorded)
	}
	if root, selErr := registeredProjectRootForRepoID(derived); selErr != nil || root != "" {
		t.Fatalf("a resolving root must not answer to a stale derived id, got root=%q err=%v", root, selErr)
	}
	var swept []string
	previousSweep := deregisterRootAgents
	deregisterRootAgents = func(ids ...string) ([]string, error) {
		swept = append(swept, ids...)
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = previousSweep })
	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoID: derived})
	if err = delErr; err != nil {
		t.Fatalf("the stale derived id must remain the idempotent no-op it was, not an error: %v", err)
	}
	for _, id := range swept {
		if id == historical || id == realID {
			t.Fatalf("the no-op may sweep only the invented id — %s belongs to the checkout that owns the path: swept %v", id, swept)
		}
	}
	if result.Deregistered || len(result.Archived) != 0 || len(result.Killed) != 0 {
		t.Fatalf("a selector that names an identity the row no longer is must change nothing: deregistered=%v archived=%d killed=%d",
			result.Deregistered, len(result.Archived), len(result.Killed))
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("the row must survive a delete that names an identity it no longer is: %v", statErr)
	}
	// And the opt-in must still attribute to the project — the resurrection
	// hazard is exactly a deregistered row under a live root_agents key.
	if !manager.repoRootAgentWillMaterialize(realID) {
		t.Fatalf("the root_agents opt-in at %s must still attribute to %s", repoPath, realID)
	}
}

// TestProvisionalDeleteRefusesWhileASessionKeysUnderAnIdentityItsPathsCannotReach
// pins the hardest present-but-not-git shape (PR #7853 review R6): the
// recorded root's own .git is gone, and the bare repository it was a linked
// worktree of still names it in worktrees/*/gitdir — evidence a routine `git
// worktree prune` erases, which is why the branch refuses rather than
// trusting any ownership scan. A session created from a SIBLING worktree of
// that bare repo carries none of the recorded root in its paths, so nothing
// af can read proves it belongs — and the delete refuses anyway, because
// nothing can prove it does not.
func TestProvisionalDeleteRefusesWhileASessionKeysUnderAnIdentityItsPathsCannotReach(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	seed := filepath.Join(parent, "seed")
	if err := exec.Command("git", "init", seed).Run(); err != nil {
		t.Fatalf("git init seed: %v", err)
	}
	for _, args := range [][]string{
		{"-C", seed, "config", "user.email", "t@t"},
		{"-C", seed, "config", "user.name", "t"},
		{"-C", seed, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	bare := filepath.Join(parent, "bare.git")
	if err := exec.Command("git", "clone", "--bare", seed, bare).Run(); err != nil {
		t.Fatalf("git clone --bare: %v", err)
	}
	worktree := filepath.Join(parent, "wt")
	sibling := filepath.Join(parent, "wt2")
	for _, wt := range []string{worktree, sibling} {
		if err := exec.Command("git", "-C", bare, "worktree", "add", "--detach", wt).Run(); err != nil {
			t.Fatalf("git worktree add %s: %v", wt, err)
		}
	}
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	// The recorded root loses its .git file; the sibling stays a real worktree,
	// and the bare repo's worktrees/<wt>/gitdir still names <root>/.git.
	if err := os.Remove(filepath.Join(worktree, ".git")); err != nil {
		t.Fatalf("remove worktree .git file: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	// The session's own paths sit BESIDE the recorded root: its workspace is
	// the sibling worktree, its identity path the bare common dir. Nothing it
	// records touches the root — the bare repo's worktree records do.
	inst, err := session.NewInstance(session.InstanceOptions{Title: "sibling-session", Path: sibling, RepoIdentityPath: bare, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(bareID, "sibling-session")] = inst
	manager.mu.Unlock()

	swept := false
	previousSweep := deregisterRootAgents
	deregisterRootAgents = func(...string) ([]string, error) {
		swept = true
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = previousSweep })

	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if err = delErr; err == nil {
			t.Fatalf("a legacy row whose recorded root is present but no longer a git checkout must refuse, not deregister (req %+v)", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "no longer a git checkout", "restore the checkout's git metadata", "remove the directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal must name %q (req %+v): %v", want, req, err)
			}
		}
		if swept {
			t.Fatalf("the refusal must precede the durable root_agents sweep — it claims nothing was changed (req %+v)", req)
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("a refused delete must archive, kill and deregister nothing (req %+v): archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
	}
	manager.mu.Lock()
	live := manager.instances[daemonInstanceKey(bareID, "sibling-session")]
	manager.mu.Unlock()
	if live == nil || live.GetStatus() != session.Running {
		t.Fatalf("the live session under the bare identity must survive the refused delete")
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("the registry row must survive the refused delete: %v", statErr)
	}
}

// TestProvisionalDeleteDoesNotBlameAnEnclosingWorkspaceSession pins the
// absent branch's restored master behavior (PR #7853 review R7/R9): an
// enclosing workspace is a bigger project, not the recorded root's owner. An
// absent legacy row beneath a live dotfiles checkout must delete as it did on
// master — a session whose paths merely contain the recorded root was never
// evidence it belonged to it.
func TestProvisionalDeleteDoesNotBlameAnEnclosingWorkspaceSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	container := filepath.Join(testguard.CanonicalTempDir(t), "dotfiles")
	repoPath := filepath.Join(container, "old")
	if err := exec.Command("git", "init", container).Run(); err != nil {
		t.Fatalf("git init container: %v", err)
	}
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init old: %v", err)
	}
	project := registerTestProject(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}
	containerID := repoID(t, container)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A live session of the ENCLOSING repository — the dotfiles repo rooted at
	// the container — whose workspace contains the absent recorded root.
	inst, err := session.NewInstance(session.InstanceOptions{Title: "dotfiles-session", Path: container, RepoIdentityPath: filepath.Join(container, ".git"), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(containerID, "dotfiles-session")] = inst
	manager.mu.Unlock()

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("a session whose workspace merely contains the recorded root must not refuse the delete — master completed it: %v", err)
	}
	if !result.Deregistered {
		t.Fatalf("the absent row must deregister exactly as on master")
	}
	manager.mu.Lock()
	live := manager.instances[daemonInstanceKey(containerID, "dotfiles-session")]
	manager.mu.Unlock()
	if live == nil || live.GetStatus() != session.Running {
		t.Fatalf("the enclosing repo's session must stay live — it was never this project's")
	}
}

// TestProvisionalDeleteLeavesAnOccupyingRootsCarryAndTicketsAlone pins PR
// #7853 review R5 setup B and R12: the derived selector matched only while
// the recorded root was determinately free, and the check is re-done as late
// as the durable sweep with the SAME verdict the optInIDs sweep requires.
// Whether a checkout resolves at the recorded root by then, or the probe can
// no longer give a verdict about it, the path may belong to a live occupant —
// path-keyed carry retirement and create-ticket withdrawal must not retire a
// stranger's state on the deleted project's behalf.
func TestProvisionalDeleteLeavesAnOccupyingRootsCarryAndTicketsAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		shim string // when set, poison git after the checkout appears
	}{
		{"the root resolves again", ""},
		{"the probe goes indeterminate", "#!/bin/sh\necho 'fatal: detected dubious ownership in repository' >&2\nexit 128\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			installOptionsRecordingBackend(t)
			repoPath := filepath.Join(testguard.CanonicalTempDir(t), "legacy-root")
			if err := exec.Command("git", "init", repoPath).Run(); err != nil {
				t.Fatalf("git init: %v", err)
			}
			project := registerTestProject(t, repoPath)
			clearRecordedRepoID(t, project.ID)
			// The recorded root is absent: determinately free, so the delete
			// still selects the row by its derived id.
			if err := os.RemoveAll(repoPath); err != nil {
				t.Fatalf("remove the checkout: %v", err)
			}
			manager, err := NewManager(config.DefaultConfig())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}

			// A checkout appears at the recorded root as the delete runs, and
			// the occupant already holds a carry and an in-flight create AT
			// that path — both under a different identity, so only the
			// path-keyed reach could touch them. In the indeterminate case git
			// then stops answering for it: the same "an occupant may own this
			// path" verdict the sweep reads.
			binDir := t.TempDir()
			deleteProjectPreSweepHookForTest = func() {
				if err := exec.Command("git", "init", repoPath).Run(); err != nil {
					t.Fatalf("git init occupant mid-delete: %v", err)
				}
				if tc.shim == "" {
					return
				}
				if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(tc.shim), 0o755); err != nil {
					t.Fatalf("write git shim: %v", err)
				}
				t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			t.Cleanup(func() { deleteProjectPreSweepHookForTest = nil })
			other := repoID(t, setupControlRepo(t))
			resolved := pathutil.ResolveForCompare(filepath.Clean(repoPath))
			manager.mu.Lock()
			manager.rootEnsureStates["occupant"] = &rootEnsureState{carry: &rootCarry{
				repoID:    other,
				workspace: repoPath,
				compare:   resolved,
			}}
			manager.rootCreatesInFlight["occupant-create"] = &rootCreateTicket{
				repoID:    other,
				workspace: repoPath,
				compare:   resolved,
			}
			manager.mu.Unlock()

			if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
				t.Fatalf("the row's delete still runs — the root was free when it was selected: %v", err)
			}
			manager.mu.Lock()
			st := manager.rootEnsureStates["occupant"]
			ticket := manager.rootCreatesInFlight["occupant-create"]
			manager.mu.Unlock()
			if st.carry == nil {
				t.Fatalf("a root whose recorded path is not determinately free at the sweep must not retire the occupant's carry at that path")
			}
			if ticket.withdrawn {
				t.Fatalf("a root whose recorded path is not determinately free at the sweep must not withdraw the occupant's in-flight create at that path")
			}
		})
	}
}

// TestDerivedIDDeleteRefusesAnIndeterminateProbeOfTheRecordedRoot pins PR
// #7853 review R14: when the request carries only the derived RepoID, the
// selector's only reach into the filesystem is the recorded root's
// RepoFromPath probe. A probe that fails WITHOUT a determinately-free
// verdict — git present but poisoned, the classic "dubious ownership" wall —
// means the derived id might still name the row, so the delete must refuse
// with a retry remedy rather than report a zero-count success over a state
// it could not read.
func TestDerivedIDDeleteRefusesAnIndeterminateProbeOfTheRecordedRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))
	clearRecordedRepoID(t, project.ID)
	// Keep the row unreconciled through startup (the checkout is restored
	// without marker proof only after NewManager has run), then poison git so
	// the selector's probe of the recorded root cannot give a verdict.
	if err := os.RemoveAll(filepath.Join(repoPath, ".git")); err != nil {
		t.Fatalf("remove .git so startup cannot reconcile the row: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("restore the checkout after startup: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	binDir := t.TempDir()
	shim := "#!/bin/sh\necho 'fatal: detected dubious ownership in repository' >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	swept := false
	previousSweep := deregisterRootAgents
	deregisterRootAgents = func(...string) ([]string, error) {
		swept = true
		return nil, nil
	}
	t.Cleanup(func() { deregisterRootAgents = previousSweep })

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoID: derived})
	if err = delErr; err == nil {
		t.Fatalf("a derived-id delete whose recorded-root probe cannot answer must refuse, not report a zero-count success")
	}
	for _, want := range []string{"nothing was changed", repoPath, "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, err)
		}
	}
	if swept {
		t.Fatalf("the refusal must precede the durable root_agents sweep — it claims nothing was changed")
	}
	if result.Deregistered || len(result.Archived) != 0 || len(result.Killed) != 0 {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); statErr != nil {
		t.Fatalf("the row must survive the refused delete: %v", statErr)
	}
}

// TestPresentNonGitRootProceedsOnlyOnAProvenMismatch pins both sides of the
// narrowed #7829 refusal — the boundary whose first cut regressed
// TestProvisionalDeleteKeepsItsIdentityOnAProvenMismatch. A marker read that
// SUCCEEDED and differed already answered the question the branch cannot
// answer itself: the checkout at the recorded root was provably a different
// project's, so the recorded identity stands and the delete proceeds exactly
// as it did on master. With that verdict withdrawn — no successful differing
// read anywhere — the identical shape is unproven, and the delete must
// refuse instead of acting on an identity it cannot name.
func TestPresentNonGitRootProceedsOnlyOnAProvenMismatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dropProbe bool
		wantErr   bool
	}{
		{"a proven mismatch proceeds under the recorded identity", false, false},
		{"an unproven identity refuses with nothing changed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoPath, derivedID, _ := pendingAttributionFixture(t, mismatchedProbe)
			if tc.dropProbe {
				// Same fixture minus the established verdict: no probe, and
				// the boot snapshot filed no mismatch while the root was
				// absent, so nothing anywhere proves the identity different.
				manager.mu.Lock()
				delete(manager.rootHealProbes, derivedID)
				manager.mu.Unlock()
			}

			swept := false
			previousSweep := deregisterRootAgents
			deregisterRootAgents = func(...string) ([]string, error) {
				swept = true
				return nil, nil
			}
			t.Cleanup(func() { deregisterRootAgents = previousSweep })

			result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a proven mismatch establishes something, so the delete must proceed: %v", err)
				}
				if result.RepoID != derivedID {
					t.Fatalf("a disproven candidate must not move the delete: got %s, want the recorded identity %s", result.RepoID, derivedID)
				}
				return
			}
			if err == nil {
				t.Fatalf("an unproven present-non-git root must refuse, not proceed on a guess about which identity the row was filed under")
			}
			for _, want := range []string{"nothing was changed", repoPath, "no longer a git checkout", "restore the checkout's git metadata", "remove the directory"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal must name %q: %v", want, err)
				}
			}
			if swept {
				t.Fatalf("the refusal must precede the durable root_agents sweep — it claims nothing was changed")
			}
			if result.Deregistered || len(result.Archived) != 0 || len(result.Killed) != 0 {
				t.Fatalf("a refused delete must archive, kill and deregister nothing")
			}
		})
	}
}
