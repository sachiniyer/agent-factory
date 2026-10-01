package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #5023: the record a `--delivered` confirm publishes must read as working —
// the operator's attestation says the incoming agent already HAS its mission,
// so a snapshot that reads idle here is the false working -> idle edge a driver
// answers by prompting an agent still processing the confirmed mission. The
// classifier is deliberately stateless (TestClassifyWatchStop_
// PendingAccountSwapClearReleasesToIdle's clear-and-release is a real idle
// edge), so the truth has to come from the record itself: this test drives the
// real confirm methods on the exact LiveReady shapes a failed delivery leaves,
// feeds each published settlement through the watcher, and asserts no idle edge
// until the monitor's own idle observation arrives.
func TestFleetWatcher_ConfirmedDeliveryReadsAsWorking(t *testing.T) {
	dir := t.TempDir()

	// The manual account swap leg: a mission send that could not confirm leaves
	// LiveReady with PendingAccountSwap still set — the row #4997's gate already
	// holds as working.
	swapInst, err := session.FromInstanceData(session.InstanceData{
		ID: "confirmed-swap", Title: "confirmed-swap", Path: dir, Program: "claude",
		Liveness: session.LiveReady,
		Account:  "personal",
		Worktree: session.GitWorktreeData{RepoPath: dir, WorktreePath: dir},
		PendingAccountSwap: &session.AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			Mission: "continue", MissionDeliveryStatus: session.PromptCouldNotConfirm,
		},
	})
	require.NoError(t, err)
	midSwap := swapInst.ToInstanceData()
	require.Equal(t, watchWorking, mustReason(classifyWatchStop(midSwap)),
		"fixture: the pending-swap row is the working baseline")
	require.NoError(t, swapInst.ConfirmPendingManualAccountSwapDelivery("work", "personal"))
	swapConfirmed := swapInst.ToInstanceData()
	require.Nil(t, swapConfirmed.PendingAccountSwap)
	require.Equal(t, session.LiveRunning, swapConfirmed.Liveness,
		"the confirmed swap publishes working — its mission is already running")

	// The agent-handoff leg: a settled ambiguous mission on an unfenced,
	// already-idle row — the shape the could-not-confirm send leaves.
	handoffInst, err := session.NewInstance(session.InstanceOptions{
		Title: "confirmed-handoff", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	handoffInst.SetBackend(session.NewFakeBackend())
	handoffInst.SetStartedForTest(true)
	handoffInst.SetStatusForTest(session.Running)
	mission := "continue the inherited work"
	handoffInst.SetPendingHandoffMission(mission)
	require.NoError(t, handoffInst.RecordPendingHandoffMissionDelivery(mission, session.PromptSentUnverified))
	require.NoError(t, handoffInst.Transition(session.ObserveLiveness(session.LiveReady)))
	midHandoff := handoffInst.ToInstanceData()
	require.Equal(t, watchWorking, mustReason(classifyWatchStop(midHandoff)),
		"fixture: the owed-mission row is the working baseline")
	require.NoError(t, handoffInst.ConfirmPendingHandoffDelivery(mission))
	handoffConfirmed := handoffInst.ToInstanceData()
	require.Empty(t, handoffConfirmed.PendingHandoffMission)
	require.Equal(t, session.LiveRunning, handoffConfirmed.Liveness,
		"the confirmed mission publishes working — the agent already has it")

	// The watcher, over the same records its poll carries: the mid-transaction
	// baseline, then each confirm's settlement — and no idle edge on either.
	w := newFleetWatcher(false)
	require.Empty(t, w.observe([]session.InstanceData{midSwap, midHandoff}),
		"the working baselines emit nothing")
	require.Empty(t, w.observe([]session.InstanceData{swapConfirmed, handoffConfirmed}),
		"a confirmed delivery must not emit the working -> idle edge (#5023)")

	// Only the monitor's own idle evidence earns the edge: when each row later
	// reads Ready from a genuinely idle pane, the transition is reported.
	swapIdle := swapConfirmed
	swapIdle.Liveness = session.LiveReady
	handoffIdle := handoffConfirmed
	handoffIdle.Liveness = session.LiveReady
	require.Equal(t, map[string]watchStopReason{
		"confirmed-swap":    watchStopIdle,
		"confirmed-handoff": watchStopIdle,
	}, reasons(w.observe([]session.InstanceData{swapIdle, handoffIdle})),
		"the monitor-observed idle, and only it, produces the edge a driver waits on")
}
