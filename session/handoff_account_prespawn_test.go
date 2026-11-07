package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// preSpawnCommittedSwapInstance builds the state the daemon's pre-spawn
// re-admission presents to ParkCommittedAccountSwapAtLimit: the resume fence
// held (OpRespawning), a committed pending replacement, and the incoming
// identity already on i.Account — while the tmux binding still runs the
// OUTGOING agent. That mirrors a real cross-agent handoff's pre-spawn
// window (#6915 review): rewriting i.Program at selection does not move the
// pane's program, so currentAgentNameLocked still answers claude while the
// committed swap's namespace is codex.
func preSpawnCommittedSwapInstance(account string, manual bool) *Instance {
	inst := &Instance{
		// Program holds the incoming launch command the selection rewrite
		// installed; the pane — the ground truth currentAgentNameLocked
		// reads — still runs the outgoing agent.
		Program:    tmux.ProgramCodex,
		Account:    account,
		liveness:   LiveRunning,
		inFlightOp: OpRespawning,
		pendingAccountSwap: &AccountSwapData{
			Manual: manual, From: "personal", To: account,
			AccountAgent:          tmux.ProgramCodex,
			MissionDeliveryStatus: PromptCouldNotConfirm,
		},
	}
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	return inst
}

// TestParkCommittedAccountSwapAtLimit_FilesWallUnderCommittedNamespace is the
// #6915-review core: a pre-spawn park must attribute the incoming account's
// wall to the committed swap's namespace, not to the agent the pane still
// runs. The pane-derived attribution the Park*AtLimit pair uses would file
// this (codex, "work") wall as (claude, "work") — excluding an unrelated
// claude account and defeating incomingManualReset's namespace match.
func TestParkCommittedAccountSwapAtLimit_FilesWallUnderCommittedNamespace(t *testing.T) {
	reset := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	inst := preSpawnCommittedSwapInstance("work", true)

	// The fixture's pane still runs the outgoing agent: pane-derived
	// attribution would name claude. The committed namespace is codex.
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramCodex, reset, "", time.Now().UTC()))

	require.Equal(t, LiveLimitReached, inst.GetLiveness())
	agent, account, ok := inst.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramCodex, agent,
		"the wall must file under the committed namespace, not the pane's outgoing agent")
	require.Equal(t, "work", account)

	observations := inst.AccountLimitObservations()
	requireAccountLimitObservation(t, observations, tmux.ProgramCodex, "work", reset)

	// The manual path's not-delivered marker flips the same way the
	// post-spawn twin's does.
	require.Equal(t, PromptNotDelivered, inst.pendingAccountSwap.MissionDeliveryStatus)

	data := inst.ToInstanceData()
	require.Equal(t, tmux.ProgramCodex, data.LimitAgent)
	require.Equal(t, "work", data.LimitAccount)
	requireAccountLimitObservation(t, data.AccountLimitObservations, tmux.ProgramCodex, "work", reset)
}

// TestParkCommittedAccountSwapAtLimit_RefusesWithoutFence guards the entry
// point: like the Park*AtLimit pair, it requires the resume fence and a
// committed pending replacement — each axis exercised independently.
func TestParkCommittedAccountSwapAtLimit_RefusesWithoutFence(t *testing.T) {
	reset := time.Now().Add(time.Hour)

	t.Run("resume fence not held", func(t *testing.T) {
		inst := preSpawnCommittedSwapInstance("work", false)
		inst.inFlightOp = OpNone
		require.Error(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramCodex, reset, "", time.Now().UTC()))
	})

	t.Run("no pending replacement", func(t *testing.T) {
		inst := preSpawnCommittedSwapInstance("work", false)
		inst.pendingAccountSwap = nil
		require.Error(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramCodex, reset, "", time.Now().UTC()))
	})
}

// TestParkCommittedAccountSwapAtLimit_AutomaticKeepsDeliveryMarker pins the
// same manual-only-marker contract the automatic twin documents: an automatic
// pending swap parked pre-spawn must not flip MissionDeliveryStatus.
func TestParkCommittedAccountSwapAtLimit_AutomaticKeepsDeliveryMarker(t *testing.T) {
	inst := preSpawnCommittedSwapInstance("work", false)
	require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus)

	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, time.Now().Add(time.Hour), "", time.Now().UTC()))

	require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus,
		"an automatic swap parked pre-spawn must not set the manual-only delivery marker")
}
