package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestArmConversationCapturesSpendsTheParkedBaselineOnTheReplacementArm is the
// #6946 regression. A refused-resume respawn parks its pre-spawn baseline
// while the OUTGOING runtime generation still reads as current — the respawn
// bumps agentRuntimeGeneration only at the end of the body. A poll tick
// inside that window must not spend the baseline on the about-to-die
// generation: the loop it launches has every commit refused by the generation
// gate and exits, the baseline is gone, and the replacement's rollout —
// typically already minted, because the limit-resume body delivers the
// mission right after the respawn — is invisible to every later fresh
// baseline.
func TestArmConversationCapturesSpendsTheParkedBaselineOnTheReplacementArm(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, _, _, codexHome := codexLoopTestInstance(t)

	// The refused-resume respawn's pre-spawn baseline, parked over the retired
	// id while the outgoing generation is still the current one — the respawn
	// body's own ordering.
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "019f386f-7206-7fc2-803b-f7045e07a242",
	})
	inst.RetireConversationForFreshRuntime(
		"019f386f-7206-7fc2-803b-f7045e07a242",
		session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath()))

	// The poll tick lands inside the respawn window: whatever it arms, it must
	// not spend the baseline here.
	manager.ArmConversationCaptures()

	// The respawn's generation bump lands — the pane already runs the
	// replacement, and the runtime generation only now catches up.
	inst.ReconcileAgentRuntimeSnapshot(session.InstanceData{
		Program:      "codex --fresh",
		CurrentAgent: tmux.ProgramCodex,
	})

	// The replacement's rollout was minted before the next deferred arm could
	// take a fresh baseline — exactly the file only the parked baseline still
	// names as new.
	writeDaemonCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-17-35-5299e00d-1111-4222-8333-f7045e07a242.jsonl",
		inst.GetWorktreePath())

	manager.ArmConversationCaptures()
	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == "5299e00d-1111-4222-8333-f7045e07a242"
	}, 2*time.Second, 10*time.Millisecond,
		"a deferred arm that could not commit must leave the parked baseline for the arm that can")
}

// TestArmConversationCapturesLaunchRefusalLeavesTheParkedBaseline: the last
// post-take decline — a launch refused while the daemon closes for shutdown —
// must hand the consumed baseline back: the park's only consumer is an arm
// that actually proceeds.
func TestArmConversationCapturesLaunchRefusalLeavesTheParkedBaseline(t *testing.T) {
	manager, inst, _, _, codexHome := codexLoopTestInstance(t)

	inst.RetireConversationForFreshRuntime(
		"019f386f-7206-7fc2-803b-f7045e07a242",
		session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath()))
	// The replacement generation the park waits for.
	inst.ReconcileAgentRuntimeSnapshot(session.InstanceData{
		Program:      "codex --fresh",
		CurrentAgent: tmux.ProgramCodex,
	})

	manager.stopAndWaitBackgroundMutationsForShutdown()
	manager.ArmConversationCaptures()

	require.True(t, inst.PendingConversationCaptureParkedForTest(),
		"a declined launch must hand the parked baseline back")
	_, armed := armedConversationToken(t, manager, inst)
	require.False(t, armed, "a refused launch leaves no arm behind")
}
