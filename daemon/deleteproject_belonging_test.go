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
	"github.com/sachiniyer/agent-factory/session/git"
)

// The #7854 fail-closed frames: a legacy registry row whose recorded root is
// ABSENT. The delete used to decide belonging from the row's path-hash
// identity and the sessions' own paths, and both missed the same shape — a
// session of a bare repository created from a SIBLING worktree, keyed under
// the bare repo's identity with no path that touches the recorded root. The
// row could be deregistered while that session stayed live. The fix refuses
// on evidence af itself wrote — never git's worktree ledger, which `git
// worktree prune` erases on every archive.

// seedRepoFixture makes a plain repo with one commit so worktree adds and
// bare clones have a HEAD to point at.
func seedRepoFixture(t *testing.T, seed string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", seed},
		{"-C", seed, "config", "user.email", "t@t"},
		{"-C", seed, "config", "user.name", "t"},
		{"-C", seed, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

// bareWorktreeFixture builds seed → bare clone → two linked worktrees.
func bareWorktreeFixture(t *testing.T, parent string) (bare, worktree, sibling string) {
	t.Helper()
	seedRepoFixture(t, filepath.Join(parent, "seed"))
	bare = filepath.Join(parent, "bare.git")
	if err := exec.Command("git", "clone", "--bare", filepath.Join(parent, "seed"), bare).Run(); err != nil {
		t.Fatalf("git clone --bare: %v", err)
	}
	worktree = filepath.Join(parent, "wt")
	sibling = filepath.Join(parent, "wt2")
	if err := exec.Command("git", "-C", bare, "worktree", "add", worktree).Run(); err != nil {
		t.Fatalf("git worktree add %s: %v", worktree, err)
	}
	if err := exec.Command("git", "-C", bare, "worktree", "add", "--detach", sibling).Run(); err != nil {
		t.Fatalf("git worktree add %s: %v", sibling, err)
	}
	return bare, worktree, sibling
}

// liveSessionUnderIdentity injects a live session keyed under the repo
// identity repoID, with its durable Worktree.RepoPath — what the absent-root
// scan reads — resolved from a real in-place worktree at worktreePath: the
// bare common directory for a session of a bare repo, the main root
// otherwise.
func liveSessionUnderIdentity(t *testing.T, m *Manager, repoID, title, worktreePath string) *session.Instance {
	t.Helper()
	gw, _, err := git.NewGitWorktreeInPlace(worktreePath)
	if err != nil {
		t.Fatalf("NewGitWorktreeInPlace(%s): %v", worktreePath, err)
	}
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: worktreePath, RepoIdentityPath: gw.GetRepoPath(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetGitWorktreeForTest(gw)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// assertAbsentRowSurvives is the shared tail of every refused delete: the
// registry row stays, no identity is suppressed, and named live sessions
// keep running under their own keys.
func assertAbsentRowSurvives(t *testing.T, m *Manager, projectID string, fencedIDs []string, liveKeys [][2]string) {
	t.Helper()
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, projectID)); statErr != nil {
		t.Fatalf("the registry row must survive the refused delete: %v", statErr)
	}
	m.mu.Lock()
	for _, key := range liveKeys {
		inst := m.instances[daemonInstanceKey(key[0], key[1])]
		if inst == nil || inst.GetStatus() != session.Running {
			m.mu.Unlock()
			t.Fatalf("the live session %s must survive the refused delete", key[1])
		}
	}
	for _, id := range fencedIDs {
		if _, suppressed := m.deletedRootRepos[id]; suppressed {
			m.mu.Unlock()
			t.Fatalf("a refused delete must not suppress identity %s", id)
		}
	}
	m.mu.Unlock()
}

// TestAbsentRootDeleteRefusesWhileASiblingWorktreeSessionKeysUnderTheBareIdentity
// pins the reported shape: the recorded root is ABSENT, and a session created
// from a sibling worktree of the same bare repo is keyed under the bare
// identity — invisible to every gate master has. The row's own checkout
// marker lives inside that bare repo (registration minted it there), so the
// evidence says "could be this project's" and the delete refuses with the
// session and its remedy named — on either request form — instead of
// deregistering the row over a live session.
func TestAbsentRootDeleteRefusesWhileASiblingWorktreeSessionKeysUnderTheBareIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	historical := config.RepoIDFromRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	// The recorded root is gone outright — the ABSENT branch, where master
	// believed the row's path-hash identity and the sessions' own paths
	// decided belonging.
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
		{RepoID: historical},
	} {
		result, delErr := manager.DeleteProject(req)
		if delErr == nil {
			t.Fatalf("a legacy row with an absent recorded root must not deregister while a live session could be its own (req %+v)", req)
		}
		for _, want := range []string{"nothing was changed", worktree, "sibling-session", bare, "af sessions archive"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("the refusal must name %q (req %+v): %v", want, req, delErr)
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
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, historical, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteProceedsPastSessionsKeyedUnderUnrelatedRepositories pins
// the discrimination: an ordinary delete master completes must not start
// refusing because of an unrelated project's sessions. A session of a
// DIFFERENT registered clone carries a different checkout marker inside its
// bare root — durable af evidence that says "not this project" — and a
// non-bare session could never have been a worktree of the row's repo at all.
// Neither blocks the delete.
func TestAbsentRootDeleteProceedsPastSessionsKeyedUnderUnrelatedRepositories(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	_, worktree, _ := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	// A second, unrelated registered clone: its own bare repo and a linked
	// worktree registered as its own project, so its marker proves it is NOT
	// the clone the deleted row was registered against.
	otherParent := testguard.CanonicalTempDir(t)
	otherBare, otherWorktree, _ := bareWorktreeFixture(t, otherParent)
	registerTestProject(t, otherWorktree)
	otherBareID := repoID(t, otherBare)
	// And a plain non-bare repository session.
	normal := filepath.Join(testguard.CanonicalTempDir(t), "normal")
	seedRepoFixture(t, normal)
	normalID := repoID(t, normal)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, otherBareID, "unrelated-bare", otherWorktree)
	liveSessionUnderIdentity(t, manager, normalID, "unrelated-normal", normal)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr != nil {
		t.Fatalf("sessions of provably unrelated repositories must not block the delete: %v", delErr)
	}
	if !result.Deregistered {
		t.Fatalf("the delete must remove the row once every live identity is cleared: %+v", result)
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); !os.IsNotExist(statErr) {
		t.Fatalf("the registry row must be deregistered: %v", statErr)
	}
	manager.mu.Lock()
	for _, key := range [][2]string{{otherBareID, "unrelated-bare"}, {normalID, "unrelated-normal"}} {
		inst := manager.instances[daemonInstanceKey(key[0], key[1])]
		if inst == nil || inst.GetStatus() != session.Running {
			manager.mu.Unlock()
			t.Fatalf("the unrelated session %s must be untouched by the delete", key[1])
		}
	}
	manager.mu.Unlock()
}

// TestAbsentRootDeleteRefusesWhileANonBareSiblingSessionKeysUnderTheParentIdentity
// pins the shape the issue's bare framing misses: a NON-bare repository holds
// linked worktrees just the same, and a session created from a sibling
// worktree of that clone is keyed under the main root's identity — equally
// invisible to every master gate. The marker the row's registration minted
// into <mainroot>/.git proves this is the clone that owned the recorded
// root, so the delete refuses.
func TestAbsentRootDeleteRefusesWhileANonBareSiblingSessionKeysUnderTheParentIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	normal := filepath.Join(parent, "repo")
	seedRepoFixture(t, normal)
	worktree := filepath.Join(parent, "wt")
	sibling := filepath.Join(parent, "wt2")
	if err := exec.Command("git", "-C", normal, "worktree", "add", worktree).Run(); err != nil {
		t.Fatalf("git worktree add %s: %v", worktree, err)
	}
	if err := exec.Command("git", "-C", normal, "worktree", "add", "--detach", sibling).Run(); err != nil {
		t.Fatalf("git worktree add %s: %v", sibling, err)
	}
	project := registerTestProject(t, worktree)
	parentID := repoID(t, normal)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	historical := config.RepoIDFromRoot(filepath.Clean(worktree))
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
	liveSessionUnderIdentity(t, manager, parentID, "sibling-session", sibling)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a session of the clone that owns the recorded root must stop the delete even when that clone is not bare")
	}
	for _, want := range []string{"nothing was changed", "sibling-session", "af sessions archive"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, historical, parentID}, [][2]string{{parentID, "sibling-session"}})
}

// TestAbsentRootDeleteRefusesWhileASessionCannotNameItsRepository pins the
// fail-closed boundary: a live session whose repository identity root cannot
// be established — vanished, or never recorded — leaves "could it be the
// row's?" open, and open is suspect. The delete refuses and names it rather
// than deregistering over it.
func TestAbsentRootDeleteRefusesWhileASessionCannotNameItsRepository(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	_, worktree, sibling := bareWorktreeFixture(t, parent)
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
	// A live session keyed under a foreign identity whose recorded root is a
	// path that no longer exists: the scan cannot establish what clone that
	// was, so it cannot clear it.
	gone := filepath.Join(parent, "gone")
	inst, err := session.NewInstance(session.InstanceOptions{Title: "unattributed-session", Path: sibling, RepoIdentityPath: gone, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	foreignID := repoID(t, filepath.Join(parent, "bare.git"))
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(foreignID, "unattributed-session")] = inst
	manager.mu.Unlock()

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a session whose repository af cannot describe must stop the delete, not be cleared by silence")
	}
	for _, want := range []string{"nothing was changed", "unattributed-session", "af sessions archive"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, foreignID}, [][2]string{{foreignID, "unattributed-session"}})
}

// TestOrdinaryDeleteIsNotRefusedByAnUnrelatedBareSession pins the scan's
// scope: it exists only for a legacy row whose recorded root is absent. A
// normal project delete — even with a live session whose bare identity could
// never be cleared — is byte-for-byte the master path.
func TestOrdinaryDeleteIsNotRefusedByAnUnrelatedBareSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	normal := filepath.Join(parent, "normal")
	seedRepoFixture(t, normal)
	project := registerTestProject(t, normal)

	otherParent := testguard.CanonicalTempDir(t)
	otherBare, otherWorktree, _ := bareWorktreeFixture(t, otherParent)
	otherBareID := repoID(t, otherBare)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	liveSessionUnderIdentity(t, manager, otherBareID, "unrelated-bare", otherWorktree)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: normal})
	if delErr != nil {
		t.Fatalf("an ordinary delete must not consult other repositories' sessions at all: %v", delErr)
	}
	if !result.Deregistered {
		t.Fatalf("the ordinary delete must complete: %+v", result)
	}
	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, project.ID)); !os.IsNotExist(statErr) {
		t.Fatalf("the registry row must be deregistered: %v", statErr)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(otherBareID, "unrelated-bare")]
	manager.mu.Unlock()
	if inst == nil || inst.GetStatus() != session.Running {
		t.Fatalf("the unrelated bare session must be untouched")
	}
}
