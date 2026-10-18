package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentTmuxForRecover returns the agent tab's tmux session for an instance built
// by lostInstanceForRecover. The agent tab is Tabs[0]; reading its tmux under
// i.mu mirrors the in-package pattern used by recover_test.go's account-swap
// fixture.
func agentTmuxForRecover(i *Instance) *tmux.TmuxSession {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if len(i.Tabs) == 0 {
		return nil
	}
	return i.Tabs[0].tmux
}

// TestRecover_ReattachStillBumpsAgentRuntimeGeneration is the reproduction named
// in the bug report: a Recover that reattaches by tmux name WITHOUT a recorded
// (pid, start-time) identity still bumps agentRuntimeGeneration. The fixture
// mirrors TestRecover_UnverifiedReattachClearsRuntimeProgram — both the agent
// and shell tmux names exist, so RestoreWithResult takes the reattach branch
// (zero new sessions) — and asserts the generation around Recover.
//
// The bump is EXPECTED here, both before and after the fix: a record carrying
// no identity cannot prove it still names the launch AF recorded, so
// clearRuntimeProgramForUnverifiedReattach retires the claim, and a retired
// claim is exactly the "process actually changed (or unverifiable)" verdict
// that warrants the post-replacement hook. This test therefore pins the
// retired-claim half of the gate the fix introduces — proving the gate did not
// regress the invalidation a genuine process change still requires — while
// TestRecover_ReattachSameLaunchPreservesAgentRuntimeGeneration pins the
// same-launch half that constitutes the bug.
func TestRecover_ReattachStillBumpsAgentRuntimeGeneration(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_reattach_no_id"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{agentName: true, shellName: true}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)
	// setRuntimeProgram records a command WITHOUT a (pid, start-time) identity
	// (setRuntimeLaunch(program, nil) underneath), so a reattach cannot prove
	// the pane is still that launch and must retire the claim.
	restored.setRuntimeProgram("/old/claude")

	require.Zero(t, restored.agentRuntimeGeneration,
		"precondition: a fresh load of a Lost instance never bumps the runtime generation")

	require.NoError(t, restored.Recover())
	require.Zero(t, newSessions,
		"the fixture must exercise name-only reattachment, not a verified respawn")
	assert.Equal(t, uint64(1), restored.agentRuntimeGeneration,
		"a reattach that retires the launch claim must still bump the runtime generation "+
			"so the post-replacement hook invalidates captures bound to the unverifiable pane")
}

// TestRecover_ReattachSameLaunchPreservesAgentRuntimeGeneration is the core fix:
// a Recover that reattaches to a pane whose (pid, start-time) identity provably
// matches the launch AF recorded must NOT bump agentRuntimeGeneration and must
// NOT tear down a healthy PTY broker capture on that pane — the pane was never
// replaced. The mismatch between this expected behavior and the pre-fix
// unconditional resetAgentBrokerCaptures(i) is the bug: it bumped the
// generation (silently and permanently dropping any in-flight conversation-id
// capture for the still-live process) and blipped attached subscribers by
// discarding buffered output and clearing size state for nothing.
//
// The fixture spawns a real `sleep` process to stand in for the pane root, so
// the (pid, start-time) identity the launch recorded can be re-verified against
// the live process table on reattach (the same pattern the sibling
// TestLocalBackendStartVerifiedReattachKeepsRuntimeProgram uses for the load
// path). paneReportingExec answers the pane-identity probes (display-message at
// launch, list-panes at reattach) with that real pid, so
// clearRuntimeProgramForUnverifiedReattach hits its "provably the same launch:
// keep the claim" arm and returns false — and the hook must now be skipped.
func TestRecover_ReattachSameLaunchPreservesAgentRuntimeGeneration(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_reattach_same_launch"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	pane := spawnPaneLookalike(t)
	exec := paneReportingExec(t, map[string]bool{agentName: true, shellName: true}, pane.PID, pane.PID)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)
	// setRuntimeLaunch records the command AND the pane root's (pid, start-time)
	// identity (via the display-message probe paneReportingExec answers with
	// pane.PID), so a reattach can prove the surviving name still names it.
	restored.setRuntimeLaunch("/old/claude", agentTmuxForRecover(restored))
	require.Equal(t, pane.PID, restored.runtimePID)
	require.Equal(t, pane.StartID, restored.runtimeStartID)

	// Arm a clientless broker capture on the agent tab the way a prior Subscribe
	// would have, so the fix can be observed at the data plane too: a same-launch
	// reattach must leave this healthy capture alone (#1682 / #5067).
	channel := &fakeClientlessChannel{snapshot: []byte("old-pane")}
	broker := newPTYBroker(channel)
	sub, err := broker.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sub.Close()
		broker.close()
	})
	server := restored.AgentServer().(*localAgentServer)
	server.brokers = map[string]*ptyBroker{"agent": broker}
	require.Equal(t, 1, channel.starts, "precondition: the subscriber armed a capture")

	require.Zero(t, restored.agentRuntimeGeneration,
		"precondition: a Lost load and a recorded launch do not bump the runtime generation")

	require.NoError(t, restored.Recover())
	require.Zero(t, newSessions,
		"the fixture must exercise name-only reattachment, not a verified respawn")

	require.Zero(t, restored.agentRuntimeGeneration,
		"a reattach that provably recovered the SAME launch is reconstruction, not a "+
			"replacement: the runtime generation must stay put so in-flight captures "+
			"bound to the still-live process keep matching (mirrors the load path's "+
			"TestUpdatedAtLoadRuntimeBoundary reattach=existing arm)")
	assert.Equal(t, "/old/claude", restored.RuntimeProgram(),
		"a verified reattach must keep the recorded launch command — the pane root "+
			"matching the recorded identity is the proof that it is still that launch")
	assert.Equal(t, pane.PID, restored.runtimePID,
		"the recorded process identity must survive a verified reattach")
	assert.Equal(t, pane.StartID, restored.runtimeStartID,
		"the recorded process identity must survive a verified reattach")

	assert.Zero(t, channel.stops,
		"a same-launch reattach must NOT tear down the healthy PTY broker capture "+
			"(the pane never changed); the pre-fix unconditional resetAgentBrokerCaptures "+
			"blipped attached subscribers and discarded buffered output for nothing")
	assert.Equal(t, 1, channel.starts,
		"no capture teardown means no capture restart either — the subscriber that "+
			"armed the capture keeps streaming without a reconnect")
}

// TestRecover_ReattachReplacementRetiresClaimAndResetsBroker guards the
// retired-claim half of the fix's gate: a reattach whose surviving tmux name
// now names a DIFFERENT process (the operator removed and recreated the
// session, or the pid was reused) must still retire the launch claim, bump the
// runtime generation, and tear down a broker capture bound to the predecessor
// pane. The fix must not over-correct by skipping the hook on every reattach.
//
// The fixture spawns TWO real `sleep` processes: `recorded` is the launch AF
// recorded, `replacement` is what the surviving tmux name now runs. The pane
// probes answer with the replacement's pid, so PaneRootProcessMatches finds no
// row carrying the recorded identity and clearRuntimeProgramForUnverifiedReattach
// retires the claim — exactly the verdict the hook must run on.
func TestRecover_ReattachReplacementRetiresClaimAndResetsBroker(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_reattach_replaced"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	recorded := spawnPaneLookalike(t)
	replacement := spawnPaneLookalike(t)
	exec := paneReportingExec(t, map[string]bool{agentName: true, shellName: true}, replacement.PID, replacement.PID)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	// Seed the recorded launch identity directly: a real prior launch recorded
	// `recorded` and its (pid, start-time). The replacement under the same tmux
	// name must fail this identity check at reattach.
	restored.mu.Lock()
	restored.setRuntimeEvidenceLocked("/old/claude", recorded.PID, recorded.StartID)
	restored.mu.Unlock()
	require.Equal(t, recorded.PID, restored.runtimePID)
	require.Equal(t, recorded.StartID, restored.runtimeStartID)

	channel := &fakeClientlessChannel{snapshot: []byte("old-pane")}
	broker := newPTYBroker(channel)
	sub, err := broker.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sub.Close()
		broker.close()
	})
	server := restored.AgentServer().(*localAgentServer)
	server.brokers = map[string]*ptyBroker{"agent": broker}
	require.Equal(t, 1, channel.starts, "precondition: the subscriber armed a capture")

	require.Zero(t, restored.agentRuntimeGeneration, "precondition: generation starts at 0")

	require.NoError(t, restored.Recover())
	require.Zero(t, newSessions,
		"the fixture must exercise name-only reattachment, not a verified respawn")

	assert.Equal(t, uint64(1), restored.agentRuntimeGeneration,
		"a reattach that retires the launch claim (the recorded identity no longer "+
			"names the pane root) must bump the runtime generation so captures bound to "+
			"the predecessor process are invalidated")
	assert.Empty(t, restored.RuntimeProgram(),
		"the recorded launch command must be retired when the pane root is not the "+
			"recorded process")
	assert.Zero(t, restored.runtimePID,
		"the retired claim's process identity must be cleared with it")
	assert.Zero(t, restored.runtimeStartID,
		"the retired claim's process identity must be cleared with it")

	assert.Equal(t, 1, channel.stops,
		"a reattach under a different process must tear down the broker capture bound "+
			"to the predecessor pane so the next Subscribe streams the replacement")
	assert.Equal(t, 2, channel.starts,
		"the attached subscriber must resume on the replacement pane without a reconnect")
}
