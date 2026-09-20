package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMarkHealedAccountSwapPanesStartedProvesTheReplacementBoundary pins the
// half of the heal settlement the scheduler reads (Codex on #4400, round 8): a
// committed swap whose replacement a create brought up must carry the same
// pane-start proof a respawn records, or ValidateAccountSwapReplacementPanes
// reads a healthy root as an incomplete boundary and the repair path stops and
// relaunches it.
func TestMarkHealedAccountSwapPanesStartedProvesTheReplacementBoundary(t *testing.T) {
	inst := &Instance{
		Account:  "personal",
		liveness: LiveRunning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal",
		},
	}
	require.Error(t, inst.ValidateAccountSwapReplacementPanes(),
		"fixture: the carried transaction has no pane proof yet")

	require.NoError(t, inst.MarkHealedAccountSwapPanesStarted())
	require.NoError(t, inst.ValidateAccountSwapReplacementPanes())
	require.True(t, inst.ToInstanceData().PendingAccountSwap.ReplacementPanesStarted,
		"the proof must be durable, not process-local")

	// A swap the replacement does not belong to is refused rather than stamped.
	other := &Instance{
		Account:            "personal",
		pendingAccountSwap: &AccountSwapData{Manual: true, From: "work", To: "someone-else"},
	}
	require.Error(t, other.MarkHealedAccountSwapPanesStarted())
}

// TestRefreshPendingManualAccountSwapMissionFollowsTheCarryOutcome pins the
// other half: a manual swap embeds its conversation outcome in the mission
// text, so a heal that demoted the carry must re-render it. Otherwise the fresh
// agent is told the history is still available.
func TestRefreshPendingManualAccountSwapMissionFollowsTheCarryOutcome(t *testing.T) {
	const carriedWording = "carried over"
	inst := &Instance{
		Title:    "root",
		Account:  "personal",
		Program:  "claude",
		liveness: LiveRunning,
		Tabs:     []*Tab{{Kind: TabKindAgent}},
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal",
			CarriedConversationID: "carried-conv",
			Mission:               "stale mission text that says the history was " + carriedWording,
		},
	}

	// While the carry stands, the outcome the brief renders from says so.
	require.True(t, inst.PendingAccountSwapConversation().Carried)

	// The heal demoted it: the transaction now records a stated fresh start.
	inst.pendingAccountSwap.CarriedConversationID = ""
	inst.pendingAccountSwap.CarryFallback = "the root agent's tmux vanished"
	conversation := inst.PendingAccountSwapConversation()
	require.False(t, conversation.Carried)
	require.Equal(t, "the root agent's tmux vanished", conversation.CarryFailure)

	inst.RefreshPendingManualAccountSwapMission()

	_, mission := inst.PendingManualAccountSwap()
	require.NotContains(t, mission, carriedWording,
		"the re-rendered mission must not keep promising history this replacement does not have")
	require.NotEmpty(t, mission, "the replacement still needs a brief")

	// An automatic swap states its outcome at delivery, so this is a no-op.
	auto := &Instance{
		Account:            "personal",
		Tabs:               []*Tab{{Kind: TabKindAgent}},
		pendingAccountSwap: &AccountSwapData{From: "work", To: "personal", Mission: "untouched"},
	}
	auto.RefreshPendingManualAccountSwapMission()
	require.Equal(t, "untouched", auto.pendingAccountSwap.Mission)
}
