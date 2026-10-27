package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The #4222 report at the session axis: a Lost-restore whose recovery RESPAWNED
// the runtime — the agent that received the task prompt is gone, and the
// replacement process is never replayed it — must end the run as interrupted at
// the restore's ConfirmLive edge. Before the carve-out, ConfirmLive kept the
// run marker (runKeep) and the replacement pane's first idle observation became
// the run's "completion" edge — the transition that hands the daemon's
// on_complete verb a session whose interrupted work it would then kill or
// archive.

func TestTaskRunInterruptedWhenRestoreRespawned(t *testing.T) {
	inst := taskRunSession(t)

	// The session is lost mid-run and the daemon raises the restore fence —
	// the state every Lost-restore reaches before its runtime work begins.
	// MarkRestoring is the edge BeginRecoverFence drives for a Lost row.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	require.NoError(t, inst.Transition(MarkRestoring()))
	require.True(t, inst.TaskRunActive(), "the run survives the loss — its recovery decides")

	// The backend's respawn verdict, stamped inside the fenced recover before
	// its ConfirmLive edge: the runtime going live is a REPLACEMENT.
	inst.MarkAgentRuntimeReplacedForTest(true)
	require.NoError(t, inst.Transition(ConfirmLive()))

	// The run ended HERE, inside the ConfirmLive — on master the marker is
	// still set and only the idle edge below clears it.
	require.False(t, inst.TaskRunActive(),
		"a restore landing on a replaced runtime must end the run as interrupted, not leave it for the idle edge")
	require.True(t, inst.TaskRunInterrupted())
	require.True(t, inst.ConsumeTaskRunInterrupted(),
		"the daemon's status stamp is owed exactly once")
	require.False(t, inst.ConsumeTaskRunInterrupted())

	// The replacement pane's first quiet tick is no longer a completion edge —
	// the run is already over, and the sticky flag keeps it that way.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive())
	require.True(t, inst.TaskRunInterrupted())
}

// The mirror: a restore that REATTACHED to the still-running agent — a tmux
// session that survived whatever fenced it — is the same runtime that owns the
// prompt, so its later idle edge remains the genuine completion.
func TestTaskRunSurvivesVerifiedReattach(t *testing.T) {
	inst := taskRunSession(t)

	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	require.NoError(t, inst.Transition(MarkRestoring()))

	// The reattach verdict the backend stamps when the pane root's
	// (pid, start-time) still names the recorded launch.
	inst.MarkAgentRuntimeReplacedForTest(false)
	require.NoError(t, inst.Transition(ConfirmLive()))

	require.True(t, inst.TaskRunActive(),
		"a verified reattach is the prompted agent still running — the run stays in flight")
	require.False(t, inst.TaskRunInterrupted())
}

// The limit-resume twin must NOT interrupt: OpRespawning's ConfirmLive is the
// path that re-delivers the pending prompt to the respawned runtime, so the
// replacement there genuinely continues the run (#2997). Only OpRestoring —
// the restore that never replays — interrupts.
func TestTaskRunRespawnConfirmLiveDoesNotInterrupt(t *testing.T) {
	inst := taskRunSession(t)

	require.NoError(t, inst.Transition(BeginRespawn()))
	inst.MarkAgentRuntimeReplacedForTest(true)
	require.NoError(t, inst.Transition(ConfirmLive()))

	require.True(t, inst.TaskRunActive(),
		"a limit-resume respawn re-delivers the prompt — the run continues")
	require.False(t, inst.TaskRunInterrupted())
}

// The verification verdict itself: a reattach can only be trusted when the
// pane identity the launch recorded still resolves to the runtime — a missing
// pane, or a launch that never recorded one, is unverified and the caller must
// treat the reattach as a replacement (#4222 review). The verdict stands alone
// because whether clearing that evidence changed durable state is the wrong
// question — a launch with no recorded PID has nothing to clear, yet nothing
// to trust either.
func TestReattachRuntimeVerifiedRequiresProvenIdentity(t *testing.T) {
	inst := taskRunSession(t)

	// No pane handle at all → nothing to verify against.
	require.False(t, inst.reattachRuntimeVerified(nil))
}

// The load path's sibling: a daemon restart that had to respawn the runtime
// interrupts an in-flight run through InterruptTaskRunIfRuntimeReplaced — no
// ConfirmLive edge exists where Start(false) re-creates a gone process.
func TestTaskRunInterruptedOnLoadReplacement(t *testing.T) {
	inst := taskRunSession(t)

	inst.MarkAgentRuntimeReplacedForTest(true)
	require.True(t, inst.InterruptTaskRunIfRuntimeReplaced(),
		"a load-time respawn ends the in-flight run as interrupted")
	require.False(t, inst.TaskRunActive())
	require.True(t, inst.TaskRunInterrupted())

	// A verified reattach leaves the run alone.
	running := taskRunSession(t)
	running.MarkAgentRuntimeReplacedForTest(false)
	require.False(t, running.InterruptTaskRunIfRuntimeReplaced())
	require.True(t, running.TaskRunActive())
}
