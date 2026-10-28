package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// An off-box row archived without a push keeps its work on its host; restore must
// refuse before raising any fence rather than re-provision a sandbox that clones a
// branch origin does not have (#5571 W8.5).
func TestRestoreRefusesPreservedUnpushedWorkspace(t *testing.T) {
	for _, route := range []string{"RestoreSession", "RestoreArchived"} {
		t.Run(route, func(t *testing.T) { restoreRefusesPreservedUnpushedWorkspace(t, route) })
	}
}

func restoreRefusesPreservedUnpushedWorkspace(t *testing.T, route string) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.FromInstanceData(session.InstanceData{
		ID: "sess-9", Title: "kept", Path: repoPath, Status: session.Archived, BackendType: "local",
		Worktree:          session.GitWorktreeData{RepoPath: repoPath, WorktreePath: repoPath + "/archived/kept"},
		ArchivedWorkspace: &session.ArchivedWorkspaceData{Host: "box", Dir: "/home/u/.af-sessions/sess-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "kept")] = inst
	m.mu.Unlock()

	if route == "RestoreSession" {
		_, _, err = m.RestoreSession(RestoreSessionRequest{ID: "sess-9", Title: "kept", RepoID: repoID})
	} else {
		_, _, err = m.RestoreArchived(RestoreArchivedRequest{ID: "sess-9", Title: "kept", RepoID: repoID})
	}
	if err == nil || !strings.Contains(err.Error(), "box:/home/u/.af-sessions/sess-9") {
		t.Fatalf("restore must refuse and name where the work lives, got %v", err)
	}
	if inst.GetLiveness() != session.LiveArchived {
		t.Fatalf("a refused restore must leave the row Archived, got %v", inst.GetLiveness())
	}
}
