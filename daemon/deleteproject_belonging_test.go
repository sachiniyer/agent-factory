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

// markerCheckoutID reads the checkout id a registration minted into the
// clone's common dir — the value a row records, which the scan later sweeps
// for under ANY home's file name.
func markerCheckoutID(t *testing.T, commonDir string) string {
	t.Helper()
	ids, present, err := config.CheckoutIDsInCommonDir(commonDir)
	if err != nil || !present || len(ids) == 0 {
		t.Fatalf("the fixture clone's marker must read: ids=%v present=%v err=%v", ids, present, err)
	}
	return ids[0]
}

// fakeCommonDir builds a git-shaped metadata directory — HEAD beside
// objects/ and refs/, the shape a resolved common directory must show — and,
// when checkoutID is non-empty, an agent-factory marker directory holding it
// under a real marker file name.
func fakeCommonDir(t *testing.T, dir, checkoutID string) {
	t.Helper()
	for _, sub := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if checkoutID != "" {
		markerDir := filepath.Join(dir, "agent-factory")
		if err := os.MkdirAll(markerDir, 0o755); err != nil {
			t.Fatalf("mkdir marker dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(markerDir, "checkout-id-"+strings.Repeat("b", 32)), []byte(checkoutID+"\n"), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
}

// sessionUnderRoot keys a live session under the repo identity root names —
// the shape a session has when af's recorded identity for it is a directory
// the scan then has to describe.
func sessionUnderRoot(t *testing.T, m *Manager, title, root string) string {
	t.Helper()
	id := config.RepoIDFromRoot(filepath.Clean(root))
	inst := runningInstance(t, title, root, root)
	m.mu.Lock()
	m.instances[daemonInstanceKey(id, title)] = inst
	m.mu.Unlock()
	return id
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
	inst := liveSessionUnderIdentity(t, manager, bareID, "sibling-session", sibling)

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
		for _, want := range []string{"nothing was changed", "sibling-session", bare, "checkout marker", "cannot be archived", "stopped or removed by the user", inst.ID, sibling} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		// The suspect here is an in-place session — archive.go refuses to
		// relocate an external worktree — so the refusal must print NO
		// archive command at all: a bare `af sessions archive <title>`
		// resolves by cwd and could land on a same-titled session elsewhere.
		if strings.Contains(delErr.Error(), "af sessions archive") {
			t.Fatalf("request %+v: an unarchivable suspect must get no archive command: %v", req, delErr)
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
	// The parent is a repository in its own right whose path provably hashes
	// to this session's key, so --repo may be printed — but the bare-title
	// form, which resolves by cwd onto a possibly different session, must
	// not appear anywhere.
	if strings.Contains(delErr.Error(), "af sessions archive -- ") {
		t.Fatalf("the refusal must never print a bare-title command: %v", delErr)
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
	for _, want := range []string{"nothing was changed", "unattributed-session", "cannot describe", "TUI or web UI"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	// A root af cannot describe yields no provable --repo: the refusal names
	// the session for the UI surfaces instead of printing a command that
	// could reach a different session.
	if strings.Contains(delErr.Error(), "af sessions archive") {
		t.Fatalf("a root af cannot describe must get no archive command: %v", delErr)
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
	for _, want := range []string{"late-sibling", "cannot be archived", "stopped or removed by the user"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the report must name %q: %v", want, delErr)
		}
	}
	// The late suspect is in-place here — unarchivable — so the report must
	// name it without printing an archive command that would be refused.
	if strings.Contains(delErr.Error(), "af sessions archive") {
		t.Fatalf("an unarchivable escaped session must get no archive command: %v", delErr)
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

// TestConcurrentAbsentRootDeletesProbeEachRootWithItsOwnCheckoutID pins the
// ownership flight's key: an observation is shared only between callers
// asking the SAME question — the same root AND the same checkout id. Two
// absent-rooted rows carry different recorded checkouts, and the live
// session's repo holds the SECOND row's marker: a verdict shared by root
// alone would hand the second delete the first row's "cleared", letting its
// row come off while the session it exists to protect stays live (#7914
// review).
func TestConcurrentAbsentRootDeletesProbeEachRootWithItsOwnCheckoutID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	// Row A's marker lives in a clone nothing else here shares; probing the
	// session's repo against it clears.
	parentA := testguard.CanonicalTempDir(t)
	_, worktreeA, _ := bareWorktreeFixture(t, parentA)
	rowA := registerTestProject(t, worktreeA)
	clearRecordedRepoID(t, rowA.ID)
	if err := os.RemoveAll(worktreeA); err != nil {
		t.Fatalf("remove row A's recorded root: %v", err)
	}

	// Row C's marker lives in the clone the live session keys under; probing
	// the same root against ITS checkout id must answer owner.
	parentC := testguard.CanonicalTempDir(t)
	cloneC, worktreeC, siblingC := bareWorktreeFixture(t, parentC)
	rowC := registerTestProject(t, worktreeC)
	cID := repoID(t, cloneC)
	clearRecordedRepoID(t, rowC.ID)
	if err := os.RemoveAll(worktreeC); err != nil {
		t.Fatalf("remove row C's recorded root: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	liveSessionUnderIdentity(t, manager, cID, "c-session", siblingC)

	// Park every probe at entry until a SECOND probe run arrives, or a
	// deadline well under the await budget frees the parked one. A flight
	// keyed by root alone produces exactly one probe run — row C's delete
	// joins row A's parked observation and inherits its cleared verdict;
	// keyed by (root, checkoutID) each delete runs its own, and the second
	// entry is what releases both.
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	firstSeen := make(chan struct{})
	previousHook := probeRootBelongingHookForTest
	probeRootBelongingHookForTest = func(string) {
		entered <- struct{}{}
		<-release
	}
	go func() {
		<-entered
		close(firstSeen)
		select {
		case <-entered:
		case <-time.After(1500 * time.Millisecond):
		}
		close(release)
	}()
	t.Cleanup(func() { probeRootBelongingHookForTest = previousHook })

	type outcome struct {
		deregistered bool
		err          error
	}
	deleteA := make(chan outcome, 1)
	go func() {
		result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktreeA})
		deleteA <- outcome{deregistered: result.Deregistered, err: err}
	}()

	// Row A's probe must be parked before row C's delete asks its question —
	// the buggy key then parks THE observation row C's delete would join.
	<-firstSeen
	deleteC := make(chan outcome, 1)
	go func() {
		result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktreeC})
		deleteC <- outcome{deregistered: result.Deregistered, err: err}
	}()

	outA := <-deleteA
	if outA.err != nil || !outA.deregistered {
		t.Fatalf("row A's delete probes the session's repo against a marker it does not hold and completes: %+v", outA)
	}
	outC := <-deleteC
	if outC.err == nil || outC.deregistered {
		t.Fatalf("row C's delete must run its own probe, find its own checkout marker in the session's repo, and refuse — a shared verdict deregistered it over a live session")
	}
	for _, want := range []string{"nothing was changed", "c-session", "checkout marker"} {
		if !strings.Contains(outC.err.Error(), want) {
			t.Fatalf("row C's refusal must name %q: %v", want, outC.err)
		}
	}

	dir, dirErr := config.ProjectRegistryDir()
	if dirErr != nil {
		t.Fatalf("ProjectRegistryDir: %v", dirErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, rowC.ID)); statErr != nil {
		t.Fatalf("row C must survive the refused delete: %v", statErr)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(cID, "c-session")]
	manager.mu.Unlock()
	if inst == nil || inst.GetStatus() != session.Running {
		t.Fatalf("the live session under the probed repo must stay live")
	}
}

// TestIdentityRootCommonDirResolvesGitfileIndirections pins the .git-entry
// layouts the marker sweep must describe rather than report unproven: a .git
// symlink — to a metadata directory or to a redirect file — and a gitdir
// pointer whose target path legitimately ends in whitespace (session/git's
// resolveRepoMetadataRoot and parseGitdirPointerLine semantics, #7914
// review).
func TestIdentityRootCommonDirResolvesGitfileIndirections(t *testing.T) {
	parent := testguard.CanonicalTempDir(t)

	// A .git symlink to the metadata directory itself — git accepts the
	// layout, and the link must be followed before classifying.
	metaReal := filepath.Join(parent, "meta-real")
	if err := os.MkdirAll(metaReal, 0o755); err != nil {
		t.Fatalf("mkdir metadata: %v", err)
	}
	symlinked := filepath.Join(parent, "symlinked")
	if err := os.MkdirAll(symlinked, 0o755); err != nil {
		t.Fatalf("mkdir symlinked root: %v", err)
	}
	if err := os.Symlink(metaReal, filepath.Join(symlinked, ".git")); err != nil {
		t.Fatalf("symlink .git: %v", err)
	}
	if common, shape, reason := identityRootCommonDir(symlinked); shape != shapeCommonDir || common != metaReal {
		t.Fatalf("a .git symlink to a metadata dir must resolve: common=%q shape=%v reason=%q", common, shape, reason)
	}

	// A .git symlink to a redirect FILE resolves the file's pointer.
	separate := filepath.Join(parent, "separate")
	fakeCommonDir(t, separate, "")
	redirect := filepath.Join(parent, "redirect.git")
	if err := os.WriteFile(redirect, []byte("gitdir: "+separate+"\n"), 0o644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	linkRoot := filepath.Join(parent, "linkroot")
	if err := os.MkdirAll(linkRoot, 0o755); err != nil {
		t.Fatalf("mkdir linkroot: %v", err)
	}
	if err := os.Symlink(redirect, filepath.Join(linkRoot, ".git")); err != nil {
		t.Fatalf("symlink redirect: %v", err)
	}
	if common, shape, reason := identityRootCommonDir(linkRoot); shape != shapeCommonDir || common != separate {
		t.Fatalf("a .git symlink to a redirect file must resolve its pointer: common=%q shape=%v reason=%q", common, shape, reason)
	}

	// A gitdir target whose path ends in a space must survive the parse —
	// trimming it names a directory that does not exist, and its missing
	// markers then read as proof of a different clone.
	spaced := filepath.Join(parent, "meta ")
	fakeCommonDir(t, spaced, "")
	spacedRoot := filepath.Join(parent, "spacedroot")
	if err := os.MkdirAll(spacedRoot, 0o755); err != nil {
		t.Fatalf("mkdir spacedroot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(spacedRoot, ".git"), []byte("gitdir: "+spaced+"\n"), 0o644); err != nil {
		t.Fatalf("write spaced pointer: %v", err)
	}
	if common, shape, reason := identityRootCommonDir(spacedRoot); shape != shapeCommonDir || common != filepath.Clean(spaced) {
		t.Fatalf("a whitespace-suffixed gitdir target must be preserved: common=%q shape=%v reason=%q", common, shape, reason)
	}

	// A linked-worktree pointer resolves through the gitdir's commondir
	// file — the way git actually answers --git-common-dir — to the real
	// common directory.
	common := filepath.Join(parent, "common")
	fakeCommonDir(t, common, "")
	leaf := filepath.Join(common, "worktrees", "leaf")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir worktree leaf: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leaf, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	wtRoot := filepath.Join(parent, "wtroot")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		t.Fatalf("mkdir wtroot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtRoot, ".git"), []byte("gitdir: "+leaf+"\n"), 0o644); err != nil {
		t.Fatalf("write worktree pointer: %v", err)
	}
	if got, shape, reason := identityRootCommonDir(wtRoot); shape != shapeCommonDir || got != common {
		t.Fatalf("a linked-worktree pointer must resolve through commondir: common=%q shape=%v reason=%q", got, shape, reason)
	}
}

// TestAbsentRootDeleteRefusesWhileASessionsGitfilePointsUnderAWorktreesName
// pins the narrow fail-open in resolving gitdir pointers by shape alone: a
// --separate-git-dir metadata tree can legitimately sit under a directory
// NAMED worktrees. Resolving the real common dir the way git does — through
// commondir, not by climbing the path — finds the row's marker inside it;
// climbing reads a parent that holds no marker and clears a possibly-owning
// root.
func TestAbsentRootDeleteRefusesWhileASessionsGitfilePointsUnderAWorktreesName(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, _ := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	chk := markerCheckoutID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The session's identity root carries a .git redirect file whose gitdir
	// lives under a directory named "worktrees" — a --separate-git-dir
	// layout, not a linked worktree. Its gitdir IS the common directory,
	// and it holds the row's marker.
	fakeParent := testguard.CanonicalTempDir(t)
	metadata := filepath.Join(fakeParent, "worktrees", "proj")
	fakeCommonDir(t, metadata, chk)
	root := filepath.Join(fakeParent, "checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+metadata+"\n"), 0o644); err != nil {
		t.Fatalf("write .git pointer: %v", err)
	}
	rootID := sessionUnderRoot(t, manager, "sepgit-session", root)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a gitdir under a worktrees-named directory must resolve via commondir — the real common dir holds this record's marker")
	}
	for _, want := range []string{"nothing was changed", "sepgit-session", "checkout marker", metadata, "TUI or web UI"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	// A gitfile root resolves to a DIFFERENT repository's identity, so --repo
	// could name the wrong project — the refusal names the session for the
	// UI surfaces instead of printing a command.
	if strings.Contains(delErr.Error(), "af sessions archive") {
		t.Fatalf("a gitfile-rooted suspect must get no archive command: %v", delErr)
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived}, [][2]string{{rootID, "sepgit-session"}})
}

// TestAbsentRootDeleteRefusesWhileAGitdirNameEndsInWhitespace pins the
// pointer parse: a separate-git-dir name that legitimately ends in a space
// must survive — trimming it names a different, nonexistent directory whose
// missing markers then read as proof of a different clone.
func TestAbsentRootDeleteRefusesWhileAGitdirNameEndsInWhitespace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, _ := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	chk := markerCheckoutID(t, bare)
	derived := config.DerivedRepoIDForUnresolvedRoot(filepath.Clean(worktree))
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	fakeParent := testguard.CanonicalTempDir(t)
	metadata := filepath.Join(fakeParent, "meta ")
	fakeCommonDir(t, metadata, chk)
	root := filepath.Join(fakeParent, "checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+metadata+"\n"), 0o644); err != nil {
		t.Fatalf("write .git pointer: %v", err)
	}
	rootID := sessionUnderRoot(t, manager, "spaced-gitdir-session", root)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a whitespace-suffixed gitdir must be preserved — the real common dir holds this record's marker")
	}
	for _, want := range []string{"nothing was changed", "spaced-gitdir-session", "checkout marker", "meta ", "TUI or web UI"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	if strings.Contains(delErr.Error(), "af sessions archive") {
		t.Fatalf("a gitfile-rooted suspect must get no archive command: %v", delErr)
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived}, [][2]string{{rootID, "spaced-gitdir-session"}})
}

// TestAbsentRootDeleteRefusesWhileASessionsGitfileDangles pins the last
// fail-open of the same class: a .git redirect whose target does not resolve
// to an existing git-shaped directory leaves the real common dir
// undetermined — its missing markers are not evidence of a different clone,
// so the root is unproven and the delete refuses.
func TestAbsentRootDeleteRefusesWhileASessionsGitfileDangles(t *testing.T) {
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

	fakeParent := testguard.CanonicalTempDir(t)
	root := filepath.Join(fakeParent, "checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	gone := filepath.Join(fakeParent, "moved-away")
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+gone+"\n"), 0o644); err != nil {
		t.Fatalf("write .git pointer: %v", err)
	}
	rootID := sessionUnderRoot(t, manager, "dangling-gitfile-session", root)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a gitfile that resolves nowhere git-shaped is unproven, not cleared — the delete must refuse")
	}
	for _, want := range []string{"nothing was changed", "dangling-gitfile-session", "cannot describe", "TUI or web UI"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	if strings.Contains(delErr.Error(), "af sessions archive") {
		t.Fatalf("a root af cannot describe must get no archive command: %v", delErr)
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived}, [][2]string{{rootID, "dangling-gitfile-session"}})
}

// TestAbsentRootDeleteClearsAPlainParentWithCleanChildren pins the everyday
// pre-#3358 layout: a session keyed under a PLAIN parent directory whose
// children are readable and carry no marker with the row's id. Nothing the
// sweep can see could hold the row's checkout, so the root clears and the
// delete completes — healthy unrelated repositories must not refuse.
func TestAbsentRootDeleteClearsAPlainParentWithCleanChildren(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	_, worktree, _ := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	clearRecordedRepoID(t, project.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	plainParent := testguard.CanonicalTempDir(t)
	// A bare clone one level down whose marker dir reads cleanly and holds a
	// DIFFERENT checkout id — the usual unrelated child.
	otherClone := filepath.Join(plainParent, "other.git")
	fakeCommonDir(t, otherClone, "chk_"+strings.Repeat("d", 32))
	// A second bare clone whose marker dir holds only a stranded atomic
	// temp — a checkout-id-*.tmp.NNN is companion debris, never evidence.
	tmpClone := filepath.Join(plainParent, "tmp.git")
	fakeCommonDir(t, tmpClone, "")
	tmpMarkerDir := filepath.Join(tmpClone, "agent-factory")
	if err := os.MkdirAll(tmpMarkerDir, 0o755); err != nil {
		t.Fatalf("mkdir tmp marker dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpMarkerDir, "checkout-id-"+strings.Repeat("c", 32)+".tmp.99"), []byte("chk_x\n"), 0o644); err != nil {
		t.Fatalf("write stranded tmp: %v", err)
	}
	// A determinately-non-repository child — its agent-factory NAME is not
	// even a directory, and none of that makes the parent unproven.
	stray := filepath.Join(plainParent, "docs")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir stray: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stray, "agent-factory"), []byte("not a marker dir\n"), 0o644); err != nil {
		t.Fatalf("write stray agent-factory: %v", err)
	}
	rootID := sessionUnderRoot(t, manager, "plain-parent-session", plainParent)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr != nil {
		t.Fatalf("a plain parent whose children all read cleanly must clear: %v", delErr)
	}
	if !result.Deregistered {
		t.Fatalf("the absent-row delete must deregister the row once no possibly-belonging session survives")
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(rootID, "plain-parent-session")]
	manager.mu.Unlock()
	if inst == nil || inst.GetStatus() != session.Running {
		t.Fatalf("the cleared session must stay live through the delete")
	}
}

// managedSessionUnderIdentity injects a live AF-MANAGED worktree session —
// externalWorktree false, so archive CAN act on it — keyed under repoID with
// identityRoot as the root af recorded for it. liveSessionUnderIdentity
// builds in-place sessions instead, which archive refuses outright.
func managedSessionUnderIdentity(t *testing.T, m *Manager, repoID, title, identityRoot, worktreePath string) *session.Instance {
	t.Helper()
	gw, err := git.NewGitWorktreeFromStorage(identityRoot, worktreePath, title, "master", "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	inst := runningInstance(t, title, worktreePath, identityRoot)
	inst.SetGitWorktreeForTest(gw)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// TestAbsentRootRefusalPrintsTheProvableRepoCommandForAnArchivableSuspect
// pins the one archive command a refusal may print: `af sessions archive
// --repo <root> <title>` where <root> provably resolves to the session's own
// key — an af-managed session whose identity root IS its repo's root. The
// bare-title form, which resolves by cwd onto a possibly different
// same-titled session, must never appear.
func TestAbsentRootRefusalPrintsTheProvableRepoCommandForAnArchivableSuspect(t *testing.T) {
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
	inst := managedSessionUnderIdentity(t, manager, bareID, "managed-sibling", bare, sibling)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("an af-managed session under the owning clone must still stop the delete")
	}
	for _, want := range []string{"nothing was changed", "managed-sibling", "checkout marker", inst.ID, sibling, "TUI or web UI"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	// --repo <bare> provably hashes to this session's key: the bare
	// directory IS RepoFromPath's identity for a bare repository, and the
	// session is keyed under exactly its hash.
	if !strings.Contains(delErr.Error(), "af sessions archive --repo "+bare) {
		t.Fatalf("the provable --repo command must be printed: %v", delErr)
	}
	if strings.Contains(delErr.Error(), "af sessions archive -- ") {
		t.Fatalf("the refusal must never print a bare-title command: %v", delErr)
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, bareID}, [][2]string{{bareID, "managed-sibling"}})
}

// TestAbsentRootRefusalPrintsNoCommandWhenTheRootCannotProveTheKey pins the
// counterpart: an owning session whose recorded root does NOT hash to the
// key it is keyed under — a storage identity af cannot map back — gets no
// --repo command at all, since --repo <root> could resolve to a different
// project. The refusal names the session for the UI surfaces instead.
func TestAbsentRootRefusalPrintsNoCommandWhenTheRootCannotProveTheKey(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
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
	// Keyed under a DIFFERENT repository's id than its recorded root's hash
	// — the marker still proves ownership of the ROOT, but --repo <root>
	// would name another project.
	foreignID := repoID(t, filepath.Join(parent, "seed"))
	inst := managedSessionUnderIdentity(t, manager, foreignID, "foreign-keyed", bare, sibling)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("an owning session must stop the delete whatever key it rides under")
	}
	for _, want := range []string{"nothing was changed", "foreign-keyed", "checkout marker", inst.ID, "TUI or web UI"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, delErr)
		}
	}
	if strings.Contains(delErr.Error(), "af sessions archive") {
		t.Fatalf("a suspect whose root cannot prove its key must get no archive command: %v", delErr)
	}
	if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
		t.Fatalf("a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
			len(result.Archived), len(result.Killed), result.Deregistered)
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{derived, foreignID}, [][2]string{{foreignID, "foreign-keyed"}})
}

// TestAbsentRootDeleteRefusesWhileAPre3358SessionKeysUnderTheClonesParentForARealId
// pins the real-id counterpart of the test above: the IDENTICAL fixture,
// minus clearRecordedRepoID, so the row keeps its real RepoID (the hash of
// the bare common dir). Pre-#3358 sessions preserved under the bare clone's
// parent-directory identity (daemon/manager_create_legacy_bare.go) are keyed
// under an identity that never equals the row's recorded RepoID, so a
// real-id absent-root delete must refuse over them exactly as the derived-id
// form does — the belonging scan's marker evidence lives one level down
// inside the bare clone the parent holds, regardless of which identity the
// row is filed under. Before the fix the gate was derived-only, so the scan
// never ran for a real-id row and the delete succeeded while the
// parent-keyed session survived as an orphan.
func TestAbsentRootDeleteRefusesWhileAPre3358SessionKeysUnderTheClonesParentForARealId(t *testing.T) {
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
	bareID := repoID(t, bare)
	// The row KEEPS its real RepoID (the hash of the bare common dir) — the
	// shape a reconciled row has, unlike the legacy row above. This is the
	// one difference from TestAbsentRootDeleteRefusesWhileAPre3358SessionKeysUnderTheClonesParent.
	if recorded := onlyIdentityFor(t, project.ID); recorded != bareID {
		t.Fatalf("fixture must keep the row's real RepoID %q, got %q", bareID, recorded)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The pre-#3358 row persisted the unrelated parent as BOTH the workspace
	// and the identity root (manager_create_legacy_bare.go).
	inst := runningInstance(t, "legacy-sibling", parent, parent)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(parentID, "legacy-sibling")] = inst
	manager.mu.Unlock()

	// Both request forms that deregister the row reach the same refusal: by
	// path, and by the real RepoID the row recorded.
	for _, req := range []DeleteProjectRequest{
		{RepoPath: worktree},
		{RepoID: bareID},
	} {
		result, delErr := manager.DeleteProject(req)
		if delErr == nil {
			t.Fatalf("request %+v: the delete deregistered the row while a parent-keyed session stayed live — the reported bug", req)
		}
		for _, want := range []string{"nothing was changed", "legacy-sibling", "checkout marker", bare, "af sessions archive --repo"} {
			if !strings.Contains(delErr.Error(), want) {
				t.Fatalf("request %+v: the refusal must name %q: %v", req, want, delErr)
			}
		}
		// The parent is a repository in its own right whose path provably hashes
		// to this session's key, so --repo may be printed — but the bare-title
		// form, which resolves by cwd onto a possibly different session, must
		// not appear anywhere.
		if strings.Contains(delErr.Error(), "af sessions archive -- ") {
			t.Fatalf("request %+v: the refusal must never print a bare-title command: %v", req, delErr)
		}
		if len(result.Archived) != 0 || len(result.Killed) != 0 || result.Deregistered {
			t.Fatalf("request %+v: a refused delete must archive, kill and deregister nothing: archived=%d killed=%d deregistered=%v",
				req, len(result.Archived), len(result.Killed), result.Deregistered)
		}
	}
	assertAbsentRowSurvives(t, manager, project.ID, []string{bareID, parentID}, [][2]string{{parentID, "legacy-sibling"}})
}

// TestRealIdAbsentRootDeleteHandlesSessionsKeyedUnderTheRowsOwnIdentity pins
// the exclusion the widened gate requires: a real-id row whose recorded root
// is absent, with a live session keyed under the row's OWN real RepoID, must
// complete exactly as it did before the gate was widened — the archive loop
// keys on repoID and tears down that session. Without the
// excludeDeletedIdentity guard the belonging scan would classify the row's
// own identity as an owner (its checkout marker lives in its common dir) and
// turn this ordinary delete into a refusal, a regression.
func TestRealIdAbsentRootDeleteHandlesSessionsKeyedUnderTheRowsOwnIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	// The parent is a repository in its own right so its common dir is
	// provably not the row's, the same shape the refusal test uses.
	if err := exec.Command("git", "init", parent).Run(); err != nil {
		t.Fatalf("git init %s: %v", parent, err)
	}
	bare, worktree, sibling := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	bareID := repoID(t, bare)
	if recorded := onlyIdentityFor(t, project.ID); recorded != bareID {
		t.Fatalf("fixture must keep the row's real RepoID %q, got %q", bareID, recorded)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A live session keyed under the row's OWN real RepoID — the identity the
	// archive loop handles. Its identity root is the bare common dir, where
	// the row's checkout marker lives, so the scan would classify it as an
	// owner without the exclusion.
	liveSessionUnderIdentity(t, manager, bareID, "own-session", sibling)

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr != nil {
		t.Fatalf("a real-id absent-root delete with a session under the row's own identity must complete, not refuse: %v", delErr)
	}
	if !result.Deregistered {
		t.Fatalf("the row must be deregistered once its own-identity session is handled")
	}
	if len(result.Archived) == 0 && len(result.Killed) == 0 {
		t.Fatalf("the own-identity session must be archived or killed by the archive loop, not left behind")
	}
	// The session must no longer be live under the row's own identity.
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(bareID, "own-session")]
	manager.mu.Unlock()
	if inst != nil && inst.GetStatus() != session.Archived {
		t.Fatalf("the own-identity session must be archived or gone after the delete, got status %v", inst.GetStatus())
	}
}

// TestRealIdAbsentRootDeleteReportsAParentKeyedSessionThatMaterializedDuringItsDeregister
// pins the post-deregister half for a real-id row: a parent-keyed session that
// materializes between the pre-deregister re-check and the durable removal
// arrives under an identity the resolution scan never cleared (no session was
// live under it at resolution), so the unvetted pass vets it — finds the row's
// checkout marker one level down inside the bare clone — and the delete fails
// closed with the archive remedy rather than reporting success over a live
// orphan.
func TestRealIdAbsentRootDeleteReportsAParentKeyedSessionThatMaterializedDuringItsDeregister(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent := testguard.CanonicalTempDir(t)
	if err := exec.Command("git", "init", parent).Run(); err != nil {
		t.Fatalf("git init %s: %v", parent, err)
	}
	bare, worktree, _ := bareWorktreeFixture(t, parent)
	project := registerTestProject(t, worktree)
	parentID := repoID(t, parent)
	bareID := repoID(t, bare)
	if recorded := onlyIdentityFor(t, project.ID); recorded != bareID {
		t.Fatalf("fixture must keep the row's real RepoID %q, got %q", bareID, recorded)
	}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the recorded root: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// No parent-keyed session is live at resolution, so the scan clears every
	// identity it sees and the delete proceeds. The session lands in the gap
	// between the pre-deregister re-check and the durable removal — the
	// window the post-deregister re-scan exists for. The hook stretches it.
	previousHook := deleteProjectPreDeregisterHookForTest
	deleteProjectPreDeregisterHookForTest = func() {
		inst := runningInstance(t, "late-legacy-sibling", parent, parent)
		manager.mu.Lock()
		manager.instances[daemonInstanceKey(parentID, "late-legacy-sibling")] = inst
		manager.mu.Unlock()
	}
	t.Cleanup(func() { deleteProjectPreDeregisterHookForTest = previousHook })

	result, delErr := manager.DeleteProject(DeleteProjectRequest{RepoPath: worktree})
	if delErr == nil {
		t.Fatalf("a parent-keyed session that materialized mid-delete under an unvetted identity must not be reported as success")
	}
	if !result.Deregistered {
		t.Fatalf("the row still comes off — the report is over what survived it")
	}
	for _, want := range []string{"late-legacy-sibling", "checkout marker"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Fatalf("the report must name %q: %v", want, delErr)
		}
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(parentID, "late-legacy-sibling")]
	manager.mu.Unlock()
	if inst == nil || inst.GetStatus() != session.Running {
		t.Fatalf("the late session stays live — the delete only reports it")
	}
}
