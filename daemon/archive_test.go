package daemon

import (
	"errors"
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
	inst.SetStatusForTest(session.Ready)

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

	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
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

// TestArchiveSessionRejectsStartupUnknown prevents archive from turning an
// uncertain runtime identity into a destructive worktree move. The only safe
// lifecycle operation on this record is an explicit kill/cleanup chosen by the
// user after inspection.
func TestArchiveSessionRejectsStartupUnknown(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "uncertain")
	inst.MarkStartupStateUnknown()

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "uncertain", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startup")
	assert.Contains(t, err.Error(), "unknown")
	assert.True(t, exists(srcPath), "archive moved a worktree whose runtime identity is unknown")
	assert.NotEqual(t, session.Archived, inst.GetStatus())
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

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "a failed move must surface an error")

	assert.Equal(t, session.Lost, inst.GetStatus(), "a failed archive marks the session Lost for self-heal")
	assert.True(t, inst.Started(), "started stays true so the Lost-restore loop re-spawns the agent")
	assert.True(t, exists(srcPath), "the worktree must remain at its original path on a failed archive")
	assert.Equal(t, session.Lost, persistedStatus(t, repoID, "worker"))
}

// TestArchiveSession_PersistFailureRollsBack is the #1538 regression: when the
// durable persist of the committed Archived state fails, the archive must NOT be
// left half-recorded (a stale on-disk record pointing at the vacated pre-archive
// worktree, which would orphan the worktree after a daemon restart). Instead the
// worktree is moved back to its original location and the session dropped to
// Lost, so the #1108 restore loop heals it and the on-disk record matches
// reality.
func TestArchiveSession_PersistFailureRollsBack(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")

	prev := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error {
		return errors.New("forced persist failure")
	}
	t.Cleanup(func() { archivePersist = prev })

	dest, derr := archivedWorktreePath(repoID, "worker")
	require.NoError(t, derr)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "a persist failure must surface an error, not a silent half-archive")

	assert.Equal(t, session.Lost, inst.GetStatus(), "a persist failure rolls the archive back to Lost for self-heal")
	assert.True(t, inst.Started(), "started stays true so the Lost-restore loop re-spawns the agent")
	assert.Equal(t, srcPath, inst.GetWorktreePath(), "the worktree must be moved back to its original location")
	assert.True(t, exists(srcPath), "the worktree must exist at its original location again")
	assert.False(t, exists(dest), "the archive directory must be vacated by the rollback")

	// The rolled-back Lost state is persisted (via the real writer in
	// undoCommittedArchive), so a restart sees a record consistent with reality.
	assert.Equal(t, session.Lost, persistedStatus(t, repoID, "worker"))
	rec := recordFor(t, repoID, "worker")
	require.NotNil(t, rec)
	assert.Equal(t, srcPath, rec.Worktree.WorktreePath, "the persisted record must point back at the original worktree")

	// The worktree is a valid, registered git worktree back at the original path.
	list, lerr := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain").CombinedOutput()
	require.NoError(t, lerr, string(list))
	assert.Contains(t, string(list), srcPath, "git must register the worktree back at the original path")
}

// TestArchiveSession_RejectsReservedRoot: the always-ensured root session cannot
// be archived.
func TestArchiveSession_RejectsReservedRoot(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, session.RootSessionTitle)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: session.RootSessionTitle, RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

// TestArchiveSession_RejectsAlreadyArchived: archiving an already-archived
// session is an error, not a second move.
func TestArchiveSession_RejectsAlreadyArchived(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetStatusForTest(session.Archived)

	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	// The message single-session callers (CLI/TUI, and every caller across the
	// control RPC, where the sentinel cannot survive) show is unchanged...
	assert.Contains(t, err.Error(), "already archived")
	// ...and it is matchable as ErrAlreadyArchived, which is what lets an in-process
	// bulk caller treat it as idempotent success (#2108). The resolved identity
	// comes back with it so that caller can report the row it skipped.
	assert.ErrorIs(t, err, ErrAlreadyArchived)
	assert.Equal(t, "worker", archived.Title)
	assert.Equal(t, inst.ID, archived.ID)
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

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in progress")
}

// TestArchiveSession_RejectsExternalWorktree (#1028 Greptile P1): an
// in-place/external worktree (an `--here` session, or root) cannot be archived —
// archive relocates the worktree, which MoveWorktree refuses for external
// worktrees. The rejection must happen UP FRONT so nothing is torn down: the
// session is left completely untouched (status unchanged, still started, its
// worktree in place), never a broken half-archive that rolls back to Lost.
func TestArchiveSession_RejectsExternalWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// An in-place/external worktree: the worktree IS the repo root, external=true.
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, "inplace", "master", "", true, false)
	require.NoError(t, err)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "inplace", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(gw)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	seedDiskInstance(t, repoID, "inplace", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "inplace")] = inst
	manager.mu.Unlock()

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "inplace", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in-place")

	// Untouched: rejected before any teardown.
	assert.Equal(t, session.Ready, inst.GetStatus(), "an unarchivable session must not be torn down or flipped Lost")
	assert.True(t, inst.Started(), "an unarchivable session must stay started")
	assert.True(t, exists(repoPath), "the user's in-place worktree must be untouched")
}

// TestRestoreArchived_MovesWorktreeBackAndRespawns: the happy path — an archived
// session's worktree is moved back to the standard sibling location, re-
// registered, the agent re-spawned (Recover flips Running), started=true, and
// the record persisted as Running at the restored path.
func TestRestoreArchived_MovesWorktreeBackAndRespawns(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, session.Archived, inst.GetStatus())
	archivedPath := inst.GetWorktreePath()

	// Compute the expected restore path BEFORE restore, while it is still free
	// (afterwards the restored worktree occupies it, and RestoreWorktreePath
	// would return the "-2" suffix).
	expected, perr := sessiongit.RestoreWorktreePath(repoPath, "worker", inst.GetBranch())
	require.NoError(t, perr)

	worktreePath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	assert.Equal(t, expected, worktreePath, "restore must land the worktree at the standard sibling location")
	assert.False(t, exists(archivedPath), "the archive directory must be gone after restore")
	assert.True(t, exists(worktreePath), "the worktree must exist at the restored path")

	dirty, rerr := os.ReadFile(filepath.Join(worktreePath, "dirty.txt"))
	require.NoError(t, rerr, "the uncommitted tree must survive the round trip")
	assert.Equal(t, "uncommitted", string(dirty))

	assert.Equal(t, 1, backend.recoverCalls(), "restore re-spawns the agent exactly once")
	assert.Equal(t, session.Running, inst.GetStatus(), "a restored session is Running")
	assert.True(t, inst.Started(), "a restored session is started")
	assert.Equal(t, session.Running, persistedStatus(t, repoID, "worker"))
	rec := recordFor(t, repoID, "worker")
	require.NotNil(t, rec)
	assert.Equal(t, worktreePath, rec.Worktree.WorktreePath)

	list, lerr := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain").CombinedOutput()
	require.NoError(t, lerr, string(list))
	assert.Contains(t, string(list), worktreePath, "git must register the worktree at the restored path")
}

// TestRestoreArchived_RejectsNonArchived: restoring a live (non-archived) session
// is an error.
func TestRestoreArchived_RejectsNonArchived(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker") // status Ready

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not archived")
}

// TestRestoreArchived_RejectsPendingKill is the #2208 regression: an archived
// row whose kill teardown was uncertain keeps its record and durable tombstone.
// Restore must honor that terminal intent before moving the worktree or starting
// a replacement runtime; otherwise the next status poll immediately finishes the
// kill that restore just appeared to undo.
func TestRestoreArchived_RejectsPendingKill(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()

	// Model the only failed-kill shape that retains a record: teardown did not
	// establish whether the pane/workspace is gone, so KillSession records the
	// tombstone and leaves the archived row addressable for its retry loop.
	backend := session.NewFakeBackend()
	backend.CompleteStart()
	inst.SetBackend(failKillBackend{readyFakeBackend{backend}})
	_, err = manager.KillSession(KillSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	require.True(t, inst.UserKilled(), "the failed kill must leave its terminal intent on the retained row")

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "restore must not revive a session whose kill is pending")
	assert.Contains(t, err.Error(), "pending kill", "the refusal must explain why retrying restore cannot work")
	assert.Equal(t, session.Archived, inst.GetStatus())
	assert.Equal(t, archivedPath, inst.GetWorktreePath(), "a refused restore must not move the archived worktree")
	assert.True(t, exists(archivedPath), "the archived worktree must stay intact for the pending kill retry")
}

// TestRestoreArchived_RepoGoneLeavesArchiveIntact: when the origin repo has been
// deleted, restore fails with an actionable message and leaves the archived
// worktree and the Archived status untouched.
func TestRestoreArchived_RepoGoneLeavesArchiveIntact(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()

	require.NoError(t, os.RemoveAll(repoPath), "simulate the origin repo being deleted")

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gone")
	assert.True(t, exists(archivedPath), "the archived worktree must be left intact when the repo is gone")
	assert.Equal(t, session.Archived, inst.GetStatus(), "a failed restore must leave the session Archived")
}

// TestRestoreArchived_CollisionSuffixesPath: when the standard sibling location
// is occupied at restore time, the worktree is restored to a suffixed path.
func TestRestoreArchived_CollisionSuffixesPath(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	// Occupy the default sibling location so restore must suffix.
	base, perr := sessiongit.RestoreWorktreePath(repoPath, "worker", inst.GetBranch())
	require.NoError(t, perr)
	require.NoError(t, os.MkdirAll(base, 0755))

	worktreePath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	assert.Equal(t, base+"-2", worktreePath, "restore must avoid clobbering an occupied sibling path")
	assert.True(t, exists(filepath.Join(worktreePath, "dirty.txt")))
}

type fakeRemoteBackend struct {
	*session.FakeBackend
}

func (fakeRemoteBackend) Type() string { return "remote" }

func (fakeRemoteBackend) Capabilities() session.Capabilities {
	return session.Capabilities{Workspace: session.WorkspaceRemote}
}

// TestArchiveSession_RejectsRemote: a remote session has no local worktree to
// relocate, so archiving it is rejected up front.
func TestArchiveSession_RejectsRemote(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "faraway", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(fakeRemoteBackend{session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	seedDiskInstance(t, repoID, "faraway", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "faraway")] = inst
	manager.mu.Unlock()

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "faraway", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote")
}
