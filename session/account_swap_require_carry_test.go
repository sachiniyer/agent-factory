package session

import (
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
