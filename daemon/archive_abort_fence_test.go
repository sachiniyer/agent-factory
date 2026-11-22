package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	// #7754 property 4: the fence is labelled an archive refusal, not a
	// restore give-up — no restore attempt ever ran on this row. The kind is
	// additive; an older daemon reads attempts+error and still keeps the row
	// fenced (the field predates the label, so downgrade stays safe).
	assert.Equal(t, session.LostRestoreFailureArchiveRefused, failure.Kind)
	assert.True(t, strings.HasPrefix(failure.Detail(), "archive refused: "),
		"the API detail must name this an archive refusal, got %q", failure.Detail())
	assert.NotContains(t, failure.Detail(), "restore gave up")

	rec := recordFor(t, repoID, title)
	require.NotNil(t, rec)
	require.NotNil(t, rec.LostRestoreFailure,
		"the fence must be durable: a daemon restart would otherwise reload the row unfenced and re-spawn it")
	assert.Contains(t, rec.LostRestoreFailure.Error, "archive did not finish")
	assert.Equal(t, session.LostRestoreFailureArchiveRefused, rec.LostRestoreFailure.Kind,
		"the refusal label must be durable too")
	assert.True(t, strings.HasPrefix(rec.LostRestoreFailure.Detail(), "archive refused: "))
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

// #7754 property 1, the first rollback arm: the committed archive whose FINAL
// VS Code sweep could not confirm the stop rolls back to Lost. Before this fix
// undoCommittedArchive left the row Lost with NO fence, and the automatic
// restore loop re-spawned the agent the user had just asked to stop — the same
// resurrection #7752 reported, reached one step later in the flow.
func TestArchiveSession_FinalVSCodeStopRollbackFencesLostRestore(t *testing.T) {
	zeroRestoreBackoff(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, origPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)
	plantStuckEditorAfterTeardown(t, manager, inst, daemonInstanceKey(repoID, "worker"), origPath)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rolled the archive back and left it Lost",
		"the caller must be told the archive was undone, not that it committed")
	require.Equal(t, origPath, inst.GetWorktreePath(),
		"precondition: the rollback-SUCCESS arm ran — the worktree is home")
	require.Equal(t, session.Lost, inst.GetStatus())
	requireArchiveFence(t, inst, repoID, "worker", "VS Code editor teardown")

	// The point of the fence: the loop leaves the row down.
	manager.RestoreLostSessions()
	assert.Equal(t, 0, backend.recoverCalls(),
		"a rolled-back requested archive must never be auto-restored into a live agent")
	assert.Equal(t, session.Lost, inst.GetStatus())
}

// #7754 property 1, the second rollback arm: same committed archive, but the
// durable write is what failed. Only the commit write is stubbed — the undo's
// own persistInstanceErr must succeed so the fence lands on disk.
func TestArchiveSession_PersistFailureRollbackFencesLostRestore(t *testing.T) {
	zeroRestoreBackoff(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)

	previous := archivePersist
	commitWriteFailed := errors.New("forced persist failure")
	calls := 0
	archivePersist = func(m *Manager, rid string, i *session.Instance) error {
		calls++
		if calls == 1 {
			return commitWriteFailed
		}
		return previous(m, rid, i)
	}
	t.Cleanup(func() { archivePersist = previous })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.ErrorIs(t, err, commitWriteFailed)
	assert.Contains(t, err.Error(), "rolled it back and left it Lost")
	require.Equal(t, session.Lost, inst.GetStatus())
	requireArchiveFence(t, inst, repoID, "worker", "forced persist failure")

	manager.RestoreLostSessions()
	assert.Equal(t, 0, backend.recoverCalls(),
		"a rolled-back requested archive must never be auto-restored into a live agent")
}

// The double failure: the rollback home succeeded but writing the fenced
// recovered state failed too. The in-memory row is still fenced (a restart is
// the exposure), and the returned error must SAY the fence could not be made
// durable — a silent drop would leave the operator believing the session is
// safely down when the next daemon start re-spawns it.
func TestArchiveSession_RollbackFencePersistFailureIsReported(t *testing.T) {
	zeroRestoreBackoff(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)

	// Make EVERY durable write for this repo fail by sealing the on-disk
	// instances directory — the undo's persist is persistInstanceErr on the
	// real path, so a stub cannot reach it.
	repoDir := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "instances", repoID)
	require.NoError(t, os.Chmod(repoDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(repoDir, 0o755) })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rolled the archive back and left it Lost",
		"the rollback still happened and must still be reported")
	assert.Contains(t, err.Error(), "could not record its recovered state on disk",
		"the unfenced-on-disk exposure must be named, not swallowed")
	require.Equal(t, session.Lost, inst.GetStatus())

	// The in-memory fence still holds — the exposure is the NEXT daemon start.
	failure := inst.LostRestoreFailureSnapshot()
	require.NotNil(t, failure)
	assert.Equal(t, session.LostRestoreFailureArchiveRefused, failure.Kind)
	require.False(t, lostSessionWantsRestore(inst.LifecycleView()))
	manager.RestoreLostSessions()
	assert.Equal(t, 0, backend.recoverCalls())
}

// #7754 property 3: the heal. A Lost row fenced by a refused archive whose SAME
// runtime answers the liveness probe alive leaves Lost and durably clears the
// failure — the fence keeps zombies down, it must not pin a recovered session
// in a terminal state.
func TestRefreshStatuses_AliveObservationClearsArchiveRefusedFence(t *testing.T) {
	zeroRestoreBackoff(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "heal-me", session.NewFakeBackend(), true, session.Lost)

	// Fence the row exactly the way a refused requested archive does — same
	// field, same kind — then make it durable so the clear below must write.
	require.NoError(t, inst.Transition(session.BeginArchive()))
	manager.abortRequestedArchiveToLost(inst, teardownRefusal)
	require.NoError(t, manager.persistInstanceErr(repoID, inst))
	requireArchiveFence(t, inst, repoID, "heal-me", "pane processes 4242")

	manager.RefreshStatuses()

	require.Equal(t, session.Ready, inst.GetLiveness(),
		"the same runtime observed alive must heal the row out of Lost")
	require.Nil(t, inst.LostRestoreFailureSnapshot(),
		"the archive-refused fence is cleared like any terminal restore failure")

	rec := recordFor(t, repoID, "heal-me")
	require.NotNil(t, rec)
	require.Nil(t, rec.LostRestoreFailure,
		"the clear must be durable: a restart would otherwise re-fence a live session")
}
