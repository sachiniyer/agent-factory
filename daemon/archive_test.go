package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// registerArchivable creates a real linked worktree in the manager's repo and a
// started instance bound to it (FakeBackend, no real tmux — ArchiveTeardown is a
// no-op on an instance with no tmux-backed tabs), registered in the manager and
// seeded on disk. Ready for ArchiveSession. Returns the instance and the
// worktree's original path.
func registerArchivable(t *testing.T, m *Manager, repoID, repoPath, title string) (*session.Instance, string) {
	t.Helper()
	wtPath := filepath.Join(filepath.Dir(repoPath), "wt-"+sanitizeArchiveTitle(title))
	branch := "af/" + sanitizeArchiveTitle(title)
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0644))

	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, branch, "", false, true)
	require.NoError(t, err)

	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(gw)
	inst.SetStartedForTest(true)
	inst.SetStatus(session.Ready)

	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst, wtPath
}

// TestArchiveSession_MovesWorktreeAndMarksArchived: the happy path — tmux torn
// down (no-op here), worktree relocated to <AF_HOME>/archived/<repoID>/<title>
// with its dirty tree + registration intact, instance flipped to inert Archived
// and preserved (not deleted) in the manager and on disk.
func TestArchiveSession_MovesWorktreeAndMarksArchived(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")

	archivedPath, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	expected, perr := archivedWorktreePath(repoID, "worker")
	require.NoError(t, perr)
	assert.Equal(t, expected, archivedPath, "worktree must land at the namespaced archive path")
	assert.False(t, exists(srcPath), "the original worktree directory must be gone")
	assert.True(t, exists(archivedPath), "the worktree must exist at the archive path")

	dirty, rerr := os.ReadFile(filepath.Join(archivedPath, "dirty.txt"))
	require.NoError(t, rerr, "the uncommitted tree must survive the archive move")
	assert.Equal(t, "uncommitted", string(dirty))

	assert.Equal(t, session.Archived, inst.GetStatus())
	assert.False(t, inst.Started(), "an archived instance is inert (started=false)")

	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "worker")]
	manager.mu.Unlock()
	assert.True(t, tracked, "an archived instance is preserved in the manager, not deleted like a kill")

	assert.Equal(t, session.Archived, persistedStatus(t, repoID, "worker"))
	rec := recordFor(t, repoID, "worker")
	require.NotNil(t, rec)
	assert.Equal(t, archivedPath, rec.Worktree.WorktreePath, "the persisted record must point at the relocated worktree")

	// The worktree is still a valid, registered git worktree at its new path.
	list, lerr := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain").CombinedOutput()
	require.NoError(t, lerr, string(list))
	assert.Contains(t, string(list), archivedPath, "git must still register the worktree at its new path")
}

// TestArchiveSession_MoveFailureMarksLost: when the worktree move fails, the
// instance is marked Lost (not Archived) with started still true, so the
// Lost-restore loop re-spawns the agent in place — and the worktree is left
// untouched at its original location.
func TestArchiveSession_MoveFailureMarksLost(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")

	// Force the move to fail by pre-creating the destination (MoveWorktree
	// refuses to clobber an existing dest), before any bytes are moved.
	dest, err := archivedWorktreePath(repoID, "worker")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dest, 0755))

	_, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "a failed move must surface an error")

	assert.Equal(t, session.Lost, inst.GetStatus(), "a failed archive marks the session Lost for self-heal")
	assert.True(t, inst.Started(), "started stays true so the Lost-restore loop re-spawns the agent")
	assert.True(t, exists(srcPath), "the worktree must remain at its original path on a failed archive")
	assert.Equal(t, session.Lost, persistedStatus(t, repoID, "worker"))
}

// TestArchiveSession_RejectsReservedRoot: the always-ensured root session cannot
// be archived.
func TestArchiveSession_RejectsReservedRoot(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, session.RootSessionTitle)

	_, err := manager.ArchiveSession(ArchiveSessionRequest{Title: session.RootSessionTitle, RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

// TestArchiveSession_RejectsAlreadyArchived: archiving an already-archived
// session is an error, not a second move.
func TestArchiveSession_RejectsAlreadyArchived(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetStatus(session.Archived)

	_, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already archived")
}

// TestArchiveSession_RejectsWhenOperationInFlight: an archive is refused while
// another destructive op (kill or archive) holds the session.
func TestArchiveSession_RejectsWhenOperationInFlight(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")

	key := daemonInstanceKey(repoID, "worker")
	manager.mu.Lock()
	manager.killsInFlight[key] = struct{}{}
	manager.mu.Unlock()

	_, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in progress")
}

type fakeRemoteBackend struct {
	*session.FakeBackend
}

func (fakeRemoteBackend) Type() string { return "remote" }

// TestArchiveSession_RejectsRemote: a remote session has no local worktree to
// relocate, so archiving it is rejected up front.
func TestArchiveSession_RejectsRemote(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "faraway", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(fakeRemoteBackend{session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	inst.SetStatus(session.Ready)
	seedDiskInstance(t, repoID, "faraway", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "faraway")] = inst
	manager.mu.Unlock()

	_, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "faraway", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote")
}
