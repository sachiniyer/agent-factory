package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// A deleted worktree that reappears during the gone route's teardown holds an
// unverified directory. The archive fails as unknown and the record is fenced,
// so the drop to Lost cannot hand that directory to automatic recovery.
func TestArchiveSession_GoneWorktreeReappearingDuringTeardownIsFenced(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "reappears")

	prev := archiveGoneTeardown
	archiveGoneTeardown = func(i *session.Instance, before func() error, trust, adopted bool) (error, error) {
		require.False(t, adopted, "premise: this archive took the deletion route")
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		return prev(i, before, trust, adopted)
	}
	t.Cleanup(func() { archiveGoneTeardown = prev })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "reappears", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	_, _, unresolved := inst.GetWorktreeRelocationCandidates()
	assert.True(t, unresolved, "Lost recovery must refuse the reappeared directory")
	rec := recordFor(t, repoID, "reappears")
	require.NotNil(t, rec)
	assert.NotNil(t, rec.Worktree.RelocationRecovery, "the fence must be durable")
}

// On the deletion route the on-archive hook still runs — in a scratch
// directory, not the deleted path (which cannot be a cwd) and not the repo root
// (where a prune of "." would hit the user's main checkout).
func TestArchiveSession_DeletionRouteRunsHookInScratchDir(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "hook-gone")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf(
		`test -z "$AF_ARCHIVE_PATH" && test "$AF_WORKTREE_PATH" = %q && `+
			`test "$PWD" != %q && test -z "$(ls -A)" && printf ran > %q`,
		wtPath, repoPath, marker))

	_, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "hook-gone", RepoID: repoID})
	require.NoError(t, err, "the hook must run cleanly, so no hook-failure warning either")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	got, err := os.ReadFile(marker)
	require.NoError(t, err, "the hook must have run")
	assert.Equal(t, "ran", string(got))
}

// fenceGoneArchive drives the reappearance fence: the deleted worktree's path is
// recreated during the gone route's teardown, so the archive fails fenced and the
// row drops to Lost with an identity-unknown stall on record.
func fenceGoneArchive(t *testing.T, manager *Manager, repoID, title, wtPath string) {
	t.Helper()
	prev := archiveGoneTeardown
	archiveGoneTeardown = func(i *session.Instance, before func() error, trust, adopted bool) (error, error) {
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		return prev(i, before, trust, adopted)
	}
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	archiveGoneTeardown = prev
	require.Error(t, err)
	require.Contains(t, err.Error(), "fenced")
}

// The fence must not outlive what it guards: once the path is conclusively
// absent again, a retried archive takes the deletion route (#5102).
func TestArchiveSession_FencedRowArchivesOnceThePathIsGoneAgain(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "refenced")
	fenceGoneArchive(t, manager, repoID, "refenced", wtPath)

	// While the path exists it stays unverified: the gone route stamped the
	// flag before teardown, so the retry refuses instead of moving it.
	missing, _ := inst.WorktreeMissing()
	require.True(t, missing, "the gone route records the absence it proved")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "refenced", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded as deleted outside af")
	assert.DirExists(t, wtPath, "the unverified directory must not be moved into the archive")

	require.NoError(t, os.Remove(wtPath))
	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "refenced", RepoID: repoID})
	require.NoError(t, err)
	assert.Equal(t, session.LiveArchived, archived.Liveness)
	assert.True(t, archived.Worktree.Missing)
	assert.Nil(t, archived.Worktree.RelocationRecovery, "the discharged fence must not be persisted again")
}

// Likewise kill: the identity-unknown fence must not make the row unkillable
// once its path is gone, and must keep refusing while it is not.
func TestKillSession_FencedRowKillableOnceThePathIsGoneAgain(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "refenced-kill")
	fenceGoneArchive(t, manager, repoID, "refenced-kill", wtPath)
	marker := filepath.Join(wtPath, "unverified.txt")
	require.NoError(t, os.WriteFile(marker, []byte("x"), 0o644))

	_, err := manager.KillSession(KillSessionRequest{Title: "refenced-kill", RepoID: repoID})
	require.Error(t, err, "the fence must hold while the unverified path exists")
	assert.False(t, inst.UserKilled())
	assert.FileExists(t, marker, "kill must not delete the unverified directory")

	require.NoError(t, os.RemoveAll(wtPath))
	_, err = manager.KillSession(KillSessionRequest{Title: "refenced-kill", RepoID: repoID})
	require.NoError(t, err)
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "refenced-kill")]
	manager.mu.Unlock()
	assert.False(t, tracked, "a completed kill removes the row")
}
