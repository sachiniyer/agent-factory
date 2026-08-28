package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// makeInstancesUnreadable turns path into a genuine READ error — not a missing
// file, which the loader deliberately maps to "this repo has no sessions".
//
// chmod 0000 is the realistic shape (permissions drifted on a stale project
// dir), but it is not a reliable fixture on its own: file modes inherit the
// ambient umask, and root ignores them entirely. So the mode is applied and then
// PROVEN, with a directory-in-place fallback (os.ReadFile refuses a directory
// for everyone, root included) for the environments where the chmod does
// nothing.
func makeInstancesUnreadable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	// Leave the tree removable no matter which branch below is taken.
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("fixture must make %s UNREADABLE, not missing: %v", path, err)
		}
		return
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Fatalf("fixture did not take: %s is still readable", path)
	}
}

// TestRemoteHookSlugRefusesWhenAnotherReposRecordsAreUnreadable is the #3476
// regression: the cross-repo hook-name check proves a name is FREE, and it was
// reading that proof out of a load that silently DROPS every repo whose
// instances.json could not be read. One unreadable file therefore made a
// colliding session invisible and the check answered "no collision" — two
// sandboxes handed the identical --name, which hook scripts receive verbatim.
//
// A failed read is not an empty result. The fixture holds a real collision, is
// proven to hold it while readable, and is then made unreadable; admission must
// flip from "taken by X" to a refusal, never to "free".
func TestRemoteHookSlugRefusesWhenAnotherReposRecordsAreUnreadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoAPath := setupControlRepo(t)
	repoBPath := setupControlRepo(t)
	repoA, err := config.RepoFromPath(repoAPath)
	if err != nil {
		t.Fatalf("RepoFromPath(A): %v", err)
	}
	repoB, err := config.RepoFromPath(repoBPath)
	if err != nil {
		t.Fatalf("RepoFromPath(B): %v", err)
	}
	if repoA.ID == repoB.ID {
		t.Fatalf("test premise broken: distinct repos must have distinct IDs")
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The #1636 title pair, so this exercises the SLUG path and nothing else:
	// "My_App" and "MyApp" derive DISTINCT git branches but one hook name.
	const existingTitle = "My_App"
	const newTitle = "MyApp"
	if manager.titlesCollide(existingTitle, newTitle) {
		t.Fatalf("test premise broken: %q and %q collide on branch, so the slug path is unreachable", existingTitle, newTitle)
	}
	candidate := session.Slugify(newTitle)
	if session.Slugify(existingTitle) != candidate {
		t.Fatalf("test premise broken: %q and %q must slugify to one hook name", existingTitle, newTitle)
	}

	// Repo A holds the colliding hook session on disk only.
	rows, err := json.Marshal([]session.InstanceData{{
		Title: existingTitle, Path: repoAPath, Program: "claude", BackendType: "remote",
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoA.ID, rows); err != nil {
		t.Fatalf("save: %v", err)
	}
	path, err := config.RepoInstancesPath(repoA.ID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}

	// Premise: while the file is READABLE the guard finds the collision. This is
	// what makes the phase below a test of the read failure and not of an empty
	// fixture — the colliding row is provably there.
	owner, ownerRepo, err := hookSlugOwnerInOtherRepos(candidate, repoB.ID)
	if err != nil {
		t.Fatalf("readable scan must succeed: %v", err)
	}
	if owner != existingTitle || ownerRepo != repoAPath {
		t.Fatalf("test premise broken: readable scan should report %q in %s, got %q in %s", existingTitle, repoAPath, owner, ownerRepo)
	}

	makeInstancesUnreadable(t, path)

	// The collision has not gone anywhere; only our ability to see it has. Assert
	// on the whole admission gate first, because that is the decision the HTTP
	// CreateSession path acts on: before the fix this returns nil and the create
	// is ADMITTED.
	manager.mu.Lock()
	err = manager.validateTitleAvailableLocked(repoB.ID, repoB.Root, newTitle, "claude", runtimeNamespaceRemoteHook, false, nil)
	manager.mu.Unlock()
	if err == nil {
		t.Fatalf("remote hook create for %q was ADMITTED while a project record that may hold the same hook name could not be read", newTitle)
	}
	if !errors.Is(err, errTitleCheckFatal) {
		t.Fatalf("a check that could not RUN must carry errTitleCheckFatal so the suffix walk surfaces it instead of trying the next candidate, got: %v", err)
	}

	// The inner scan agrees, and its refusal is actionable: an unactionable one
	// is barely better than the fail-open it replaces, so the user has to be told
	// which file and why.
	owner, _, err = hookSlugOwnerInOtherRepos(candidate, repoB.ID)
	if err == nil {
		t.Fatalf("an unreadable project record is not evidence that hook name %q is free (reported owner %q)", candidate, owner)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal must name the unreadable file %s so it can be repaired, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("refusal must carry the underlying I/O error, got: %v", err)
	}

	// The refusal is scoped to the global hook namespace: a stale unreadable
	// project file must not block ordinary local sessions, whose names are
	// per-repo and never consult this scan.
	manager.mu.Lock()
	err = manager.validateTitleAvailableLocked(repoB.ID, repoB.Root, newTitle, "claude", runtimeNamespaceLocalTmux, false, nil)
	manager.mu.Unlock()
	if err != nil {
		t.Fatalf("local session titles are per-repo and must stay creatable while another project's record is unreadable: %v", err)
	}
}

// TestRemoteHookSlugStillReportsTheOwnerItCanSee keeps the refusal from
// swallowing a better answer. A collision found in a repo that DID load is
// definitive — no unread file can make a name that is provably taken free — so
// an unrelated unreadable record must not downgrade "taken by My_App in <path>"
// into a generic "could not verify".
func TestRemoteHookSlugStillReportsTheOwnerItCanSee(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoAPath := setupControlRepo(t)
	repoA, err := config.RepoFromPath(repoAPath)
	if err != nil {
		t.Fatalf("RepoFromPath(A): %v", err)
	}

	const existingTitle = "My_App"
	rows, err := json.Marshal([]session.InstanceData{{
		Title: existingTitle, Path: repoAPath, Program: "claude", BackendType: "remote",
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoA.ID, rows); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A second, unrelated project whose record cannot be read.
	if err := config.SaveRepoInstances("unreadable-repo", json.RawMessage("[]")); err != nil {
		t.Fatalf("save: %v", err)
	}
	blocked, err := config.RepoInstancesPath("unreadable-repo")
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	makeInstancesUnreadable(t, blocked)

	owner, ownerRepo, err := hookSlugOwnerInOtherRepos(session.Slugify("MyApp"), "some-other-repo")
	if err != nil {
		t.Fatalf("a collision that IS visible is a definitive answer and must not be downgraded to a refusal: %v", err)
	}
	if owner != existingTitle || ownerRepo != repoAPath {
		t.Fatalf("expected the visible owner %q in %s, got %q in %s", existingTitle, repoAPath, owner, ownerRepo)
	}
}
