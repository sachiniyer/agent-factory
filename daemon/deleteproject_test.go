package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type killAnotherSessionBackend struct {
	*session.FakeBackend
	onKill func() error
}

func (b *killAnotherSessionBackend) Kill(instance *session.Instance) error {
	if err := b.onKill(); err != nil {
		return err
	}
	return b.FakeBackend.Kill(instance)
}

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

	// The failure is atomic: NO in-memory root-agent suppression is left behind, so
	// a failed delete doesn't silently keep the always-on root from re-registering
	// (#1740 review). The persist runs before any in-memory mutation.
	manager.mu.Lock()
	_, suppressed := manager.deletedRootRepos[repoID]
	manager.mu.Unlock()
	assert.False(t, suppressed, "a failed delete must leave no dangling root-agent suppression")
}

// TestDeleteProject_TildePathMatches: a request whose path is a literal "~/…"
// (git does not expand tilde; the shell normally would) still resolves to the
// real project — the daemon expands the tilde before canonicalizing (#1740
// review), so it is never a silent miss.
func TestDeleteProject_TildePathMatches(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")

	// Point HOME at the repo's parent so "~/<basename>" expands to the real repo.
	t.Setenv("HOME", filepath.Dir(repoPath))
	tildePath := "~/" + filepath.Base(repoPath)

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: tildePath})
	require.NoError(t, err)
	assert.Len(t, result.Archived, 1, "a ~/-prefixed path must resolve to the real project, not silently miss")
	assert.Empty(t, liveProjectRoots(manager.Snapshot(repoID)))
}

// TestDeleteProject_ConcurrentlyArchivedTargetIsSuccess is the #2108 regression:
// DeleteProject snapshots the repo's live sessions under m.mu, releases the lock,
// then archives them one by one. A concurrent ArchiveSession that lands in that
// window leaves a snapshot target ALREADY in the desired archived state, so
// ArchiveSession's early liveness guard rejects it. That is idempotent SUCCESS,
// not a failure: the session is archived, which is all the delete wanted. Before
// the fix DeleteProject returned a partial-failure error and omitted the session
// from result.Archived, so the TUI took the error path (no refresh) and reported
// an undercount.
func TestDeleteProject_ConcurrentlyArchivedTargetIsSuccess(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "alpha")
	beta, betaSrc := registerArchivable(t, manager, repoID, repoPath, "beta")
	require.NoError(t, manager.SaveInstances())

	// Stand in for the race deterministically. Targets are archived in sorted
	// order, so flipping "beta" to Archived during "alpha"'s archive commit puts it
	// in exactly the state a concurrent ArchiveSession would have left it in by the
	// time the loop reaches it — no real data race needed to hit the guard.
	orig := archivePersist
	t.Cleanup(func() { archivePersist = orig })
	archivePersist = func(m *Manager, rid string, inst *session.Instance) error {
		if inst.Title == "alpha" {
			beta.SetStatusForTest(session.Archived)
		}
		return orig(m, rid, inst)
	}

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err, "a target already in the desired archived state is success, not a partial failure")

	var titles []string
	for _, d := range result.Archived {
		titles = append(titles, d.Title)
		assert.NotEmpty(t, d.ID, "every reported session carries its stable id, so the event names the right row")
	}
	assert.ElementsMatch(t, []string{"alpha", "beta"}, titles,
		"the already-archived session counts toward Archived — omitting it undercounts what the delete achieved")
	assert.Empty(t, result.Killed)

	// beta really is archived (the pre-flip stood in for a completed concurrent
	// archive, so its worktree is still where that archive would have left it) and
	// the project is gone from the active list — the contract the caller is told.
	assert.Equal(t, session.Archived, beta.GetStatus())
	assert.True(t, exists(betaSrc), "the seam only flipped liveness; this delete must not re-move an archived worktree")
	assert.Empty(t, liveProjectRoots(manager.Snapshot(repoID)), "no live session ⇒ the project drops out of the active list")
}

// TestDeleteProject_GenuineArchiveFailureStillReportsPartialFailure is the
// other half of #2108: treating "already archived" as success must NOT become a
// blanket swallow of ArchiveSession errors. A session that genuinely could not be
// archived — here one held by another destructive op — is still a real failure,
// still reported, and still excluded from result.Archived, so the caller knows to
// retry.
func TestDeleteProject_GenuineArchiveFailureStillReportsPartialFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "alpha")
	beta, betaSrc := registerArchivable(t, manager, repoID, repoPath, "beta")
	require.NoError(t, manager.SaveInstances())

	// beta is genuinely busy: another destructive op holds it, so its archive fails
	// for real and it stays LIVE.
	manager.mu.Lock()
	manager.killsInFlight[daemonInstanceKey(repoID, "beta")] = struct{}{}
	manager.mu.Unlock()

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.Error(t, err, "a genuine archive failure must still surface as a partial failure")
	assert.Contains(t, err.Error(), "could not be removed")
	assert.Len(t, result.Archived, 1, "only the session that actually archived is reported")
	assert.Equal(t, "alpha", result.Archived[0].Title)

	assert.NotEqual(t, session.Archived, beta.GetStatus(), "the busy session is untouched")
	assert.True(t, exists(betaSrc), "the busy session's worktree is untouched")
}

// TestDeleteProject_ConcurrentlyKilledExternalTargetIsSuccess is the external
// counterpart to TestDeleteProject_ConcurrentlyArchivedTargetIsSuccess above.
// DeleteProject's under-lock snapshot proves beta existed. If another kill
// removes beta before the sorted loop reaches it, "not found" means beta is
// already in the desired gone state — not a partial failure.
func TestDeleteProject_ConcurrentlyKilledExternalTargetIsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name             string
		legacyIDlessBeta bool
	}{
		{name: "stable id"},
		{name: "legacy id-less row", legacyIDlessBeta: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)

			registerExternal := func(title string, backend session.Backend) *session.Instance {
				gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, title, "master", "", true, false)
				require.NoError(t, err)
				inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
				require.NoError(t, err)
				inst.SetBackend(backend)
				inst.SetGitWorktreeForTest(gw)
				inst.SetStartedForTest(true)
				inst.SetStatusForTest(session.Ready)
				manager.mu.Lock()
				manager.instances[daemonInstanceKey(repoID, title)] = inst
				manager.mu.Unlock()
				return inst
			}

			beta := registerExternal("beta", session.NewFakeBackend())
			if tc.legacyIDlessBeta {
				beta.ID = ""
			}
			var concurrentKillErr error
			alphaBackend := &killAnotherSessionBackend{
				FakeBackend: session.NewFakeBackend(),
				onKill: func() error {
					_, concurrentKillErr = manager.KillSession(KillSessionRequest{ID: beta.ID, Title: beta.Title, RepoID: repoID})
					return concurrentKillErr
				},
			}
			alpha := registerExternal("alpha", alphaBackend)
			require.NoError(t, manager.SaveInstances())

			result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
			require.NoError(t, concurrentKillErr, "the competing kill must complete before DeleteProject reaches beta")
			require.NoError(t, err, "a snapshotted target already in the desired gone state is success, not a partial failure")

			wantIDs := map[string]string{"alpha": alpha.ID, "beta": beta.ID}
			var titles []string
			for _, data := range result.Killed {
				titles = append(titles, data.Title)
				assert.Equal(t, wantIDs[data.Title], data.ID,
					"every reported kill carries the identity captured in the snapshot")
			}
			assert.ElementsMatch(t, []string{"alpha", "beta"}, titles,
				"the concurrently killed session counts toward Killed instead of being undercounted")
			assert.Empty(t, result.Archived)
			assert.Empty(t, manager.Snapshot(repoID), "both external sessions are gone")

			_, err = manager.KillSession(KillSessionRequest{ID: beta.ID, Title: beta.Title, RepoID: repoID})
			require.Error(t, err, "a normal single-session caller must still see a stale target as not found")
			assert.Contains(t, err.Error(), "not found")
		})
	}
}
