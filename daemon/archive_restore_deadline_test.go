package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// partialRelocateGitOnPath installs a `git` shim that reproduces the exact
// restore shape this guards: the fast path refuses (as it does across
// filesystems), so the manual move lands the bytes at dest — and then
// `git worktree repair` makes no progress, so the bounded runner SIGKILLs it.
//
// Everything else execs the real git, so the repo, the worktree registration and
// the archive that precede this are genuine.
func partialRelocateGitOnPath(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)

	dir := t.TempDir()
	// Args always arrive as: git -C <path> <verb> <subverb> …
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = \"worktree\" ] && [ \"$4\" = \"repair\" ]; then sleep 300 & wait; fi\n" +
		"if [ \"$3\" = \"worktree\" ] && [ \"$4\" = \"move\" ]; then\n" +
		"  echo 'simulated: git worktree move refused (cross-device)' >&2; exit 1\n" +
		"fi\n" +
		"exec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRestoreArchived_RelocateCutOffPersistsTheNewLocation is the restore half of
// the bounded-relocate contract.
//
// The git layer commits the new worktree location to the object BEFORE repairing,
// precisely because a stale registration is recoverable while a lost location is
// not. That new location lives only in memory, and this failure path returns
// without persisting — so a daemon restart would reload the session as Archived
// pointing at an archive directory that is no longer there, and every later
// restore would fail the source-exists guard while the user's work sat, intact
// and unreachable, at the restore path.
//
// Archive already refuses to finalize over this state (teardown's stateUnknown).
// Restore has no teardown to gate, so it must instead durably record what DID
// happen before reporting the failure.
func TestRestoreArchived_RelocateCutOffPersistsTheNewLocation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Equal(t, archivedPath, recordFor(t, repoID, "worker").Worktree.WorktreePath,
		"precondition: the archive location is what is persisted going in")

	expected, perr := sessiongit.RestoreWorktreePath(repoPath, "worker", inst.GetBranch())
	require.NoError(t, perr)

	// Only now: the archive above ran on real git.
	partialRelocateGitOnPath(t)
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(300 * time.Millisecond))

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err, "a restore cut off mid-relocate must not report success")
	require.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), expected,
		"the error must name where the worktree actually is, so the operator can find it")

	// The bytes really did land, which is why losing the pointer would be so bad.
	assert.True(t, exists(expected), "premise: the manual move placed the worktree at the restore path")
	assert.False(t, exists(archivedPath), "premise: it is no longer at the archive path")

	rec := recordFor(t, repoID, "worker")
	require.NotNil(t, rec)
	assert.Equal(t, expected, rec.Worktree.WorktreePath,
		"the new location must be persisted before returning: a restart that reloads the old, "+
			"now-missing archive path can never restore this session again")
}
