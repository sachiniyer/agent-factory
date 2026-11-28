package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// The #7854 fail-closed frames: a legacy registry row whose recorded root is
// ABSENT. The delete used to decide belonging from the row's path-hash
// identity and the sessions' own paths, and both missed the same shape — a
// session of a bare repository created from a SIBLING worktree, keyed under
// the bare identity with no path that touches the recorded root. The row
// could be deregistered while that session stayed live. The fix refuses on
// evidence af itself wrote — never git's worktree ledger, which `git
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
	inst := runningInstance(t, title, worktreePath, gw.GetRepoPath())
	inst.SetGitWorktreeForTest(gw)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// runningInstance builds a live instance row the way a restored or in-flight
// session can look: a recorded identity root and no resolvable worktree
// backend behind it.
func runningInstance(t *testing.T, title, path, identityRoot string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: path, RepoIdentityPath: identityRoot, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
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
// from a sibling worktree of the same bare clone is keyed under the bare
// identity — invisible to every master gate, while the marker the row's
// registration minted into the bare common directory proves this is the clone
// that owned the root.
func TestAbsentRootDeleteRefusesWhileASiblingWorktreeSessionKeysUnderTheBareIdentity(t *testing.T) {
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

	// Both request forms that deregister the row reach the same refusal: by
	// path, and by the derived identity the row's reconciler invents.
	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: derived},
	} {
		result, delErr := manager.DeleteProject(req)
		if delErr == nil {
			t.Fatalf("request %+v: the delete deregistered the row while a sibling-worktree session stayed live — the reported bug", req)
		}
		for _, want := range []string{"nothing was changed", "sibling-session", bare, "checkout marker", "af sessions archive --repo"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "sibling-session"}})
}

// TestAbsentRootDeleteProceedsPastSessionsKeyedUnderUnrelatedRepositories
// pins the discrimination: sessions that provably belong to OTHER clones —
// one whose bare common dir carries a DIFFERENT registered checkout marker,
// and one a plain non-bare repo that never hosted a registration — must not
// block the delete. The row is deregistered and the unrelated sessions stay
// live.
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

	// A SECOND registered project whose sessions key under its own bare
	// clone: its marker differs from the deleted row's, which is the durable
	// proof it is a different clone.
	otherParent := testguard.CanonicalTempDir(t)
	otherBare, otherWorktree, _ := bareWorktreeFixture(t, otherParent)
	registerTestProject(t, otherWorktree)
	otherBareID := repoID(t, otherBare)
	// And a session of an ordinary non-bare repo, keyed under its main root.
	normal := filepath.Join(otherParent, "normal")
	seedRepoFixture(t, normal)
	normalID := repoID(t, normal)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if recorded := onlyIdentityFor(t, project.ID); recorded != "" {
		t.Fatalf("fixture must keep the row unreconciled, got %q", recorded)
	}
	liveSessionUnderIdentity(t, manager, otherBareID, "other-bare-session", otherWorktree)
	liveSessionUnderIdentity(t, manager, normalID, "other-normal-session", normal)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr != nil {
		t.Fatalf("unrelated sessions must not block the delete: %v", delErr)
	}
	if !result.Deregistered {
		t.Fatalf("the absent-row delete must deregister the row once no possibly-belonging session survives")
	}
	manager.mu.Lock()
	for _, key := range [][2]string{{otherBareID, "other-bare-session"}, {normalID, "other-normal-session"}} {
		inst := manager.instances[daemonInstanceKey(key[0], key[1])]
		if inst == nil || inst.GetStatus() != session.Running {
			manager.mu.Unlock()
			t.Fatalf("unrelated session %s must stay live through the delete", key[1])
		}
	}
	manager.mu.Unlock()
}

// TestAbsentRootDeleteRefusesWhileAPre3358SessionKeysUnderTheClonesParent
// pins the narrow second shape: sessions created before #3358 — or still
// minted by pre-#3358 tasks — are keyed under the bare clone's PARENT
// directory, so the row's marker does not live at <root>/.git but one level
// down, inside the bare clone the parent holds. The review's shape is a
// dotfiles home, so the parent here is itself a checkout.
func TestAbsentRootDeleteRefusesWhileAPre3358SessionKeysUnderTheClonesParent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	// The parent is a repository in its own right — the dotfiles checkout the
	// review describes — so its own common dir is provably not the row's.
	if err := exec.Command("git", "init", parent).Run(); err != nil {
		t.Fatalf("git init %s: %v", parent, err)
	}
	bare, worktree, _ := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	parentID := repoID(t, parent)
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
	// The pre-#3358 row persisted the unrelated parent as BOTH the workspace
	// and the identity root (manager_create_legacy_bare.go).
	inst := runningInstance(t, "legacy-sibling", parent, parent)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(parentID, "legacy-sibling")] = inst
	manager.mu.Unlock()

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a session keyed under the clone's parent is as suspect as one keyed under the clone — the marker lives one level down")
	}
	for _, want := range []string{"nothing was changed", "legacy-sibling", "checkout marker", bare, "af sessions archive --repo"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, parentID}, [][2]string{{parentID, "legacy-sibling"}})
}

// TestAbsentRootDeleteRefusesWhenItsAbsenceProbeCannotAnswer pins the gate's
// own contract: the bounded absence probe reporting "unanswered" must not be
// read as "present", which is what selects the unscanned path and orphans
// exactly like master's delete. A recorded root whose stat stalls past the
// bound gets a refusal whose remedy is the retry that gets a responsive
// filesystem.
func TestAbsentRootDeleteRefusesWhenItsAbsenceProbeCannotAnswer(t *testing.T) {
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

	previousTimeout := boundedRecordRootAbsentTimeout
	boundedRecordRootAbsentTimeout = 20 * time.Millisecond
	previousProbe := recordRootAbsentProbe
	t.Cleanup(func() {
		boundedRecordRootAbsentTimeout = previousTimeout
		recordRootAbsentProbe = previousProbe
	})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// An observation that never returns, which is what an unresponsive mount
	// gives: the syscall cannot be cancelled, so only the WAIT can be bounded.
	recordRootAbsentProbe = func(string) (bool, error) {
		<-release
		return true, nil
	}

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a probe that did not answer is not 'present' — the delete must refuse, not select the unscanned path")
	}
	for _, want := range []string{"nothing was changed", "retry"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived}, nil)
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
	foreignID := repoID(t, filepath.Join(parent, "bare.git"))
	inst := runningInstance(t, "unattributed-session", sibling, gone)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(foreignID, "unattributed-session")] = inst
	manager.mu.Unlock()

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a session whose repository af cannot describe must stop the delete, not be cleared by silence")
	}
	for _, want := range []string{"nothing was changed", "unattributed-session", "cannot describe", "af sessions archive"} {
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

// TestAbsentRootDeleteReportsSessionsThatMaterializedDuringItsDeregister
// pins the post-deregister half: the cleared set is computed at resolution,
// so a session that lands between it and the row coming off arrives under an
// identity nothing vetted. It is classified the same way the resolution scan
// does — and the report names the archive that resolves it, because a retry
// finds no row to vet it against.
func TestAbsentRootDeleteReportsSessionsThatMaterializedDuringItsDeregister(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
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

	// The session lands in the gap between the pre-deregister re-check and
	// the durable removal — the window the post-deregister re-scan exists
	// for. The hook stretches it.
	previousHook := deleteProjectPreDeregisterHookForTest
	deleteProjectPreDeregisterHookForTest = func() {
		liveSessionUnderIdentity(t, manager, bareID, "late-sibling", sibling)
	}
	t.Cleanup(func() { deleteProjectPreDeregisterHookForTest = previousHook })

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a session that materialized mid-delete under an unvetted identity must not be reported as success")
	}
	if !result.Deregistered {
		t.Fatalf("the row still comes off — the report is over what survived it")
	}
	for _, want := range []string{"late-sibling", "af sessions archive --repo"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the report must name %q: %v", want, delErr)
		}
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(bareID, "late-sibling")]
	manager.mu.Unlock()
	if inst == nil || inst.GetStatus() != session.Running {
		t.Fatalf("the late session stays live — the delete only reports it")
	}
}

// TestOrdinaryDeleteIsNotRefusedByAnUnrelatedBareSession pins the scan's
// scope: it exists only for a legacy row whose recorded root is absent. A
// normal project delete — even with a live session whose bare identity could
// theoretically parent the row — completes on master terms.
func TestOrdinaryDeleteIsNotRefusedByAnUnrelatedBareSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	repoPath := filepath.Join(parent, "repo")
	seedRepoFixture(t, repoPath)
	registerTestProject(t, repoPath)
	realID := repoID(t, repoPath)

	otherParent := testguard.CanonicalTempDir(t)
	otherBare, otherWorktree, _ := bareWorktreeFixture(t, otherParent)
	otherBareID := repoID(t, otherBare)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	liveSessionUnderIdentity(t, manager, otherBareID, "other-bare", otherWorktree)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoID: realID, RepoPath: repoPath})
	if delErr != nil {
		t.Fatalf("an ordinary delete with an unrelated bare session must complete: %v", delErr)
	}
	if !result.Deregistered {
		t.Fatalf("the ordinary delete must deregister its row")
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(otherBareID, "other-bare")]
	manager.mu.Unlock()
	if inst == nil || inst.GetStatus() != session.Running {
		t.Fatalf("the unrelated session must stay live")
	}
}
