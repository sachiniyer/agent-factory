package session

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// requireCarryInstance is a limit-blocked Codex session on the registered
// "old" account whose recorded thread is filed there, swapping into "work".
func requireCarryInstance(t *testing.T) (*Instance, string) {
	t.Helper()
	carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
	return inst, target
}

// Under require_carry, the move's own plan-time fallback is a refusal: the
// swap is refused before anything is recorded or stopped, and never becomes a
// stated fresh start (#7282). Probe P4: the engine's check against "work"
// read a snapshot, and the target has since filed this thread under another
// day dir — the move's own plan sees it and falls back.
func TestValidateAccountSwapRequiringCarryRefusesAPlanTimeFallback(t *testing.T) {
	inst, target := requireCarryInstance(t)
	writeCarryFile(t, target, codexRolloutRelDay(carryTestID, time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)),
		codexRollout(inst.GetWorktreePath()))

	err := inst.ValidateAccountSwapRequiringCarry("work")
	var required *CarryRequiredError
	require.ErrorAs(t, err, &required, "a plan that falls back must be refused under require_carry")
	require.Equal(t, carryTargetHoldsOtherRolloutReason, required.Reason)
	require.Equal(t, handoff.CarryDuplicated, required.Code())
	require.Nil(t, inst.accountSwapLaunch, "a refused swap records no launch plan")

	// Without require_carry, the same state keeps today's stated fresh start.
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.Nil(t, inst.accountSwapLaunch.carry)
	require.Equal(t, carryTargetHoldsOtherRolloutReason, inst.accountSwapLaunch.carryFallback)
}

// A swap whose plan carries is admitted under require_carry, with the carry
// recorded as ValidateAccountSwap records it.
func TestValidateAccountSwapRequiringCarryAdmitsACarry(t *testing.T) {
	inst, _ := requireCarryInstance(t)
	require.NoError(t, inst.ValidateAccountSwapRequiringCarry("work"))
	require.NotNil(t, inst.accountSwapLaunch)
	require.NotNil(t, inst.accountSwapLaunch.carry)
	require.Empty(t, inst.accountSwapLaunch.carryFallback)
}

// A session af holds no conversation for cannot carry one either.
func TestValidateAccountSwapRequiringCarryRefusesWithoutAConversation(t *testing.T) {
	carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)

	err := inst.ValidateAccountSwapRequiringCarry("work")
	var required *CarryRequiredError
	require.ErrorAs(t, err, &required)
	require.Equal(t, handoff.CarryUnrecorded, required.Code())
	require.Nil(t, inst.accountSwapLaunch)
}

// A committed swap is exempt from require_carry's plan-time refusal: its
// conversation was decided at the identity checkpoint, after the outgoing
// runtime had stopped, so refusing it now would strand a swap already under
// way. Committed means a pending record for this very account — whether it
// recorded a fresh start (CarryFallback) or a carried id whose plan now falls
// back. A pending record for another account commits nothing here, so the
// refusal stands (#7283 review R1).
func TestValidateAccountSwapRequiringCarryExemptsOnlyTheCommittedSwap(t *testing.T) {
	notRequired := func(t *testing.T, err error) {
		t.Helper()
		var required *CarryRequiredError
		require.False(t, errors.As(err, &required), "a committed swap was refused: %v", err)
		require.NoError(t, err)
	}
	fellBack := func(t *testing.T, inst *Instance) {
		t.Helper()
		require.NotNil(t, inst.accountSwapLaunch)
		require.Nil(t, inst.accountSwapLaunch.carry, "fixture: the committed plan must fall back, or the exemption is untested")
		require.NotEmpty(t, inst.accountSwapLaunch.carryFallback)
	}
	t.Run("committed to a fresh start", func(t *testing.T) {
		inst, _ := requireCarryInstance(t)
		inst.pendingAccountSwap = &AccountSwapData{From: "old", To: "work",
			CarryFallback: carryTargetHoldsOtherRolloutReason}
		notRequired(t, inst.ValidateAccountSwapRequiringCarry("work"))
		fellBack(t, inst)
	})
	t.Run("committed to a carry whose plan now falls back", func(t *testing.T) {
		carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
		registerAccount(t, tmux.ProgramCodex, "old")
		inst.Account, inst.accountAutoSelected = "old", true
		recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
		// No rollout of the carried thread in either home: the committed
		// carry's plan falls back to a missing rollout.
		inst.pendingAccountSwap = &AccountSwapData{From: "old", To: "work",
			CarriedConversationID: carryTestID, CarrySourceAccount: "old"}
		notRequired(t, inst.ValidateAccountSwapRequiringCarry("work"))
		fellBack(t, inst)
	})
	t.Run("pending for another account", func(t *testing.T) {
		inst, target := requireCarryInstance(t)
		writeCarryFile(t, target, codexRolloutRelDay(carryTestID, time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)),
			codexRollout(inst.GetWorktreePath()))
		inst.pendingAccountSwap = &AccountSwapData{From: "old", To: "elsewhere",
			CarryFallback: carryTargetHoldsOtherRolloutReason}
		err := inst.ValidateAccountSwapRequiringCarry("work")
		var required *CarryRequiredError
		require.ErrorAs(t, err, &required, "a record for another account commits nothing to this one")
		require.Equal(t, handoff.CarryDuplicated, required.Code())
	})
}

// A plan that carries nothing and names no reason — no carry applies at all —
// is a refusal too, and its code is never empty: an empty code would read as
// "carries" to anything that asks it (#7283 review R1).
func TestRequiredCarryRefusalOfAPlanThatCarriesNothing(t *testing.T) {
	err := requiredCarryRefusal(true, false, nil, "")
	var required *CarryRequiredError
	require.ErrorAs(t, err, &required)
	require.NotEmpty(t, required.Reason)
	require.NotEmpty(t, required.Code())
	require.Equal(t, handoff.CarryUnconfirmed, required.Code())

	require.NoError(t, requiredCarryRefusal(false, false, nil, ""), "without require_carry nothing is refused")
	require.NoError(t, requiredCarryRefusal(true, true, nil, ""), "a committed swap is exempt")
}
