package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveProjectRoots mirrors the "active projects" derivation the TUI and web use
// (live-only grouping by repo root): archived sessions do not keep a project in
// the list, which is what makes delete-project remove it and restore bring it
// back. Kept in the test so the reversible contract is asserted against the same
// rule the surfaces derive from.
func liveProjectRoots(data []session.InstanceData) map[string]bool {
	roots := map[string]bool{}
	for _, d := range data {
		if session.IsArchivedData(d) {
			continue
		}
		if d.Worktree.RepoPath != "" {
			roots[d.Worktree.RepoPath] = true
		}
	}
	return roots
}

// TestDeleteProject_ArchivesAllSessionsRestorableRepoUntouched is the core
// contract (#1735): deleting a project with N live sessions archives all N
// (restorable), drops the project from the active/derived list, and never
// touches the user's real git repo; restoring one archived session brings the
// project back.
func TestDeleteProject_ArchivesAllSessionsRestorableRepoUntouched(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst1, src1 := registerArchivable(t, manager, repoID, repoPath, "alpha")
	_, src2 := registerArchivable(t, manager, repoID, repoPath, "beta")
	// A recover-capable backend so the later restore re-spawns to Running (the
	// plain FakeBackend's Recover doesn't), mirroring the archive restore test.
	inst1.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	// registerArchivable rewrites the per-repo file each call, so persist ALL
	// in-memory instances together — otherwise the second seed clobbers the first
	// on disk and the reconcile inside archive drops the in-memory-only session.
	require.NoError(t, manager.SaveInstances())

	// Precondition: the repo is an active project with 2 live sessions.
	require.True(t, liveProjectRoots(manager.Snapshot(repoID))[repoPath])

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err)
	assert.Len(t, result.Archived, 2, "both live sessions must be archived")
	assert.Empty(t, result.Killed, "neither session is in-place, so none is torn down")

	// Both sessions are now inert Archived rows, worktrees relocated out.
	for _, title := range []string{"alpha", "beta"} {
		manager.mu.Lock()
		got := manager.instances[daemonInstanceKey(repoID, title)]
		manager.mu.Unlock()
		require.NotNil(t, got, "an archived session is preserved (restorable), not deleted")
		assert.Equal(t, session.Archived, got.GetStatus())
	}
	assert.False(t, exists(src1), "alpha's original worktree must be gone (relocated to the archive)")
	assert.False(t, exists(src2), "beta's original worktree must be gone (relocated to the archive)")

	// The project is gone from the active/derived list.
	assert.Empty(t, liveProjectRoots(manager.Snapshot(repoID)), "no live session ⇒ the project drops out of the active list")

	// The user's real git repository is UNTOUCHED.
	assert.True(t, exists(repoPath), "the real repo directory must survive a project delete")
	assert.True(t, exists(filepath.Join(repoPath, ".git")), "the real repo's .git must be untouched")

	// Reversible: restoring one archived session brings the project back.
	_, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "alpha", RepoID: repoID})
	require.NoError(t, err)
	assert.Equal(t, session.Running, inst1.GetStatus(), "the restored session is live again")
	assert.True(t, liveProjectRoots(manager.Snapshot(repoID))[repoPath], "restoring an archived session reconstitutes the project")
}

// TestDeleteProject_UnknownProjectIsNoOp: deleting a project the daemon knows
// nothing about archives nothing, drops no opt-in, and returns a zero-count
// success (idempotent).
func TestDeleteProject_UnknownProjectIsNoOp(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: "/no/such/repo"})
	require.NoError(t, err)
	assert.Empty(t, result.Archived)
	assert.Empty(t, result.Killed)
}

// TestDeleteProject_RemovesRootAgentsOptInAndSuppressesRespawn: when the repo has
// a persisted root_agents opt-in, delete removes it from config on disk AND
// suppresses the daemon from re-ensuring the always-on root for the rest of its
// life; restoring an archived session still reconstitutes the project.
func TestDeleteProject_RemovesRootAgentsOptInAndSuppressesRespawn(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")

	// Seed a durable root_agents opt-in for this repo on disk.
	seed := config.DefaultConfig()
	seed.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
	require.NoError(t, config.SaveConfig(seed))

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err)
	assert.Len(t, result.Archived, 1)

	// The opt-in is gone from disk...
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	assert.NotContains(t, cfg.RootAgents, repoPath, "the root_agents opt-in must be removed on disk")

	// ...and the running daemon suppresses re-ensuring the root.
	manager.mu.Lock()
	_, suppressed := manager.deletedRootRepos[repoID]
	manager.mu.Unlock()
	assert.True(t, suppressed, "the ensure loop must be suppressed for the deleted repo")

	// Reversible: restore reconstitutes the project in the active list.
	_, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	assert.True(t, liveProjectRoots(manager.Snapshot(repoID))[repoPath], "restore reconstitutes the project")
}

// TestDeleteProject_DerivesRepoIDFromPath: a request that carries only repo_path
// (the web/CLI shape) still targets the right sessions — the daemon derives the
// repo id from the path.
func TestDeleteProject_DerivesRepoIDFromPath(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath})
	require.NoError(t, err)
	assert.Len(t, result.Archived, 1, "the session is found via the path-derived repo id")
	assert.Empty(t, liveProjectRoots(manager.Snapshot(repoID)))
}

// TestDeleteProject_NonCanonicalPathStillMatches: a path-only request whose path
// is NOT the canonical repo root (here a subdirectory of the repo — hashing it
// raw would miss) still targets the right project, because the daemon resolves it
// to the git toplevel the same way it keys repos everywhere (#1740 review). A
// silent no-op on a real project must never happen.
func TestDeleteProject_NonCanonicalPathStillMatches(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")

	// A subdirectory path: RepoIDFromRoot(subdir) != repoID, so without
	// canonicalization this would silently match nothing.
	subdir := filepath.Join(repoPath, "nested", "deep")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	require.NotEqual(t, repoID, config.RepoIDFromRoot(subdir), "precondition: the raw subdir hash differs from the repo id")

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: subdir})
	require.NoError(t, err)
	assert.Len(t, result.Archived, 1, "a non-canonical path must still resolve to the real project's sessions")
	assert.Empty(t, liveProjectRoots(manager.Snapshot(repoID)), "the project is gone, not silently missed")
}

// TestDeleteProject_RootAgentsWriteFailureIsFatal: if the durable root_agents
// removal fails, DeleteProject must FAIL (never report success) and, because the
// write is attempted before any teardown, leave the project intact — otherwise a
// daemon restart would re-register the root and the "deleted" project would
// reappear while the caller believed it was gone (#1740 review).
func TestDeleteProject_RootAgentsWriteFailureIsFatal(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, src := registerArchivable(t, manager, repoID, repoPath, "worker")

	orig := deregisterRootAgents
	deregisterRootAgents = func(string) ([]string, error) { return nil, fmt.Errorf("forced config write failure") }
	t.Cleanup(func() { deregisterRootAgents = orig })

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.Error(t, err, "a failed root_agents write must surface as an error, not a silent success")
	assert.Empty(t, result.Archived, "nothing is archived when the durable removal fails")

	// The project is NOT gone: its session is still live and it still appears in
	// the active list.
	assert.NotEqual(t, session.Archived, inst.GetStatus(), "the session must remain live on a failed delete")
	assert.True(t, exists(src), "the session's worktree must be untouched on a failed delete")
	assert.True(t, liveProjectRoots(manager.Snapshot(repoID))[repoPath], "the project must remain in the active list on a failed delete")
}
