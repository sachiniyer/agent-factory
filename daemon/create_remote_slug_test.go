package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// TestCreateSessionRejectsRemoteSlugCollisionWithInMemoryInstance pins the
// #1636 fix: the daemon's remote-slug collision check must consider in-memory
// remote sessions, not just persisted diskData.
//
// refreshDaemonInstances preserves a running remote instance in m.instances
// even after its repo directory vanishes from disk (a recoverable
// inconsistency), yet loadRepoInstanceData then returns nothing for that repo.
// Two titles like "My_App" and "MyApp" derive DISTINCT git branches (Slugify
// drops the underscore while branch sanitization keeps it) so the branch
// collision check lets both coexist, but both slugify to the same remote hook
// name "myapp". The HTTP /v1/CreateSession path relies solely on this
// daemon-side validation — the TUI's FindSlugCollision pre-check never runs for
// it — so the daemon must reject the second create itself.
func TestCreateSessionRejectsRemoteSlugCollisionWithInMemoryInstance(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Register a running remote session in memory only — no on-disk record.
	// Because the repo has no persisted instances directory, refreshLocked's
	// "preserve in-memory instances for a missing repo directory" path keeps it
	// alive across the refresh reserveCreate runs, while loadRepoInstanceData
	// returns an empty slice for it — exactly the buggy state.
	const existingTitle = "My_App"
	inst, err := session.NewInstance(session.InstanceOptions{Title: existingTitle, Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(remoteTypeBackend{session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, existingTitle)] = inst
	manager.mu.Unlock()

	// Sanity-check the premise: the two titles do NOT collide on their git
	// branch (so the branch check would pass), but they DO slugify to the same
	// remote hook name. If this ever stops holding, the test is no longer
	// exercising the slug-only collision path.
	const newTitle = "MyApp"
	if manager.titlesCollide(existingTitle, newTitle) {
		t.Fatalf("test premise broken: %q and %q collide on branch, so the slug path is unreachable", existingTitle, newTitle)
	}
	if session.Slugify(existingTitle) != session.Slugify(newTitle) {
		t.Fatalf("test premise broken: %q and %q must slugify to the same hook name", existingTitle, newTitle)
	}

	_, err = manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:       newTitle,
		RepoPath:    repoPath,
		Program:     "claude",
		ForceRemote: true,
	})
	if err == nil {
		t.Fatalf("expected duplicate remote hook slug to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "hook name") {
		t.Fatalf("rejection must name the remote hook-name collision, got: %v", err)
	}
}
