package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// The TUI sends both the stable ID and the repo id. The ID must be resolved in the
// ID namespace, so a session whose title differs from its ID is still found, and a
// stale ID refuses instead of matching a title (#5796 review).
func TestWorkspaceFactsResolvesExplicitStableID(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.FromInstanceData(session.InstanceData{
		ID: "sess-123", Title: "alpha", Path: repoPath, Status: session.Archived, BackendType: "local",
		Worktree: session.GitWorktreeData{RepoPath: repoPath, WorktreePath: repoPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "alpha")] = inst
	m.mu.Unlock()

	_, got, err := m.agentServerForStreamTarget(workspaceFactsTarget(WorkspaceFactsRequest{ID: "sess-123", Title: "alpha", RepoID: repoID}))
	if err != nil || got != inst {
		t.Fatalf("explicit ID with a repo scope must resolve the session: got %v, err %v", got, err)
	}
	if _, _, err := m.agentServerForStreamTarget(workspaceFactsTarget(WorkspaceFactsRequest{ID: "stale", Title: "alpha", RepoID: repoID})); err == nil {
		t.Fatal("a stale ID must refuse, not fall back to the title")
	}
}
