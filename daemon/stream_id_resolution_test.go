package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// The /v1/sessions/{id}/stream route resolves its {id} segment by the session's
// stable id FIRST, then by title (#1592 Phase 5 PR4). The TUI/apiclient pass a
// title there (unchanged); the browser web client passes the globally-unique
// session id so a title shared across two repos names exactly one session. These
// tests pin resolveStreamSession — the resolver agentServerForStream delegates to.

// TestResolveStreamSessionByID resolves a tracked instance by its stable id and
// returns the instance's own repoID and title (so the killsInFlight gate keys off
// the real title even when the caller addressed the session by id).
func TestResolveStreamSessionByID(t *testing.T) {
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

	inst, _ := newCountingInstance(t, "worker", repoPath)
	inst.ID = "id-worker-123"
	key := daemonInstanceKey(repo.ID, "worker")
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	// Address by the stable id, with no repo_id (the browser's call shape).
	got, rid, title, err := manager.resolveStreamSession("id-worker-123", "")
	if err != nil {
		t.Fatalf("resolveStreamSession by id: %v", err)
	}
	if got != inst {
		t.Fatalf("resolved instance = %p, want the tracked instance %p", got, inst)
	}
	if rid != repo.ID {
		t.Fatalf("resolved repoID = %q, want %q", rid, repo.ID)
	}
	if title != "worker" {
		t.Fatalf("resolved title = %q, want %q (killsInFlight gate keys off title)", title, "worker")
	}
}

// TestResolveStreamSessionIDBeatsCrossRepoTitleCollision is the reason the browser
// keys off id: two sessions share a title across two repos, so a title alone is
// ambiguous, but each id names exactly one. Resolving by the second session's id
// must return THAT session (its repo), never the first same-titled one.
func TestResolveStreamSessionIDBeatsCrossRepoTitleCollision(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoA := setupControlRepo(t)
	repoB := setupControlRepo(t)
	ra, err := config.RepoFromPath(repoA)
	if err != nil {
		t.Fatalf("RepoFromPath A: %v", err)
	}
	rb, err := config.RepoFromPath(repoB)
	if err != nil {
		t.Fatalf("RepoFromPath B: %v", err)
	}
	if ra.ID == rb.ID {
		t.Fatalf("test needs two distinct repos; both hashed to %q", ra.ID)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	instA, _ := newCountingInstance(t, "dup", repoA)
	instA.ID = "id-in-repo-a"
	instB, _ := newCountingInstance(t, "dup", repoB)
	instB.ID = "id-in-repo-b"
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(ra.ID, "dup")] = instA
	manager.instances[daemonInstanceKey(rb.ID, "dup")] = instB
	manager.mu.Unlock()

	got, rid, _, err := manager.resolveStreamSession("id-in-repo-b", "")
	if err != nil {
		t.Fatalf("resolveStreamSession: %v", err)
	}
	if got != instB {
		t.Fatalf("id resolution picked the wrong same-titled session across repos")
	}
	if rid != rb.ID {
		t.Fatalf("resolved repoID = %q, want repo B %q", rid, rb.ID)
	}
}

// TestResolveStreamSessionTitleFallback pins the unchanged TUI path: a value that
// matches no instance id falls back to title resolution, returning the passed
// string as the title (what the killsInFlight gate and error messages use).
func TestResolveStreamSessionTitleFallback(t *testing.T) {
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

	inst, _ := newCountingInstance(t, "byname", repoPath)
	inst.ID = "id-byname"
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, "byname")] = inst
	manager.mu.Unlock()

	// Address by TITLE (no id match) — the TUI's call shape.
	got, rid, title, err := manager.resolveStreamSession("byname", repo.ID)
	if err != nil {
		t.Fatalf("resolveStreamSession by title: %v", err)
	}
	if got != inst {
		t.Fatalf("title fallback did not return the tracked instance")
	}
	if rid != repo.ID {
		t.Fatalf("resolved repoID = %q, want %q", rid, repo.ID)
	}
	if title != "byname" {
		t.Fatalf("resolved title = %q, want %q", title, "byname")
	}
}
