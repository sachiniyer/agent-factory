package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

func TestSendPromptEvidencePersistFailureIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := observedPromptBackend{
		readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()},
		status:           session.PromptDelivered,
	}
	inst := registerStarted(t, manager, repoID, repoPath, "prompt-settlement", backend, true, session.Ready)
	failedWrites, seen, heal := fullDiskFor(t, inst.Title, errors.New("disk full"))

	status, err := manager.SendPromptWithStatus(SendPromptRequest{
		Title: inst.Title, RepoID: repoID, Prompt: "ship it",
	})
	require.NoError(t, err)
	require.Equal(t, session.PromptDelivered, status)
	require.NotZero(t, failedWrites(), "test failed no write; saw %s", seen())
	require.True(t, recordFor(t, repoID, inst.Title).LastPromptAttemptAt.IsZero(),
		"failed checkpoint unexpectedly reached disk")

	heal()
	manager.FlushOwedSettlements()
	rec := recordFor(t, repoID, inst.Title)
	require.False(t, rec.LastPromptAttemptAt.IsZero())
	require.Equal(t, session.PromptDelivered, rec.LastPromptDeliveryStatus)
}

func TestPostPromptChurnPersistFailureIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "churn-settlement", &updatedOnlyBackend{
		FakeBackend: session.NewFakeBackend(),
	}, true, session.Running)
	attemptedAt := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt))
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	failedWrites, seen, heal := fullDiskFor(t, inst.Title, errors.New("disk full"))

	manager.refreshInstanceStatus(repoID, inst)
	manager.refreshInstanceStatus(repoID, inst) // the one-shot checkpoint edge is now consumed in memory
	require.NotZero(t, failedWrites(), "test failed no write; saw %s", seen())
	require.True(t, recordFor(t, repoID, inst.Title).LastPaneChurnAt.IsZero(),
		"failed checkpoint unexpectedly reached disk")

	heal()
	manager.FlushOwedSettlements()
	rec := recordFor(t, repoID, inst.Title)
	require.True(t, rec.LastPaneChurnAt.After(attemptedAt),
		"retried churn %v must be after prompt %v", rec.LastPaneChurnAt, attemptedAt)
}

func TestSuccessfulLimitResumePersistFailureIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "resume-settlement", backend, true, session.Running)
	inst.SetLimitReached(time.Time{})
	// Seed the blocked pre-resume row before failing the resume's final write. A
	// restart from this record would otherwise lose both the successful prompt
	// attempt and the fact that the limit was cleared.
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	failedWrites, seen, heal := fullDiskFor(t, inst.Title, errors.New("disk full"))

	require.NoError(t, manager.resumeFromLimit(ResumeFromLimitRequest{
		Title: inst.Title, RepoID: repoID,
	}))
	require.NotZero(t, failedWrites(), "test failed no write; saw %s", seen())
	rec := recordFor(t, repoID, inst.Title)
	require.True(t, rec.LastPromptAttemptAt.IsZero(),
		"failed final checkpoint unexpectedly reached disk")
	require.Equal(t, session.LiveLimitReached, rec.Liveness)

	heal()
	manager.FlushOwedSettlements()
	rec = recordFor(t, repoID, inst.Title)
	require.False(t, rec.LastPromptAttemptAt.IsZero())
	require.Equal(t, session.PromptCouldNotConfirm, rec.LastPromptDeliveryStatus)
	require.Equal(t, session.LiveRunning, rec.Liveness)
}

func TestPollSettlementBookkeepingIsOrderedWithItsWrite(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "ordered-settlement", session.NewFakeBackend(), true, session.Ready)
	lock := manager.startLockForRepo(repoID)

	probed := false
	heldAtRecord := false
	prev := testHookPollBeforeSettlementRecord
	t.Cleanup(func() { testHookPollBeforeSettlementRecord = prev })
	testHookPollBeforeSettlementRecord = func() {
		probed = true
		if lock.TryLock() {
			lock.Unlock()
			return
		}
		heldAtRecord = true
	}

	manager.persistPollChange(repoID, inst, session.LiveRunning, time.Time{}, false)
	require.True(t, probed, "poll never reached settlement bookkeeping")
	require.True(t, heldAtRecord,
		"settlement bookkeeping escaped the write ordering lock; an older success can erase a newer failed-write retry")
}
