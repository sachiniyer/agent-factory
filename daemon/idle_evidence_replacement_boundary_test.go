package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

type liveBeforeRecoverReturnBackend struct {
	*session.FakeBackend
	live    chan struct{}
	release chan struct{}
}

type liveBeforeRespawnReturnBackend struct {
	*limitResumeBackend
	live    chan struct{}
	release chan struct{}
}

func (b *liveBeforeRespawnReturnBackend) Respawn(inst *session.Instance) error {
	b.mu.Lock()
	b.respawnCalls++
	mutate := b.onRespawn
	b.mu.Unlock()
	if mutate != nil {
		mutate(inst)
	}
	if err := inst.Transition(session.ConfirmLive()); err != nil {
		return err
	}
	close(b.live)
	<-b.release
	return nil
}

func (b *liveBeforeRecoverReturnBackend) Recover(inst *session.Instance) error {
	if err := inst.Transition(session.ConfirmLive()); err != nil {
		return err
	}
	close(b.live)
	<-b.release
	return nil
}

// Recovery must retire predecessor evidence durably before the backend exposes
// its replacement. Production backends confirm the fresh runtime live before
// Recover returns, so persisting only afterward leaves a crash window in which
// restart reattaches that replacement with the predecessor's evidence.
func TestRecoverRetiresIdleEvidenceBeforeReplacementIsExposed(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &liveBeforeRecoverReturnBackend{
		FakeBackend: session.NewFakeBackend(),
		live:        make(chan struct{}),
		release:     make(chan struct{}),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "recover-boundary", backend, true, session.Lost)
	attemptedAt := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt))
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), inst.StateEpoch()))
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(backend.release) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreSession(RestoreSessionRequest{
			Title: inst.Title, RepoID: repoID,
		})
		done <- err
	}()

	select {
	case <-backend.live:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement backend never became live")
	}
	rec := recordFor(t, repoID, inst.Title)
	require.True(t, rec.LastPromptAttemptAt.IsZero(),
		"replacement became live while disk still carried the predecessor prompt attempt")
	require.Empty(t, rec.LastPromptDeliveryStatus,
		"replacement became live while disk still carried the predecessor delivery verdict")
	require.True(t, rec.LastPaneChurnAt.IsZero(),
		"replacement became live while disk still carried the predecessor pane churn")

	release()
	require.NoError(t, <-done)
}

// A limit resume can also replace an exited agent process. Its ConfirmLive edge
// must retire the limit-blocked predecessor's evidence before the replacement is
// exposed, just like Lost recovery; bash-like pane churn after the replacement
// must never be ordered against the predecessor prompt.
func TestLimitRespawnRetiresIdleEvidenceBeforeReplacementIsExposed(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &liveBeforeRespawnReturnBackend{
		limitResumeBackend: &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: false},
		live:               make(chan struct{}),
		release:            make(chan struct{}),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "limit-boundary", backend, true, session.Running)
	inst.Prompt = "continue the task"
	inst.SetLimitReached(time.Time{})
	attemptedAt := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt))
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), inst.StateEpoch()))
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(backend.release) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		done <- manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID})
	}()

	select {
	case <-backend.live:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement backend never became live")
	}
	rec := recordFor(t, repoID, inst.Title)
	require.True(t, rec.LastPromptAttemptAt.IsZero(),
		"limit replacement became live while disk still carried the predecessor prompt attempt")
	require.Empty(t, rec.LastPromptDeliveryStatus,
		"limit replacement became live while disk still carried the predecessor delivery verdict")
	require.True(t, rec.LastPaneChurnAt.IsZero(),
		"limit replacement became live while disk still carried the predecessor pane churn")

	release()
	require.NoError(t, <-done)
}

// A remote archive restore has already created a fresh runtime when it clears
// predecessor evidence. If that write fails, the clear must remain enrolled for
// settlement retry; otherwise an unclean exit reloads the archived runtime's
// evidence onto the surviving replacement.
func TestRemoteArchiveIdleEvidenceClearPersistFailureIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-archive-settlement", "http://127.0.0.1:1", session.Archived)
	attemptedAt := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt))
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), inst.StateEpoch()))
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	failedWrites, seen, heal := fullDiskFor(t, inst.Title, errors.New("disk full"))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: inst.Title, RepoID: repoID})
	require.NoError(t, err)
	require.NotZero(t, failedWrites(), "test failed no write; saw %s", seen())
	require.False(t, recordFor(t, repoID, inst.Title).LastPromptAttemptAt.IsZero(),
		"failed clear unexpectedly reached disk")

	heal()
	manager.FlushOwedSettlements()
	rec := recordFor(t, repoID, inst.Title)
	require.True(t, rec.LastPromptAttemptAt.IsZero())
	require.Empty(t, rec.LastPromptDeliveryStatus)
	require.True(t, rec.LastPaneChurnAt.IsZero())
}
