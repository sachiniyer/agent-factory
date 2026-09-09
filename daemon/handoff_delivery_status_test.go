package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestHandoffSession_NonDeliveryVerdictKeepsPendingMission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptCouldNotConfirm,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "unconfirmed-mission", backend)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: inst.Title, RepoID: repoID, To: tmux.ProgramGemini,
	})
	require.ErrorIs(t, err, task.ErrPromptDelivery)
	require.NotEmpty(t, inst.PendingHandoffMission())
	require.NotEmpty(t, recordFor(t, repoID, inst.Title).PendingHandoffMission)
}

func TestResumePendingHandoffs_NonDeliveryVerdictKeepsPendingMission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptCouldNotConfirm,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "unconfirmed-recovery", backend)
	inst.SetStatusForTest(session.Ready)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)

	manager.ResumePendingHandoffs()

	require.Equal(t, mission, inst.PendingHandoffMission())
}
