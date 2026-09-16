package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestHandoffSession_AmbiguousVerdictKeepsPendingMission(t *testing.T) {
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
	handoff, ok := inst.LastHandoff()
	require.True(t, ok)
	require.Equal(t, handoff.HeadSHA, resp.HeadSHA,
		"the response must carry the recorded boundary; an unborn or unbound fixture may have no HEAD")
	require.NotEmpty(t, inst.PendingHandoffMission(),
		"an ambiguous delivery must retain the in-memory mission obligation")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"readiness proved the incoming runtime live, so the replacement fence settles (#4429): "+
			"fencing a possibly-running agent freezes its status forever and hides every lifecycle action")
	require.True(t, inst.CanRetryPendingHandoffMissionDelivery(),
		"the ambiguous verdict on a known-live pane must admit explicit retry after inspection")
	rec := recordFor(t, repoID, inst.Title)
	require.NotEmpty(t, rec.PendingHandoffMission,
		"an ambiguous delivery must retain the durable mission obligation")
	require.Equal(t, session.PromptCouldNotConfirm, rec.HandoffDeliveryStatus)
}

// TestHandoffSession_AmbiguousVerdictSettlesFenceKeepsMission is the #4429
// regression: an ambiguous mission verdict must not leave the replacement fence
// raised. Readiness already proved the incoming runtime live, so fencing the
// row freezes a possibly-working agent at its checkpoint state, skips every
// status poll, and hides every lifecycle action — the reported wedge. The
// fence settles while the pending mission remains the durable obligation for
// explicit retry; automatic replay still requires positive non-delivery.
func TestHandoffSession_AmbiguousVerdictSettlesFenceKeepsMission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptSentUnverified,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "ambiguous-settle", backend)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: inst.Title, RepoID: repoID, To: tmux.ProgramGemini,
	})
	require.ErrorIs(t, err, task.ErrPromptDelivery,
		"the handoff still reports its mission delivery as unconfirmed — settling the fence must not read as success")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the replacement fence must settle once readiness proved the incoming runtime live")
	require.NotEmpty(t, inst.PendingHandoffMission(),
		"the exact mission stays pending for explicit retry after inspection")
	require.False(t, inst.PendingHandoffMissionAutoRetryable(),
		"an ambiguous verdict must never authorize automatic replay")
	require.True(t, inst.CanRetryPendingHandoffMissionDelivery(),
		"the ambiguous verdict on a known-live pane must admit explicit retry")

	// With the fence down the status poll observes the row normally instead of
	// freezing it at its checkpoint state.
	manager.refreshInstanceStatus(repoID, inst)
	_, _, statusPolls := backend.eventSnapshot()
	require.Positive(t, statusPolls,
		"an ambiguous verdict must not fence the row out of the status poll")

	rec := recordFor(t, repoID, inst.Title)
	require.NotEmpty(t, rec.PendingHandoffMission)
	require.Equal(t, session.PromptSentUnverified, rec.HandoffDeliveryStatus)
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

func TestResumePendingHandoffs_AmbiguousVerdictKeepsPendingMission(t *testing.T) {
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
