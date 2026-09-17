package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// stageHandoffWedge reproduces the persisted state issue #4429 found inert: an
// ambiguous mission verdict under the replacement fence with the startup-state
// flag set — OpReplacing, startup_state_unknown, a pending mission carrying
// sent-unverified. MarkStartupStateUnknown clears the op, so the fence goes up
// LAST, exactly as a reload inside the wedge would have left it.
func stageHandoffWedge(t *testing.T, inst *session.Instance, mission string, status session.PromptDeliveryStatus) {
	t.Helper()
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, status))
	inst.MarkStartupStateUnknown()
	require.NoError(t, inst.Transition(session.BeginHandoff()))
}

// #4429: the wedge's supported exit. The operator inspected the pane, saw the
// incoming agent already acting on its mission, and confirms — the daemon
// retires the obligation, settles the replacement fence, and lifts the
// startup-unknown flag WITHOUT resending. Before this verb the same state
// required hand-editing the store and killing the socket owner.
func TestConfirmHandoffDelivery_RetiresAmbiguousWedgeWithoutResend(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptSentUnverified,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-handoff", backend)
	mission := "continue the inherited work"
	stageHandoffWedge(t, inst, mission, session.PromptSentUnverified)
	manager.persistInstance(repoID, inst)

	require.True(t, inst.CanConfirmPendingHandoffDelivery(),
		"the wedge must advertise its supported exit")

	performed, err := manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.NoError(t, err)
	require.True(t, performed)

	require.Empty(t, inst.PendingHandoffMission(), "confirmation retires the durable obligation")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the replacement fence settles")
	require.False(t, inst.StartupStateUnknown(), "the probe-backed attestation resolves the flag")

	_, prompts := backend.snapshot()
	require.Empty(t, prompts,
		"confirmation must never resend — the mission may already be executing")

	rec := recordFor(t, repoID, inst.Title)
	require.NotNil(t, rec)
	require.Empty(t, rec.PendingHandoffMission,
		"the retired obligation must be durable, or a restart would resurrect the wedge")
}

// The other half of the wedge exit: the operator inspected the pane, saw the
// mission did NOT land, and explicitly resends it. MarkStartupStateUnknown
// lowered `started`, and handoffBackend — like LocalBackend — neither captures
// nor sends on such a row, so the retry only works because the daemon probes
// the pane first and restores the binding before readiness runs.
func TestResumeFromLimit_ExplicitRetryClearsStartupUnknownWedge(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptDelivered,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-retry", backend)
	mission := "continue the inherited work"
	stageHandoffWedge(t, inst, mission, session.PromptSentUnverified)

	require.True(t, inst.CanRetryPendingHandoffMissionDelivery(),
		"explicit retry is the other half of the wedge exit")

	outcome, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.NoError(t, err)
	require.Equal(t, resumePerformed, outcome)
	require.Empty(t, inst.PendingHandoffMission())
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.False(t, inst.StartupStateUnknown(),
		"the probe that admitted the retry resolves the flag")
	require.True(t, inst.Started(), "and restores the binding the flag lowered")

	_, prompts := backend.snapshot()
	require.Equal(t, []string{mission}, prompts, "exactly one resend of the exact mission")
}

// absentPaneHandoffBackend answers the liveness probe with a definite "no such
// pane" — the local backend's answer once the tmux session is gone.
type absentPaneHandoffBackend struct {
	*handoffBackend
}

func (b *absentPaneHandoffBackend) IsAlive(*session.Instance) (bool, error) { return false, nil }

// A startup-unknown row whose pane does not answer the probe must refuse the
// retry up front — not spend the readiness timeout under the session locks and
// then fail on the lowered `started` bit — and must leave the row exactly as
// it was, so restore or kill can still own it.
func TestResumeFromLimit_ExplicitRetryRefusesStartupUnknownWithoutLivePane(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptDelivered,
	}
	backend := &absentPaneHandoffBackend{handoffBackend: base}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-absent", backend)
	mission := "continue the inherited work"
	stageHandoffWedge(t, inst, mission, session.PromptSentUnverified)
	require.True(t, inst.CanRetryPendingHandoffMissionDelivery(), "fixture: the row advertises Retry")

	outcome, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.ErrorContains(t, err, "could not be confirmed live")
	require.Equal(t, resumeNotPerformed, outcome)

	_, previews, _ := base.eventSnapshot()
	require.Zero(t, previews, "a pane that failed the probe must not be polled for readiness")
	_, prompts := base.snapshot()
	require.Empty(t, prompts)
	require.True(t, inst.StartupStateUnknown(), "a refused retry keeps the unknown marker")
	require.False(t, inst.Started(), "a refused retry must not restore a binding nothing proved")
	require.Equal(t, mission, inst.PendingHandoffMission())
	require.Equal(t, session.PromptSentUnverified, inst.PendingHandoffDeliveryStatus())
}

// The automatic settle of a delivered crash window must not claim a runtime.
// CommitHandoff always lands on Running, so using it on a reloaded Lost or Dead
// row saved and published that row as a live agent; the fence has to drop
// through an edge that keeps liveness.
func TestResumePendingHandoffs_DeliveredSettleKeepsDeadLiveness(t *testing.T) {
	for name, lv := range map[string]session.Liveness{"lost": session.LiveLost, "dead": session.LiveDead} {
		t.Run(name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := &handoffBackend{
				FakeBackend:    session.NewFakeBackend(),
				deliveryStatus: session.PromptDelivered,
			}
			inst := registerHandoffSubject(t, manager, repoID, repoPath, "delivered-dead", backend)
			mission := "continue the inherited work"
			inst.SetPendingHandoffMission(mission)
			require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptDelivered))
			require.NoError(t, inst.Transition(session.BeginHandoff()))
			require.NoError(t, inst.Transition(session.ObserveLiveness(lv)))
			require.Equal(t, session.OpReplacing, inst.GetInFlightOp(), "fixture: the reloaded fence is up")

			manager.ResumePendingHandoffs()

			require.Empty(t, inst.PendingHandoffMission(), "the delivered obligation still retires")
			require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the fence still drops")
			require.Equal(t, lv, inst.GetLiveness(), "settling a dead row must not claim a live runtime")
			_, prompts := backend.snapshot()
			require.Empty(t, prompts)

			rec := recordFor(t, repoID, inst.Title)
			require.NotNil(t, rec)
			require.Empty(t, rec.PendingHandoffMission)
			require.NotEqual(t, session.LiveRunning, rec.Liveness,
				"the saved record must not describe a dead row as Running")
		})
	}
}

// The could-not-confirm attempt marker is written only once readiness has
// proved the incoming runtime (#4429). While the readiness wait is still
// running, the record on disk carries the verdict that admitted the attempt, so
// a crash there reloads positive non-delivery — and with it the fence that
// hands the mission to automatic recovery — rather than an ambiguous verdict
// for a mission that was never sent.
func TestHandoffSession_AttemptMarkerWaitsForReadiness(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &blockingReadinessHandoffBackend{
		handoffBackend: base,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "marker-order", backend)
	done := make(chan error, 1)
	go func() {
		_, err := manager.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repoID, To: tmux.ProgramGemini,
		})
		done <- err
	}()

	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handoff never reached incoming-agent readiness")
	}
	rec := recordFor(t, repoID, inst.Title)
	close(backend.release)
	require.NoError(t, <-done)

	require.NotNil(t, rec)
	require.NotEmpty(t, rec.PendingHandoffMission, "fixture: the swap checkpoint carries the mission")
	require.Equal(t, session.PromptNotDelivered, rec.HandoffDeliveryStatus,
		"no attempt marker may be durable before readiness has proved the runtime")
	crashed := *rec
	crashed.BackendType = "docker"
	reloaded, err := session.FromInstanceData(crashed)
	require.NoError(t, err)
	require.Equal(t, session.OpReplacing, reloaded.GetInFlightOp(),
		"a crash inside the readiness wait reloads the fence automatic recovery owns")
	require.True(t, reloaded.PendingHandoffMissionAutoRetryable())
}

// A failed attempt-marker write sends nothing and must put back the verdict
// that admitted the attempt. An explicit retry is admitted by an AMBIGUOUS
// verdict; restoring positive non-delivery instead would hand automatic
// recovery a mission that may already have landed.
func TestResumeFromLimit_MarkerWriteFailureKeepsTheAmbiguousVerdict(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptDelivered,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "marker-write-fails", backend)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptSentUnverified))
	manager.persistInstance(repoID, inst)

	diskFull := errors.New("no space left on device")
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == inst.Title && data.HandoffDeliveryStatus == session.PromptCouldNotConfirm {
			return diskFull
		}
		return nil
	}

	outcome, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.ErrorIs(t, err, diskFull)
	require.Equal(t, resumeNotPerformed, outcome)
	_, prompts := backend.snapshot()
	require.Empty(t, prompts, "a marker that is not durable must not be followed by a send")
	require.Equal(t, session.PromptSentUnverified, inst.PendingHandoffDeliveryStatus(),
		"the admitting verdict comes back, not positive non-delivery")
	require.False(t, inst.PendingHandoffMissionAutoRetryable(),
		"automatic recovery must never own a mission that may already have landed")
}

// Automatic recovery must NOT touch the wedge: an ambiguous verdict may mean
// the mission already landed, so a poll-driven resend could double-deliver it.
// Only the operator's explicit verbs admit this row.
func TestResumePendingHandoffs_LeavesAmbiguousWedgeForTheOperator(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptDelivered,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-auto", backend)
	mission := "continue the inherited work"
	stageHandoffWedge(t, inst, mission, session.PromptSentUnverified)

	manager.ResumePendingHandoffs()
	manager.ResumePendingHandoffs()

	require.Equal(t, mission, inst.PendingHandoffMission(),
		"automatic recovery leaves an ambiguous mission for the operator")
	_, prompts := backend.snapshot()
	require.Empty(t, prompts, "automatic recovery must not resend an ambiguous mission")
}

// The crash window between a confirmed delivery and its bookkeeping settle: a
// delivered verdict is already proof the mission landed, so recovery retires
// the obligation and the fence itself — no operator attestation needed and no
// resend.
func TestResumePendingHandoffs_SettlesDeliveredCrashWindow(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptDelivered,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "delivered-crash", backend)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptDelivered))
	require.NoError(t, inst.Transition(session.BeginHandoff()))
	manager.persistInstance(repoID, inst)

	manager.ResumePendingHandoffs()

	require.Empty(t, inst.PendingHandoffMission(), "the delivered obligation retires")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the reconstructed fence settles")
	_, prompts := backend.snapshot()
	require.Empty(t, prompts, "a delivered verdict is proof; recovery must not resend")

	rec := recordFor(t, repoID, inst.Title)
	require.NotNil(t, rec)
	require.Empty(t, rec.PendingHandoffMission)
}

// Confirmation refuses rows whose runtime is gone: attest or not, there is no
// pane to have landed in — restore owns the dead, kill owns the tombstoned.
func TestConfirmHandoffDelivery_RefusesLostAndKilled(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptSentUnverified,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-lost", backend)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptSentUnverified))
	inst.MarkStartupStateUnknown()
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))

	require.False(t, inst.CanConfirmPendingHandoffDelivery(),
		"a lost runtime has no pane the mission could have landed in")
	performed, err := manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.Error(t, err)
	require.False(t, performed)
	require.Equal(t, mission, inst.PendingHandoffMission(),
		"a refused confirmation leaves the obligation for restore")

	// A pending kill outranks any attestation.
	inst2 := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-killed", backend)
	stageHandoffWedge(t, inst2, mission, session.PromptSentUnverified)
	inst2.MarkUserKilled()

	performed, err = manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst2.ID, Title: inst2.Title, RepoID: repoID,
	})
	require.Error(t, err)
	require.False(t, performed)
	require.Equal(t, mission, inst2.PendingHandoffMission())
}

// An unambiguous non-delivery is not the operator's call: automatic recovery
// owns a mission proven absent, and "mark delivered" would retire a mission
// that never ran.
func TestConfirmHandoffDelivery_RefusesPositiveNonDelivery(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptNotDelivered,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-notdelivered", backend)
	mission := "continue the inherited work"
	stageHandoffWedge(t, inst, mission, session.PromptNotDelivered)

	require.False(t, inst.CanConfirmPendingHandoffDelivery())
	performed, err := manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.Error(t, err)
	require.False(t, performed)
	require.Equal(t, mission, inst.PendingHandoffMission(),
		"not-delivered evidence keeps the mission for automatic recovery")
}

// The ambiguous verdict with a settled fence — the state the send path itself
// now leaves — confirms through the same verb, proving the exit does not
// depend on the wedge's exact flag combination.
func TestConfirmHandoffDelivery_RetiresSettledAmbiguousMission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptSentUnverified,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "settled-ambiguous", backend)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptSentUnverified))
	manager.persistInstance(repoID, inst)

	performed, err := manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.NoError(t, err)
	require.True(t, performed)
	require.Empty(t, inst.PendingHandoffMission())
	_, prompts := backend.snapshot()
	require.Empty(t, prompts)
}

// The durable shape after an ambiguous delivery reloads WITHOUT the fence
// (#4429): pendingHandoffMissionNeedsFence scopes reconstruction to the
// verdicts the daemon itself still owns. The ambiguous row must load usable —
// rebuilding OpReplacing there manufactures the wedge this issue reports.
func TestReloadedAmbiguousMissionLoadsWithoutTheFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptSentUnverified,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "ambiguous-reload", backend)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptSentUnverified))
	manager.persistInstance(repoID, inst)

	rec := recordFor(t, repoID, inst.Title)
	require.NotNil(t, rec)
	require.Equal(t, session.PromptSentUnverified, rec.HandoffDeliveryStatus)

	// A sandbox record skips the local git-worktree reconstruction the fixture
	// cannot satisfy — and its container is genuinely gone after a restart, so
	// it loads inert-Lost and restore owns it. The exits on a live row are
	// exercised by the in-memory tests above; what this leg pins is that the
	// reload does not rebuild the wedge and does not strand the mission.
	rec.BackendType = "docker"
	reloaded, err := session.FromInstanceData(*rec)
	require.NoError(t, err)
	require.Equal(t, session.OpNone, reloaded.GetInFlightOp(),
		"an ambiguous verdict loads without the fence")
	require.Equal(t, mission, reloaded.PendingHandoffMission(),
		"the obligation stays durable for whoever owns the row's runtime")
	require.Equal(t, session.LiveLost, reloaded.GetLiveness())
	require.False(t, reloaded.CanConfirmPendingHandoffDelivery() ||
		reloaded.CanRetryPendingHandoffMissionDelivery(),
		"a runtime that no longer exists refuses both exits — restore owns this row")

	// And not-delivered keeps its fence: the automatic resend owns that row.
	rec.HandoffDeliveryStatus = session.PromptNotDelivered
	reloaded2, err := session.FromInstanceData(*rec)
	require.NoError(t, err)
	require.Equal(t, session.OpReplacing, reloaded2.GetInFlightOp(),
		"positive non-delivery still reconstructs the fence for automatic recovery")
}
