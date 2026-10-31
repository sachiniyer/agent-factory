package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// registerClientlessRemoteLostHandoff stages a remote-workspace instance in
// the exact post-restart shape the reattach feature (#5571 W1.5) made possible:
// remoteClient == nil (the loader left it inert; the one-shot startup reattach
// has not yet re-bound it), so AgentServer() is the stateless
// deadRemoteAgentServer; liveness LiveLost, started down, an OpReplacing fence
// reconstructed from a pending takeover mission whose verdict is
// PromptNotDelivered — the verdict automatic recovery owns. It is the row
// recovery's first ResumePendingHandoffs pass reaches before reattach when it
// wins the per-session op-lock race, and the row the burn-then-latch bug wedged.
func registerClientlessRemoteLostHandoff(t *testing.T, m *Manager, repoID, repoPath, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	// No RemoteAgentServer ⇒ remoteClient stays nil, so a remote-workspace
	// backend's AgentServer() is the deadRemoteAgentServer the bug burns against.
	backend := &remoteWorkspaceBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)
	// The fence is reconstructed by the loader for a PromptNotDelivered mission,
	// so raise it the way a live handoff would (BeginHandoff only admits a live
	// row), then drop liveness to Lost — the same sequence the existing dead-row
	// handoff test uses — and finally lower `started` to the inert value a
	// reloaded sandbox row loads with.
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission) // sets PromptNotDelivered
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptNotDelivered))
	require.NoError(t, inst.Transition(session.BeginHandoff()))
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	inst.SetStartedForTest(false)
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	// Persist the full reloaded shape (mission + verdict + fence-reconstructing
	// verdict) so the on-disk record is the one a restart would have loaded —
	// the refusal under test writes nothing, so the record must already carry
	// the obligation for the post-condition to read it back.
	m.persistInstance(repoID, inst)
	return inst
}

// TestResumePendingHandoffs_ClientlessRemoteDoesNotBurnIntoPermanentWedge is the
// regression test for the race the reattach feature opened (#5634): a reloaded
// ssh/sandbox row the startup reattach has not yet re-bound is client-less, so
// AgentServer() is the deadRemoteAgentServer sentinel. When recovery's first
// pass won the per-session op-lock race, it admitted the row into a readiness
// wait whose only gate was that sentinel — burning the full readiness budget
// and then latching MarkStartupStateUnknown — which permanently refused every
// recovery exit for a runtime that was alive and rebindable.
//
// The fix refuses the retry BEFORE the readiness wait for an unbound row, so
// recovery does not burn the budget or latch the terminal marker, and the row
// keeps the shape reattach (or restore) can still act on.
func TestResumePendingHandoffs_ClientlessRemoteDoesNotBurnIntoPermanentWedge(t *testing.T) {
	// Shrink the readiness budget so a regression that re-admitted the unbound
	// row into the readiness wait burns a short, measurable interval rather than
	// 60s. The fix refuses before the wait starts, so the call returns well under
	// this budget.
	restore := task.SetWaitForReadyTimingForTest(2*time.Second, 20*time.Millisecond)
	t.Cleanup(restore)

	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerClientlessRemoteLostHandoff(t, manager, repoID, repoPath, "clientless-handoff")
	mission := inst.PendingHandoffMission()
	require.Equal(t, session.PromptNotDelivered, inst.PendingHandoffDeliveryStatus())
	require.True(t, inst.PendingHandoffMissionAutoRetryable(), "fixture: the verdict admits automatic recovery")
	require.False(t, inst.StartupStateUnknown(), "fixture: the reloaded row is not yet startup-unknown")
	require.True(t, inst.RemoteRuntimeUnbound(), "fixture: the row is client-less — AgentServer() is the sentinel")

	start := time.Now()
	manager.ResumePendingHandoffs()
	elapsed := time.Since(start)

	// The fix refuses the retry before entering the readiness wait, so the call
	// returns in milliseconds, not the (shrunk) 2s budget. A regression that
	// re-admitted the unbound row would burn through the 2s first.
	require.Less(t, elapsed, 500*time.Millisecond,
		"recovery must refuse an unbound remote row immediately, not burn the readiness budget polling the deadRemoteAgentServer sentinel")

	// No permanent wedge: the row is exactly as the restart left it — still
	// unbound + Lost, the OpReplacing fence still up, the mission still pending
	// behind its positive non-delivery verdict — so the startup reattach (or
	// restore) that owns this row can still reach it.
	require.False(t, inst.StartupStateUnknown(),
		"a client-less remote row must not be latched startup-unknown from a readiness burn it never legitimately ran")
	require.Equal(t, session.PromptNotDelivered, inst.PendingHandoffDeliveryStatus(),
		"the verdict that admitted automatic recovery must be unchanged — a burn against the sentinel proves nothing")
	require.True(t, inst.PendingHandoffMissionAutoRetryable(),
		"the mission stays auto-retryable until a real runtime is bound and misses")
	require.Equal(t, mission, inst.PendingHandoffMission(), "the durable obligation stays")
	require.Equal(t, session.OpReplacing, inst.GetInFlightOp(), "the reconstructed fence stays up for recovery to settle")
	require.Equal(t, session.LiveLost, inst.GetLiveness(), "liveness is unchanged")
	require.True(t, inst.RemoteRuntimeUnbound(),
		"the row stays client-less and reattachable, not terminally marked")

	// The operator verbs see the SAME Lost row they would have before the burn —
	// neither gains a wedge exit, and neither is closed by a permanent
	// startup-unknown marker. The retry and confirm verbs refuse a Lost runtime
	// (they always did); the restore verb is refused for the NOT-STARTED inert
	// row, never for the "unknown startup state" the bug latched.
	require.False(t, inst.CanRetryPendingHandoffMissionDelivery(), "a Lost row is not operator-retryable")
	require.False(t, inst.CanConfirmPendingHandoffDelivery(), "a Lost row is not operator-confirmable")
	restoreErr := inst.ValidateRuntimeAction(session.RuntimeActionRestoreLostOrDead)
	require.Error(t, restoreErr)
	require.NotContains(t, restoreErr.Error(), "unknown startup state",
		"restore must not be vetoed by the permanent startup-unknown marker the bug latched — "+
			"a reloaded inert row is restore-ineligible for the ordinary not-started reason")

	// A second sweep must leave the row in the SAME recoverable shape: recovery
	// does not accumulate state on a row it refuses, and the refusal is stable.
	manager.ResumePendingHandoffs()
	require.False(t, inst.StartupStateUnknown(), "a repeated sweep must not wedge the row either")
	require.Equal(t, session.PromptNotDelivered, inst.PendingHandoffDeliveryStatus())
	require.Equal(t, mission, inst.PendingHandoffMission())
	require.Equal(t, session.OpReplacing, inst.GetInFlightOp())
	require.True(t, inst.PendingHandoffMissionAutoRetryable())
	require.True(t, inst.RemoteRuntimeUnbound())

	// And the persisted record carries the recoverable shape — a restart would
	// reload the row for reattach, not a permanent startup-unknown wedge.
	rec := recordFor(t, repoID, inst.Title)
	require.NotNil(t, rec)
	require.Equal(t, mission, rec.PendingHandoffMission, "the obligation is durable")
	require.False(t, rec.StartupStateUnknown, "no startup-unknown marker is persisted")
	require.Equal(t, session.PromptNotDelivered, rec.HandoffDeliveryStatus)
}

// TestResumePendingHandoffs_LocalLostHandoffStillBurnsIntoUnknown proves the
// gate and safety net are surgical: a LOCAL row (the pre-reattach shape) with
// the same fence + mission is NOT unbound-remote, so recovery still admits it,
// burns the readiness budget against a pane that never becomes ready, and
// latches startup-unknown — the established #4429 outcome the fix must not
// regress. The bug was strictly about client-less REMOTE rows whose readiness
// burn was against a stateless sentinel, not a real runtime.
func TestResumePendingHandoffs_LocalLostHandoffStillBurnsIntoUnknown(t *testing.T) {
	// Shrink the readiness budget so the burn is fast, and the trust-prompt
	// loop's timing so it never adds to it.
	t.Cleanup(task.SetWaitForReadyTimingForTest(200*time.Millisecond, 10*time.Millisecond))
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))

	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend:    session.NewFakeBackend(),
		deliveryStatus: session.PromptDelivered,
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "local-burn", backend)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, session.PromptNotDelivered))
	require.NoError(t, inst.Transition(session.BeginHandoff()))
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	inst.SetStartedForTest(false)
	require.False(t, inst.RemoteRuntimeUnbound(),
		"fixture: a local row is never unbound-remote, so the gate must admit it")

	manager.ResumePendingHandoffs()

	// A local row that fails readiness latches startup-unknown as before — the
	// gate never refused it (it is not an unbound remote row), and the safety net
	// does not skip the latch (the burn was against a real local pane, not a
	// stateless sentinel). This is the established #4429 outcome.
	require.True(t, inst.StartupStateUnknown(),
		"a local Lost row that fails readiness keeps the established startup-unknown outcome")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"MarkStartupStateUnknown clears the op axis, as before")
	require.False(t, inst.PendingHandoffMissionAutoRetryable(),
		"the latched row drops out of automatic recovery, as before")
}
