package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// require_carry's input is the live carry's own answer: for every state of the
// outgoing side, AccountSwapCarryRefusal says "carries" exactly when the
// swap's plan carries (#7152), and otherwise names why with a carry refusal
// code (#7156 review P3-2). Its Codex checks read the shared store snapshot,
// and the live plan walks the store, so this is also their agreement.
func TestAccountSwapCarryPossibleAgreesWithTheSwapPlan(t *testing.T) {
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name    string
		agent   string
		setup   func(t *testing.T, inst *Instance, source string)
		carries bool
		code    string
	}{
		{
			name: "a codex rollout of the recorded thread", agent: tmux.ProgramCodex, carries: true,
			setup: func(t *testing.T, inst *Instance, source string) {
				writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
			},
		},
		{
			name: "no rollout of the recorded thread", agent: tmux.ProgramCodex, code: handoff.CarryMissing,
			setup: func(t *testing.T, inst *Instance, source string) {},
		},
		{
			name: "a newer thread in the worktree", agent: tmux.ProgramCodex, code: handoff.CarryStale,
			setup: func(t *testing.T, inst *Instance, source string) {
				recorded := writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
				newer := writeCarryFile(t, source, codexRolloutRelDay(carryOtherThreadID, day), codexRollout(inst.GetWorktreePath()))
				old := time.Now().Add(-2 * time.Hour)
				require.NoError(t, os.Chtimes(recorded, old, old))
				require.NoError(t, os.Chtimes(newer, old.Add(time.Hour), old.Add(time.Hour)))
			},
		},
		{
			name: "two rollouts record the thread", agent: tmux.ProgramCodex, code: handoff.CarryDuplicated,
			setup: func(t *testing.T, inst *Instance, source string) {
				writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
				writeCarryFile(t, source, codexRolloutRelDay(carryTestID, day), codexRollout(inst.GetWorktreePath()))
			},
		},
		{
			name: "the thread is held only compressed", agent: tmux.ProgramCodex, code: handoff.CarryCompressed,
			setup: func(t *testing.T, inst *Instance, source string) {
				writeCarryFile(t, source, codexRolloutRel(carryTestID)+".zst", "\x28\xb5\x2f\xfd")
			},
		},
		{
			name: "the thread was backtracked", agent: tmux.ProgramCodex, code: handoff.CarryBacktracked,
			setup: func(t *testing.T, inst *Instance, source string) {
				writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
				writeCarryFile(t, source, codexRevertRolloutRel(carryTestID, carryRevertRolloutID, day, ""),
					codexRollout(inst.GetWorktreePath()))
			},
		},
		{
			// No revert is minted before the rollout it reverts, so the live
			// revert check never opens that day; the snapshot must not count it.
			name: "a revert rollout filed long before the thread began", agent: tmux.ProgramCodex, carries: true,
			setup: func(t *testing.T, inst *Instance, source string) {
				writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
				writeCarryFile(t, source, codexRevertRolloutRel(carryTestID, carryRevertRolloutID,
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local), ""), codexRollout(inst.GetWorktreePath()))
			},
		},
		{
			name: "a claude transcript of the recorded conversation", agent: tmux.ProgramClaude, carries: true,
			setup: func(t *testing.T, inst *Instance, source string) {
				writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{}\n")
			},
		},
		{
			name: "no claude transcript", agent: tmux.ProgramClaude, code: handoff.CarryMissing,
			setup: func(t *testing.T, inst *Instance, source string) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useCarryAheadStores(t)
			carryAmbientProviderHomes(t)
			inst, _ := carrySwapInstance(t, tc.agent)
			source := registerAccount(t, tc.agent, "old")
			inst.Account, inst.accountAutoSelected = "old", true
			recordOutgoingConversation(t, inst, tc.agent, carryTestID)
			tc.setup(t, inst, source)

			refusal := inst.AccountSwapCarryRefusal()
			require.NoError(t, inst.ValidateAccountSwap("work"))
			plan := inst.accountSwapLaunch
			require.Equal(t, tc.carries, plan.carry != nil, "fixture: the swap plan (fallback %q)", plan.carryFallback)
			require.Equal(t, plan.carry != nil, refusal == "",
				"require_carry's answer must be the live plan's (fallback %q, code %q)", plan.carryFallback, refusal)
			require.Equal(t, tc.code, refusal, "the code must name the live plan's refusal %q", plan.carryFallback)
			require.Equal(t, tc.code, carryRefusalCode(plan.carryFallback))
		})
	}
}

// A session af never recorded a launch command for cannot have its
// conversation located, so require_carry cannot be satisfied.
func TestAccountSwapCarryPossibleNeedsTheLaunchCommand(t *testing.T) {
	inst := &Instance{
		Program: tmux.ProgramCodex,
		Tabs:    []*Tab{{Name: agentTabName, Kind: TabKindAgent, Conversation: AgentConversationData{Agent: tmux.ProgramCodex, ID: carryTestID}}},
	}
	require.True(t, inst.AccountSwapCarryRecorded(), "the in-memory precondition holds")
	require.Equal(t, handoff.CarryUnresolved, inst.AccountSwapCarryRefusal())
	require.Equal(t, "af has no record of the command this session launched, so it cannot locate its conversation",
		inst.accountSwapCarryRefusalReason())
}

// The ambient identity carries from the home the launch command resolves.
// The answer reads that home's shared snapshot, so a rollout written since is
// seen once the snapshot's TTL runs out.
func TestAccountSwapCarryPossibleFromTheAmbientHome(t *testing.T) {
	clock := useCarryAheadStores(t)
	home := carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	require.Equal(t, handoff.CarryMissing, inst.AccountSwapCarryRefusal(), "no rollout yet")
	writeCarryFile(t, filepath.Join(home, ".codex"), codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
	clock.advance(carryAheadStoreTTL)
	require.Empty(t, inst.AccountSwapCarryRefusal())
}
