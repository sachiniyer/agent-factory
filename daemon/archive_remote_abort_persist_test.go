package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The remote archive abort path — push landed, teardown failed — rolls the
// fence back to Lost and persists the recovery state. That persist is the ONLY
// writer of the recovery-critical state to disk (the pushed branch, the
// unknown-cleanup fence). The local route's identical rollback uses the
// error-returning persistInstanceErr and surfaces a failed write; both remote
// abort paths (archiveRemoteSession and archiveRemotePreserving) used the
// best-effort persistInstance, which logs a failed write and returns an error
// that never mentions it — leaving the on-disk row with an empty branch while
// naming the pushed branch as if it were recorded. After a daemon restart the
// empty Branch makes restore re-provision the sandbox on the repo's default
// branch, stranding the pushed work — the exact failure #1781/#3335 describe
// and fix elsewhere.
//
// These tests pin the fix: the remote abort paths mirror the local route and
// surface the persist failure, so the operator knows the recovery record may
// not survive a restart.

// TestArchiveRemoteSession_AbortPersistFailureIsSurfaced is the central
// assertion: when ArchiveSandbox fails after the push landed (so the branch is
// in memory but the on-disk row does not know it) AND the rollback persist
// fails, the returned error must name BOTH the archive failure and the persist
// failure — not just the archive failure with the pushed branch named as if it
// were recorded.
//
// The reap fails with ErrWorkspaceStateUnknown so the push has already run
// (recordArchivePush set the branch in memory) before the teardown fails. The
// testHookPersistInstanceData hook injects a persist failure for this session's
// row only, so every other write in the same archive stays real.
func TestArchiveRemoteSession_AbortPersistFailureIsSurfaced(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	inst, _, reap := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "abort-undurable", srv.url, session.Running)

	// Push landed, teardown did not — the exact half-failed archive whose
	// rollback persist is the only writer of the branch to disk.
	reap.fail(session.ErrWorkspaceStateUnknown)

	// Fail the rollback persist for this session's row only. The hook gates
	// every write through persistInstanceData, so gating on the title keeps
	// every other write in the same archive real.
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(id string, data session.InstanceData) error {
		if id == repoID && data.Title == "abort-undurable" {
			return errors.New("no space left on device")
		}
		return prev(id, data)
	}

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "abort-undurable", RepoID: repoID})
	require.Error(t, err, "an archive whose teardown and rollback persist both failed must not report success")

	// The persist failure must be surfaced — not swallowed. The old code
	// returned an error that named the pushed branch while the disk row held
	// an empty branch, telling the operator the work was recorded when it was
	// not.
	assert.Contains(t, err.Error(), "could not record its recovered state on disk",
		"the persist failure must be surfaced so the operator knows the recovery record may not survive a restart")
	assert.Contains(t, err.Error(), "no space left on device",
		"the persist failure cause must be named so the operator can act on it")
	assert.Contains(t, err.Error(), "af/pushed-branch",
		"the pushed branch must still be named for manual recovery — it is on origin even if the session record does not say so")

	// NOT committed: the archive failed and the rollback persist failed, so
	// nothing landed durably. A committed marker would let DeleteProject
	// deregister the project over a stale row — the same hazard #3335's
	// review fixed for the commit-side persist.
	assert.False(t, isMutationCommitted(err),
		"an archive whose rollback persist failed must not claim committed: nothing reached disk")

	// The branch IS in memory — recordArchivePush ran before the teardown
	// failed — so in-process recovery still works. The hazard is only across a
	// restart, when the disk row (not memory) is authoritative.
	assert.Equal(t, "af/pushed-branch", inst.GetBranch(),
		"in memory the branch is still recorded — the hazard is the disk row, not the live instance")

	// The disk row does NOT know the branch: the persist failed, so the on-disk
	// record still carries the seeded empty value. This is the stranding
	// scenario — a restart here loads Branch="" and the Lost re-provision
	// clones the repo's default branch.
	rec := recordFor(t, repoID, "abort-undurable")
	require.NotNil(t, rec, "the record must still exist — the persist failed, it was not deleted")
	assert.Empty(t, rec.Branch,
		"the on-disk row must NOT name the pushed branch: the persist failed, so a restart would load an empty branch and strand the pushed work")
}

// TestArchiveRemotePreserving_AbortPersistFailureIsSurfaced is the preserve
// route's twin: when ArchiveSandboxPreserving fails and the rollback persist
// also fails, the returned error must name the persist failure. The preserve
// route's severity is lower than the push route's (no in-memory-only branch on
// this path), but the Lost rollback itself is recovery-critical state that only
// reaches disk through this one persist.
//
// The remoteWorkspaceBackend does not implement runtimeCleanupProvider, so
// ArchiveSandboxPreserving fails with "no preservable session directory" — a
// genuine stop failure that exercises the abort path. BeginArchive is called
// first to put the instance in the state archiveRemotePreserving expects (it is
// always called after BeginArchive in production).
func TestArchiveRemotePreserving_AbortPersistFailureIsSurfaced(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	inst, _, _ := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "preserve-abort", srv.url, session.Running)

	// Enter the archive fence, as archiveRemoteSession does before dispatching
	// to the preserve route.
	require.NoError(t, inst.Transition(session.BeginArchive()))

	// Fail the rollback persist for this session's row only.
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(id string, data session.InstanceData) error {
		if id == repoID && data.Title == "preserve-abort" {
			return errors.New("no space left on device")
		}
		return prev(id, data)
	}

	_, err := manager.archiveRemotePreserving(repoID, inst, "preserve-abort")
	require.Error(t, err, "a preserve archive whose stop and rollback persist both failed must not report success")

	// The persist failure must be surfaced — not swallowed.
	assert.Contains(t, err.Error(), "could not record its recovered state on disk",
		"the persist failure must be surfaced so the operator knows the recovery record may not survive a restart")
	assert.Contains(t, err.Error(), "no space left on device",
		"the persist failure cause must be named so the operator can act on it")
	assert.False(t, isMutationCommitted(err),
		"an archive whose rollback persist failed must not claim committed: nothing reached disk")
}
