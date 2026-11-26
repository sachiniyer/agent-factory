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
	"github.com/sachiniyer/agent-factory/task"
)

// The #7829 fail-closed frames: both deletes name a legacy registry row whose
// recorded root is present but no longer resolves, and both must refuse with
// "nothing was changed" literally true — the task preflight across every
// fenced identity (R1) and the path-evidence gate for the identity the path
// cannot recompute (R2). Each fixture is built so the pinned delete succeeds
// on master and on #7833's head: the former asks only the derived id's task
// bindings and checks only the identities the path can spell, the latter
// treats a present-but-unresolvable recorded root the same way.

// TestProvisionalDeleteRefusesWhileATaskBoundToTheHistoricalIdTargetsTheSweptRoot
// pins #7829 R1: the task preflight must cover EVERY identity the delete
// fences, not only the one the request was addressed by. A legacy row whose
// recorded root is present but no longer a checkout is addressed by the
// derived d-<hex> id, yet its root_agents opt-in and its tasks sit under the
// historical path hash — the id everything recorded while the path resolved.
// A preflight that asks only the derived id sees no task, sweeps the opt-in
// the historical root materializes through, and strands the enabled task
// behind a successful delete. Both the swept opt-in and the stranded task
// name the SAME root, which is exactly what the preflight exists to refuse.
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
	if err := os.RemoveAll(filepath.Join(repoPath, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
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
// pins #7829 R2: a derived-id delete may deregister only after checking every
// identity the row's sessions could be keyed under — and when that set cannot
// be known, it must fail closed on evidence that does not come from the path
// hash. A recorded root that was a linked worktree of a BARE repository keyed
// its sessions under the bare common directory's id — an identity neither the
// derived id nor the recorded path's hash can recompute — so the stranded
// gate above cannot see them, and deregistering removes the durable record
// while they stay live (#2549's orphan class). The fail-closed evidence is a
// live session whose paths touch the recorded root.
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
			t.Fatalf("a delete that cannot know which identity the row's sessions are keyed under must refuse while one is live at the recorded root (req %+v)", req)
		}
		for _, want := range []string{"nothing was changed", "bare-session", worktree, "archive those sessions first"} {
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
// the derived-id match must hold (PR #7853 review): the invented namespace
// exists for roots that cannot answer. Once the recorded root resolves again
// the row has a knowable real identity, and honoring the stale derived id
// would deregister the row while the opt-in sweep skips the path id — which a
// resolving checkout does not own under its path hash — leaving root_agents
// behind to recreate the project after restart. The request stays the no-op
// it was before the derived match existed: the row answers to its real id
// and its recorded path, not to this selector.
func TestStaleDerivedIDDoesNotSelectARowWhoseRootResolvesAgain(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, repoPath)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(repoPath))
	realID := repoID(t, repoPath)
	clearRecordedRepoID(t, project.ID)
	// The checkout stays in place and still resolves: the derived id the row
	// was once filed under is stale the moment the root answers for itself.
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	if root, selErr := registeredProjectRootForRepoID(derived); selErr != nil || root != "" {
		t.Fatalf("a resolving root must not answer to a stale derived id, got root=%q err=%v", root, selErr)
	}
	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoID: derived})
	if err = delErr; err != nil {
		t.Fatalf("the stale derived id must remain the idempotent no-op it was, not an error: %v", err)
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
// pins PR #7853 review R6: the recorded root's own .git is gone, but the bare
// repository it was a linked worktree of still names it in worktrees/*/gitdir —
// git's own evidence that this repository could key sessions at the root. A
// session created from a SIBLING worktree of that bare repo carries none of the
// recorded root in its paths — workspace, worktree, and repo path all sit
// beside it — so only the identity-root evidence can still see that it belongs.
// Without it the delete deregisters the row while the session stays live.
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
			t.Fatalf("a session keyed under the bare identity that owns the recorded root must refuse the delete even while none of its paths touch it (req %+v)", req)
		}
		for _, want := range []string{"nothing was changed", "sibling-session", worktree, "archive those sessions first"} {
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

// TestProvisionalDeleteDoesNotBlameAnEnclosingWorkspaceSession pins PR #7853
// review R7: an enclosing workspace is a bigger project, not the recorded
// root's owner. An absent legacy row beneath a live dotfiles checkout must
// delete as it did on master — the session's paths contain the root, which the
// old containing-direction read as belonging, but only git's own worktree
// records can make an enclosing path an identity that owns the root.
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
