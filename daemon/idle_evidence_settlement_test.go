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
