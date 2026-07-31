package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
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

// TestRestoreArchived_RelocateCutOffReportsAFailedPersist is the other half of the
// branch above. Persisting is not a nicety there — it IS the mitigation, so a
// write that did not happen (full or read-only disk, drifted storage) reproduces
// exactly the stranding the branch exists to prevent. Reporting only the
// relocation error would tell the operator their worktree is safely recorded when
// nothing recorded it, which is worse than the original bug: it is the same data
// loss plus a reassurance.
//
// The forced failure is storage drift; see driftPersistedStableID for why that
// lever rather than a permissions denial.
func TestRestoreArchived_RelocateCutOffReportsAFailedPersist(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	// The durable write can no longer land: see driftPersistedStableID.
	driftPersistedStableID(t, repoID, "worker")

	partialRelocateGitOnPath(t)
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(300 * time.Millisecond))

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	require.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"the relocation error must still be reported")
	assert.Contains(t, err.Error(), "could not be written to disk",
		"a persist that failed must escalate: nothing durable points at the moved worktree")
	assert.Contains(t, err.Error(), "before restarting the daemon",
		"the operator needs to know a restart is what turns this into lost work")
}

// TestArchiveSession_TeardownFailureReportsAFailedPersist is the archive-side
// twin of the restore case above, and it exists because the two must not drift:
// since the relocate became bounded, this branch is reachable with the worktree
// already at the archive destination and only the in-memory object knowing it.
// A logged-and-swallowed write failure would leave the record pointing at the
// pre-archive path, so a restart would send the Lost-restore loop to rebuild a
// worktree whose bytes are in the archive directory.
func TestArchiveSession_TeardownFailureReportsAFailedPersist(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	driftPersistedStableID(t, repoID, "worker")
	partialRelocateGitOnPath(t)
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(300 * time.Millisecond))

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	require.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"the cut-off relocate must still be reported")
	assert.Contains(t, err.Error(), "could not record its recovered state on disk",
		"a persist that failed must escalate rather than be logged and swallowed")
	assert.Contains(t, err.Error(), "before restarting the daemon",
		"the operator needs to know a restart is what turns this into lost work")
}

// driftPersistedStableID makes the durable write for this title fail
// deterministically: the row keeps its title (so the action still resolves) but
// carries a different stable id, which persistInstanceData refuses to overwrite
// by design. Deterministic regardless of the uid the suite runs as — a
// permissions-based denial is not, since the container harness can run as root.
func driftPersistedStableID(t *testing.T, repoID, title string) {
	t.Helper()
	require.NoError(t, config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var records []session.InstanceData
		require.NoError(t, json.Unmarshal(raw, &records))
		require.NotEmpty(t, records)
		for i := range records {
			if records[i].Title == title {
				records[i].ID = "a-different-session-entirely"
			}
		}
		return json.Marshal(records)
	}))
}
