package daemon

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
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
	require.Error(t, walked.CodexBaselineErr(), "fixture: a store walk now fails")

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

// persistedAgentConversation is the record the row's last write put on disk —
// what a restarted daemon reads back.
func persistedAgentConversation(t *testing.T, repoID string) session.AgentConversationData {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var stored []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Len(t, stored, 1)
	require.NotNil(t, stored[0].AgentConversation)
	return *stored[0].AgentConversation
}

// TestOperatorRepairStampIsPersistedWithTheRecord is #7122's property 2 at
// the daemon: what the runtime held when the operator's repair was made is
// written to disk with the repair itself, so a restarted daemon still knows
// which thread the pane was on and follows a move made while it was down. A
// re-run renews the repair as of now and re-stamps it.
func TestOperatorRepairStampIsPersistedWithTheRecord(t *testing.T) {
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	fresh := writeHeldTestRollout(t, codexHome, heldFreshThreadID, dir)
	writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held := &heldOnly{}
	held.set(fresh)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, heldUserThreadID), nil))
	stored := persistedAgentConversation(t, repoID)
	require.Equal(t, heldUserThreadID, stored.ID)
	require.Equal(t, session.ConversationCaptureOperator, stored.CaptureKind)
	require.True(t, stored.RepairStamped, "the observation must reach disk with the repair")
	require.Equal(t, heldFreshThreadID, stored.RepairHeldID)

	next := writeHeldTestRollout(t, codexHome, heldNextThreadID, dir)
	held.set(next)
	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, heldUserThreadID), nil))
	renewed := persistedAgentConversation(t, repoID)
	require.Equal(t, heldUserThreadID, renewed.ID)
	require.Equal(t, heldNextThreadID, renewed.RepairHeldID, "a re-run re-stamps what the runtime holds now")
	require.False(t, renewed.CapturedAt.Before(stored.CapturedAt))
}

// TestUnstampedOperatorRepairBaselineIsPersisted: a repair that reached disk
// with no observation (the held query could not answer, or the record
// predates the stamp) takes the follow's first answer as its baseline, and
// that baseline is persisted like any record write, so a restart does not
// take a later move as a fresh baseline (#7122).
func TestUnstampedOperatorRepairBaselineIsPersisted(t *testing.T) {
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	fresh := writeHeldTestRollout(t, codexHome, heldFreshThreadID, dir)
	writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held := &heldOnly{}
	held.set(fresh)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldUserThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureOperator,
	})

	require.NoError(t, manager.followAgentConversation(repoID, key, inst,
		inst.SessionCodexCaptureScope(), inst.AgentRuntimeToken(), inst.AgentConversation()))
	stored := persistedAgentConversation(t, repoID)
	require.Equal(t, heldUserThreadID, stored.ID, "the baseline moves nothing")
	require.True(t, stored.RepairStamped)
	require.Equal(t, heldFreshThreadID, stored.RepairHeldID)
}

// TestOperatorRepairIsDatedNoLaterThanItsObservation is the #7125 review's
// R-2: the repair's stamp says what the runtime held when set-conversation
// looked, before it took the op-lock and walked the store, so the repair's
// date — the instant the staleness bound disregards threads up to — must be
// no later than that look. Otherwise a thread the pane started in between is
// one the bound treats as overridden while the follow treats it as a move.
func TestOperatorRepairIsDatedNoLaterThanItsObservation(t *testing.T) {
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	fresh := writeHeldTestRollout(t, codexHome, heldFreshThreadID, dir)
	writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held := &heldOnly{}
	held.set(fresh)
	var mu sync.Mutex
	var firstLook time.Time
	inst.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		mu.Lock()
		if firstLook.IsZero() {
			firstLook = time.Now()
		}
		mu.Unlock()
		return held.snapshot()
	})

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, heldUserThreadID), nil))
	stored := persistedAgentConversation(t, repoID)
	require.True(t, stored.RepairStamped)
	mu.Lock()
	look := firstLook
	mu.Unlock()
	require.False(t, look.IsZero(), "fixture: the observation must have read the runtime's held files")
	require.False(t, stored.CapturedAt.After(look),
		"the repair is dated %s, after the observation it was stamped with (%s)", stored.CapturedAt, look)
}
