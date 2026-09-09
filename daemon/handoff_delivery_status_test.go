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

	resp, err := manager.HandoffSession(HandoffSessionRequest{
		Title: inst.Title, RepoID: repoID, To: tmux.ProgramGemini,
	})
	require.ErrorIs(t, err, task.ErrPromptDelivery)
	require.True(t, isMutationCommitted(err),
		"the target runtime was already installed before delivery became ambiguous")
	require.Equal(t, tmux.ProgramClaude, resp.From)
	require.Equal(t, tmux.ProgramGemini, resp.To)
	require.NotEmpty(t, resp.HeadSHA)
	require.NotEmpty(t, inst.PendingHandoffMission())
	rec := recordFor(t, repoID, inst.Title)
	require.NotEmpty(t, rec.PendingHandoffMission)
	require.Equal(t, session.PromptCouldNotConfirm, rec.HandoffDeliveryStatus)
}

func TestResumeFromLimit_ExplicitlyRetriesAmbiguousAgentHandoff(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "explicit-agent-retry", backend)
	mission := "continue the inherited work"
	require.NoError(t, inst.Transition(session.BeginHandoff()))
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.BeginPendingHandoffMissionDelivery(mission))
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptCouldNotConfirm))
	manager.persistInstance(repoID, inst)

	outcome, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.NoError(t, err)
	require.Equal(t, resumePerformed, outcome)
	require.Empty(t, inst.PendingHandoffMission())
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	_, prompts := backend.snapshot()
	require.Equal(t, []string{mission}, prompts)
}

func TestResumePendingHandoffs_RetriesObservedNonDelivery(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptNotDelivered,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "observed-nondelivery", backend)
	inst.SetStatusForTest(session.Ready)
	inst.SetPendingHandoffMission("continue the inherited work")

	manager.ResumePendingHandoffs()
	manager.clearPendingHandoffRetry(repoID, inst)
	manager.ResumePendingHandoffs()

	_, prompts := backend.snapshot()
	require.Len(t, prompts, 2,
		"mission-scoped not-delivered evidence must retain automatic recovery")
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

func TestResumePendingHandoffs_DoesNotRetryAmbiguousMission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptCouldNotConfirm,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "ambiguous-recovery", backend)
	inst.SetStatusForTest(session.Ready)
	inst.SetPendingHandoffMission("continue the inherited work")
	manager.persistInstance(repoID, inst)

	manager.ResumePendingHandoffs()
	rec := recordFor(t, repoID, inst.Title)
	require.Equal(t, session.PromptCouldNotConfirm, rec.HandoffDeliveryStatus,
		"the ambiguous verdict must be durable and scoped to the pending mission")
	reloadedHandoffRow(t, manager, repoID, repoPath, rec, backend)
	manager.ResumePendingHandoffs()

	_, prompts := backend.snapshot()
	require.Len(t, prompts, 1,
		"could-not-confirm may have submitted the mission and must suppress automatic redelivery")
}
