package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// #7752 property 2: a session whose archive was REQUESTED must never be
// re-spawned by the Lost-restore loop after its teardown refused. The incident
// was `af sessions archive --self`: the teardown refused on the requester
// itself, AbortArchiveToLost dropped the row to Lost with no fence, and the
// loop re-spawned the agent about a second later — undoing the archive the
// user asked for and leaving them a running session they had shelved.
//
// Each test drives one of the four requested-archive refusal sites and asserts
// the durable give-up the loop's entry gate reads, on the live row AND on disk
// (a restart must keep the fence). The local test also runs the loop itself,
// with a control arm that clears the fence and watches the same fixture
// recover — the proof that the fence, not some other gate, is what held it.

// teardownRefusal is the incident's refusal shape: the close path's survivors
// error, naming the requester pid as an undecidable af-daemon claimant.
var teardownRefusal = errors.New("pane processes 4242 are still alive after bounded teardown; " +
	"pid 4242 is the claimant itself — its argv could not be decided by af's own parse")

// requireArchiveFence asserts the durable restore fence on both the live row
// and its persisted record.
func requireArchiveFence(t *testing.T, inst *session.Instance, repoID, title, cause string) {
	t.Helper()
	view := inst.LifecycleView()
	require.True(t, view.LostRestoreGaveUp,
		"a refused requested-archive teardown must fence the automatic restore loop off the row")
	require.False(t, lostSessionWantsRestore(view),
		"the loop's entry gate must refuse the fenced row")

	failure := inst.LostRestoreFailureSnapshot()
	require.NotNil(t, failure)
	assert.Equal(t, 1, failure.Attempts)
	assert.Contains(t, failure.Error, "archive did not finish",
		"the row detail must say why the session is not restarting")
	assert.Contains(t, failure.Error, cause, "the row detail must keep the teardown's own cause")

	rec := recordFor(t, repoID, title)
	require.NotNil(t, rec)
	require.NotNil(t, rec.LostRestoreFailure,
		"the fence must be durable: a daemon restart would otherwise reload the row unfenced and re-spawn it")
	assert.Contains(t, rec.LostRestoreFailure.Error, "archive did not finish")
}

// The incident path: ArchiveSession's archiveTeardown refused.
func TestArchiveSession_RefusedTeardownFencesLostRestore(t *testing.T) {
	zeroRestoreBackoff(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "self-archive")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)

	prev := archiveTeardown
	archiveTeardown = func(*session.Instance, string, sessiongit.RelocationClaim, func() error, bool) (error, error) {
		return nil, teardownRefusal
	}
	t.Cleanup(func() { archiveTeardown = prev })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "self-archive", RepoID: repoID})
	require.ErrorIs(t, err, teardownRefusal)
	assert.Contains(t, err.Error(), "will not restart on its own",
		"the caller must be told the session stays down, not that it will be restored in place")
	assert.NotContains(t, err.Error(), "restored in place")

	// The rollback itself is unchanged: Lost, started kept, worktree home.
	require.Equal(t, session.Lost, inst.GetStatus())
	require.True(t, inst.Started())
	require.True(t, exists(srcPath))
	requireArchiveFence(t, inst, repoID, "self-archive", "pane processes 4242")

	// The loop must leave it alone — the re-spawn #7752 observed.
	manager.RestoreLostSessions()
	assert.Equal(t, 0, backend.recoverCalls(),
		"the Lost-restore loop re-spawned a session whose archive was requested")
	assert.Equal(t, session.Lost, inst.GetStatus())

	// Control: the same row with the fence lifted IS recovered by the same
	// loop, so the fence is the only thing that held it above.
	require.True(t, inst.ClearLostRestoreFailure())
	manager.RestoreLostSessions()
	assert.Equal(t, 1, backend.recoverCalls(),
		"control: without the fence this fixture must be recoverable, or the assertion above proved nothing")
}

// The way out stays open: a fenced Lost row is still archivable, so retrying
// the archive finishes the job the refused teardown left.
func TestArchiveSession_FencedRowAdmitsArchiveRetry(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "retry-me")

	prev := archiveTeardown
	archiveTeardown = func(*session.Instance, string, sessiongit.RelocationClaim, func() error, bool) (error, error) {
		return nil, teardownRefusal
	}
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "retry-me", RepoID: repoID})
	archiveTeardown = prev
	require.ErrorIs(t, err, teardownRefusal)
	requireArchiveFence(t, inst, repoID, "retry-me", "pane processes 4242")

	_, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "retry-me", RepoID: repoID})
	require.NoError(t, err, "a fenced Lost row must still admit an explicit archive retry")
	assert.Equal(t, session.LiveArchived, data.Liveness)
}

// The missing-worktree route's teardown refused (archive_gone.go).
func TestArchiveSession_GoneRouteRefusedTeardownFencesLostRestore(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, "gone-refused")

	prev := archiveGoneTeardown
	archiveGoneTeardown = func(*session.Instance, func() error, bool, bool) (error, error) {
		return nil, teardownRefusal
	}
	t.Cleanup(func() { archiveGoneTeardown = prev })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "gone-refused", RepoID: repoID})
	require.ErrorIs(t, err, teardownRefusal)
	assert.NotContains(t, err.Error(), "restored in place")
	require.Equal(t, session.LiveLost, inst.GetLiveness())
	requireArchiveFence(t, inst, repoID, "gone-refused", "pane processes 4242")
}

// The remote push route: the push landed and the sandbox reap did not
// (ArchiveSandbox). The loop must not re-provision a sandbox the user asked to
// archive on top of one whose reap is still owed.
func TestArchiveRemoteSession_ReapFailureFencesLostRestore(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	inst, _, reap := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "remote-refused", srv.url, session.Running)
	reap.fail(session.ErrWorkspaceStateUnknown)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "remote-refused", RepoID: repoID})
	require.Error(t, err)
	require.Equal(t, 1, reap.count(), "premise: the push route's reap is the step that failed")
	require.Equal(t, session.LiveLost, inst.GetLiveness())
	requireArchiveFence(t, inst, repoID, "remote-refused", "failed to tear the sandbox down")
}

// The remote preserve route: the agent stop failed (ArchiveSandboxPreserving).
// The remoteWorkspaceBackend has no preservable session directory, so the stop
// fails for real; BeginArchive first, as archiveRemoteSession always does.
func TestArchiveRemotePreserving_StopFailureFencesLostRestore(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	inst, _, _ := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "preserve-refused", srv.url, session.Running)
	require.NoError(t, inst.Transition(session.BeginArchive()))

	_, err := manager.archiveRemotePreserving(repoID, inst, "preserve-refused")
	require.Error(t, err)
	require.Equal(t, session.LiveLost, inst.GetLiveness())
	requireArchiveFence(t, inst, repoID, "preserve-refused", "no preservable session directory")
}
