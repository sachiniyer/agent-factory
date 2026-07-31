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
// the archive that precede this are genuine. The returned heal() drops the
// misbehavior so the same test can then drive a healthy retry.
func partialRelocateGitOnPath(t *testing.T) (heal func()) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)

	dir := t.TempDir()
	shim := filepath.Join(dir, "git")
	// Args always arrive as: git -C <path> <verb> <subverb> …
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = \"worktree\" ] && [ \"$4\" = \"repair\" ]; then sleep 300 & wait; fi\n" +
		"if [ \"$3\" = \"worktree\" ] && [ \"$4\" = \"move\" ]; then\n" +
		"  echo 'simulated: git worktree move refused (cross-device)' >&2; exit 1\n" +
		"fi\n" +
		"exec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// heal rewrites the shim as a pass-through, so a test can exercise a healthy
	// retry through the same PATH. Rewriting the file rather than reading an env
	// var is deliberate: runGitCommandContext filters the environment it hands
	// git, so a test-only variable would not survive the call.
	return func() {
		require.NoError(t, os.WriteFile(shim, []byte("#!/bin/sh\nexec "+realGit+" \"$@\"\n"), 0o755))
	}
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
	heal := partialRelocateGitOnPath(t)
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

	// ...and the retry the error advertises has to actually work. The session is
	// still Archived (RestoreFromArchive never ran), and the standard restore path
	// is now occupied by the half-relocated worktree — RestoreWorktreePath's
	// collision suffix is what keeps that from being a dead end, so assert the
	// whole round trip rather than trusting it.
	heal()
	retryPath, _, retryErr := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, retryErr, "the advertised retry must be able to finish the restore")
	assert.NotEqual(t, expected, retryPath,
		"the retry relocates off the occupied path rather than colliding with itself")
	assert.True(t, exists(retryPath), "the worktree must exist at the retried location")
	assert.Equal(t, session.Running, inst.GetStatus(), "the retried restore re-spawns the agent")
	assert.Equal(t, retryPath, recordFor(t, repoID, "worker").Worktree.WorktreePath,
		"and the durable record follows it")
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

	_ = partialRelocateGitOnPath(t)
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
	_ = partialRelocateGitOnPath(t)
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

// TestReserveCreate_ReuseArchivedNameCutOffRecordsTheLocation is the THIRD
// caller of the bounded relocate, and the one with no rollback to fall back on.
//
// renameArchivedForReuseLocked frees an archived session's title so a new
// session can take it (#2127). When the relocate is cut off after the bytes have
// reached the new archive path, RenameArchived returns before it changes the
// title — so the re-key and the durable rewrite below it are both skipped, and
// the persisted row keeps pointing at an archive directory that is no longer
// there. A restart strands the worktree, exactly as archive and restore would
// have before their own handling.
func TestReserveCreate_ReuseArchivedNameCutOffRecordsTheLocation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Branch freed: #2127's guard refuses before the rename whenever the archived
	// session still holds the branch the create derives, and this must REACH the
	// relocate.
	archived, _ := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	origDest, err := archivedWorktreePath(repoID, "foo")
	require.NoError(t, err)
	newDest, err := archivedWorktreePath(repoID, "foo (archived)")
	require.NoError(t, err)
	require.True(t, exists(origDest), "precondition: the archive starts at the pre-rename path")

	_ = partialRelocateGitOnPath(t)
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(300 * time.Millisecond))

	_, _, release, _, rerr := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "foo",
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.Error(t, rerr, "the create must abort when the archived name could not be freed")
	require.ErrorIs(t, rerr, sessiongit.ErrRelocateStateUnknown)

	// The bytes moved even though the rename did not complete — which is the whole
	// hazard: the location changed and only memory knew it.
	require.True(t, exists(newDest), "premise: the manual move placed the worktree at the new path")
	require.False(t, exists(origDest), "premise: it is no longer at the pre-rename path")
	assert.Equal(t, "foo", archived.Title, "the rename did not happen: the title is unchanged")

	rec := recordFor(t, repoID, "foo")
	require.NotNil(t, rec, "the row is still keyed by its original title")
	assert.Equal(t, newDest, rec.Worktree.WorktreePath,
		"the durable record must follow the bytes: a restart that reloads the pre-rename path "+
			"can never reach this archive again")
}
