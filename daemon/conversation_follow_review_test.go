package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7120 review suite: the follow check must leave an operator's repair
// alone until the runtime itself moves, and following an already-recorded row
// must never depend on — or pay for — a walk of the conversation store.

const (
	heldFreshThreadID = "01a1ccd0-1111-7222-8333-444455556666"
	heldNextThreadID  = "01a1cce0-1111-7222-8333-444455556666"
)

// TestOperatorRepairStandsUntilTheRuntimeMoves is the review's P1, in #7101's
// own aftermath: a lost-restore started the pane fresh and the user sent a
// message, so the runtime holds a fresh thread F. The operator repairs the
// record to the real thread R with set-conversation so the next restore or
// handoff returns to it. The follow must leave R standing for as long as the
// runtime still holds what it held at the repair, and replace it only with a
// thread the runtime moves to afterwards.
func TestOperatorRepairStandsUntilTheRuntimeMoves(t *testing.T) {
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	fresh := writeHeldTestRollout(t, codexHome, heldFreshThreadID, dir)
	writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held := &heldOnly{}
	held.set(fresh)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldFreshThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	})

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, heldUserThreadID), nil))
	require.Equal(t, heldUserThreadID, inst.AgentConversation().ID)

	snap := inst.SessionCodexCaptureBaseline()
	token := inst.AgentRuntimeToken()
	for check := 0; check < 3; check++ {
		require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
		require.Equal(t, heldUserThreadID, inst.AgentConversation().ID,
			"check %d: the operator's repair must stand while the runtime still holds what it held at the repair", check)
	}

	// The pane moves to a thread started after the repair: that one wins.
	next := writeHeldTestRollout(t, codexHome, heldNextThreadID, dir)
	held.set(next)
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	require.Equal(t, heldNextThreadID, inst.AgentConversation().ID,
		"a thread the runtime moved to after the repair replaces it")
	require.Equal(t, heldNextThreadID, persistedConversationID(t, repoID))
}

// TestRecordedRowFollowLoopNeedsNoStoreWalk is the review's F3: a recorded
// row's follow check reads the runtime's descriptors, never the store, so a
// store the baseline walk cannot traverse must neither end its loop after
// one attempt nor make the poll re-arm it — re-walking the store — on every
// tick.
func TestRecordedRowFollowLoopNeedsNoStoreWalk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	manager, inst, _, _, codexHome := codexLoopTestInstance(t)
	writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	})
	// A day directory inside the scan window that the walk cannot enter.
	blocked := configAgentCodexDayDir(codexHome, time.Now().AddDate(0, 0, -1))
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.Chmod(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	walked := inst.SessionCodexCaptureBaseline()
	require.True(t, walked.CodexBaselineFailed(), "fixture: a store walk now fails")

	manager.ArmConversationCaptures()
	token, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "a recorded row must gain its follow loop")

	time.Sleep(10 * conversationCaptureRearmInterval)
	still, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "a baseline the follow never uses must not end its loop")
	require.Equal(t, token, still)
}

// TestFollowFreezeIsReportedOnceWhenItLingers is the review's F7: a freeze is
// silent while a swap or handoff settles, but one that outlasts a few checks —
// an obligation nobody is clearing — is reported, once.
func TestFollowFreezeIsReportedOnceWhenItLingers(t *testing.T) {
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())
	recorded := session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	}
	inst.SetAgentConversation(recorded)
	snap := inst.SessionCodexCaptureScope()
	token := inst.AgentRuntimeToken()
	inst.SetInFlightOpForTest(session.OpRespawning)

	reported := 0
	for check := 0; check < 6; check++ {
		if err := manager.followAgentConversation(repoID, key, inst, snap, token, recorded); err != nil {
			reported++
			require.ErrorContains(t, err, "not following recorded conversation")
		}
	}
	require.Equal(t, 1, reported, "a lingering freeze is reported exactly once")

	inst.SetInFlightOpForTest(session.OpNone)
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, recorded))
}

// TestRepairToTheAlreadyRecordedIDBecomesAnOperatorRepair: re-recording the id
// a capture already holds is the operator saying "this one, not the thread the
// pane is on now". It must become a standing repair — not a no-op that the
// next follow check overwrites (#7120 Codex review).
func TestRepairToTheAlreadyRecordedIDBecomesAnOperatorRepair(t *testing.T) {
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	fresh := writeHeldTestRollout(t, codexHome, heldFreshThreadID, dir)
	held := &heldOnly{}
	held.set(fresh)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	// A capture recorded R; the pane has since started F, which the follow
	// has not yet seen.
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldUserThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	})

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, heldUserThreadID), nil))
	require.Equal(t, session.ConversationCaptureOperator, inst.AgentConversation().CaptureKind,
		"repairing to the recorded id makes it the operator's")

	require.NoError(t, manager.followAgentConversation(repoID, key, inst,
		inst.SessionCodexCaptureScope(), inst.AgentRuntimeToken(), inst.AgentConversation()))
	require.Equal(t, heldUserThreadID, inst.AgentConversation().ID,
		"the repair stands while the runtime holds what it held when the repair was made")
}
