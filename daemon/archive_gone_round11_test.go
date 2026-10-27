package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

func gitForTest(t *testing.T, repoPath string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repoPath}, args...)...).CombinedOutput()
	require.NoError(t, err, string(out))
}

// swapDirectory replaces the directory at path with a stranger's, the same-uid
// swap the identity reconfirmations exist to catch. It reports errors rather
// than failing the test, because one caller runs off the test goroutine.
func swapDirectory(path string) (string, error) {
	if err := os.Rename(path, path+"-swapped-out"); err != nil {
		return "", err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return "", err
	}
	marker := filepath.Join(path, "stranger.txt")
	return marker, os.WriteFile(marker, []byte("not the session's"), 0o644)
}

// The refusal that tells the user to move their checkout back must lead
// somewhere: once it is back at the recorded path, git proves it is the
// session's, and archive takes it even though the row was flagged missing
// while it was away (#5102).
func TestArchiveSession_FlaggedRowWithItsCheckoutPutBackArchives(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "put-back")
	elsewhere := filepath.Join(filepath.Dir(repoPath), "user-moved-put-back")
	moveWorktreeForTest(t, repoPath, wtPath, elsewhere)
	manager.RefreshStatuses()
	missing, _ := inst.WorktreeMissing()
	require.True(t, missing, "premise: the poll flagged the vacated path")

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "put-back", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "move it back to "+wtPath)

	moveWorktreeForTest(t, repoPath, elsewhere, wtPath)
	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "put-back", RepoID: repoID})
	require.NoError(t, err, "the user's own checkout, put back, must be archivable")
	assert.Equal(t, session.LiveArchived, archived.Liveness)
	assert.False(t, archived.Worktree.Missing)
	dirty, readErr := os.ReadFile(filepath.Join(archived.Worktree.WorktreePath, "dirty.txt"))
	require.NoError(t, readErr, "the session's bytes moved into the archive")
	assert.Equal(t, "uncommitted", string(dirty))
}

// The restore-side twin: a flagged archived row whose archive is back where af
// recorded it is restored, not refused (#5102).
func TestRestoreArchived_FlaggedRowWithItsArchivePutBackRestores(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := seedArchivedSession(t, manager, repoID, repoPath, "archive-put-back", "archive-put-back")
	archivedPath := inst.GetWorktreePath()
	inst.SetWorktreeMissing(session.WorktreeMissingDeletedReason(archivedPath))

	restoredPath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archive-put-back", RepoID: repoID})
	require.NoError(t, err)
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	missing, _ := inst.WorktreeMissing()
	assert.False(t, missing)
	dirty, readErr := os.ReadFile(filepath.Join(restoredPath, "dirty.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "uncommitted-archive-put-back", string(dirty))
}

// The flagged-row refusal is the fence: if it cannot be written, the caller
// must hear so rather than be told the row is safely refused (#5102).
func TestArchiveSession_UnverifiedRefusalReportsAFailedFenceWrite(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "fence-write")
	manager.RefreshStatuses()
	missing, _ := inst.WorktreeMissing()
	require.True(t, missing)
	require.NoError(t, os.Mkdir(wtPath, 0o755))

	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "fence-write" {
			return errors.New("disk full")
		}
		return nil
	}
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "fence-write", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded as deleted outside af")
	assert.Contains(t, err.Error(), "could not persist the session's worktree-missing fence")
	assert.Contains(t, err.Error(), "disk full")
}

// A worktree deleted together with its branch can still be archived — that is
// the action that resolves the row — but the archive must not promise a rebuild,
// and restore must say why it cannot run one (#5102).
func TestArchiveSession_DeletedWorktreeAndBranchArchivesHonestly(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "no-branch")
	gitForTest(t, repoPath, "worktree", "prune")
	gitForTest(t, repoPath, "branch", "-D", "af/no-branch")

	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "no-branch", RepoID: repoID})
	require.NoError(t, err)
	assert.Equal(t, session.LiveArchived, archived.Liveness)
	assert.True(t, archived.Worktree.Missing)
	assert.Equal(t, session.WorktreeMissingBranchGoneReason(wtPath), archived.Worktree.MissingReason)

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "no-branch", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer exists either")
	assert.Contains(t, err.Error(), "the session stays archived")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
}

// The adopted route's pre-teardown cancel persists the path it re-aimed on
// git's proof. A directory swapped in while the editor was being stopped must
// not be written as that path's authoritative worktree: the record is fenced
// instead, and later operations refuse the stranger (#5102).
func TestArchiveSession_AdoptedCancelReconfirmsBeforePersisting(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "adopt-swap")
	dest, err := archivedWorktreePath(repoID, "adopt-swap")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)

	key := daemonInstanceKey(repoID, "adopt-swap")
	var (
		swapOnce sync.Once
		marker   string
		swapErr  error
	)
	cmd, _ := startOwnedSleep(t)
	manager.vscode.mu.Lock()
	manager.vscode.servers[key] = &vscodeServer{
		worktree: dest, instanceID: inst.ID, cmd: cmd, exited: make(chan struct{}),
		stopGrace: 10 * time.Millisecond,
		killGroup: func(int, syscall.Signal) error {
			swapOnce.Do(func() { marker, swapErr = swapDirectory(dest) })
			return nil
		},
	}
	manager.vscode.mu.Unlock()

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "adopt-swap", RepoID: repoID})
	require.NoError(t, swapErr)
	require.NotEmpty(t, marker, "premise: the swap ran during the editor stop")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VS Code editor teardown could not be confirmed")
	assert.Contains(t, err.Error(), "no longer the directory git proved")
	rec := recordFor(t, repoID, "adopt-swap")
	require.NotNil(t, rec)
	require.NotNil(t, rec.Worktree.RelocationRecovery, "the swapped path is persisted fenced, not as the worktree")
	assert.NotZero(t, rec.Worktree.RelocationRecovery.Inode, "a real fence carrying the proven identity, not the missing projection")

	manager.vscode.mu.Lock()
	delete(manager.vscode.servers, key)
	manager.vscode.mu.Unlock()
	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "adopt-swap", RepoID: repoID})
	require.Error(t, err, "the fence must refuse the stranger")
	assert.FileExists(t, marker)
}

// Both gone-route cancels persist the flag the route just settled; a write that
// fails must be reported with the cancellation, not swallowed (#5102).
func TestArchiveSession_GoneRouteCancelReportsAFailedWrite(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "cancel-write")
	key := daemonInstanceKey(repoID, "cancel-write")
	plantStuckEditor(t, manager, inst, key, wtPath)

	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "cancel-write" {
			return errors.New("disk full")
		}
		return nil
	}
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "cancel-write", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VS Code editor teardown could not be confirmed")
	assert.Contains(t, err.Error(), "could not record the session's worktree state on disk")
	assert.Contains(t, err.Error(), "disk full")
}

// A rebuilt or adopted restore directory is reconfirmed before the restore
// records it or starts the agent: one swapped in after af placed it is refused,
// the row stays archived and flagged, and the fence is persisted (#5102).
func TestRestoreArchived_PlacedWorktreeSwappedBeforeTheCommitRefuses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ownRebuilt bool
	}{
		{name: "fresh rebuild", ownRebuilt: false},
		{name: "adopted earlier rebuild", ownRebuilt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			title := "placed-swap"
			if tc.ownRebuilt {
				title = "placed-swap-adopt"
			}
			inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, title)
			branch := "af/" + sanitizeArchiveTitle(title)
			inst.SetBranchForTest(branch)
			_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
			require.NoError(t, err)
			dest, err := sessiongit.RestoreWorktreeBasePath(repoPath, title, branch)
			require.NoError(t, err)
			if tc.ownRebuilt {
				gitForTest(t, repoPath, "worktree", "prune")
				gitForTest(t, repoPath, "worktree", "add", dest, branch)
			}

			var marker string
			prev := afterRestoreWorktreePlaced
			t.Cleanup(func() { afterRestoreWorktreePlaced = prev })
			var swapErr error
			afterRestoreWorktreePlaced = func() { marker, swapErr = swapDirectory(dest) }

			_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: title, RepoID: repoID})
			require.NoError(t, swapErr)
			require.NotEmpty(t, marker, "premise: the placement ran and the swap with it")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no longer the directory it verified")
			assert.Equal(t, session.LiveArchived, inst.GetLiveness())
			assert.Equal(t, session.OpNone, inst.GetInFlightOp())
			assert.FileExists(t, marker, "the stranger's files are untouched")
			rec := recordFor(t, repoID, title)
			require.NotNil(t, rec)
			assert.Equal(t, session.LiveArchived, rec.Liveness)
			assert.True(t, rec.Worktree.Missing, "the flag clears only on a committed restore")
			require.NotNil(t, rec.Worktree.RelocationRecovery, "the fence reached disk")
			assert.NotZero(t, rec.Worktree.RelocationRecovery.Inode)
		})
	}
}
