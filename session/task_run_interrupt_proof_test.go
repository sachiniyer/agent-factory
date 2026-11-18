package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

// #7518 R1: TaskRunInterrupted says the run's runtime was replaced, not that the
// process its prompt went to is gone. Only a respawn proves that, because tmux
// takes the respawn branch only after confirming the agent's session absent. A
// reattach whose identity could not be verified is also stamped "replaced", and
// its agent may be the very one still working on the prompt. The interrupt
// records which it was (TaskRunInterruptedByRespawn), and the daemon's pending
// fire reconcile delivers an occurrence again only on the first.

// lostRestoreFence is the state every Lost-restore reaches before its runtime
// work begins.
func lostRestoreFence(t *testing.T, inst *Instance) {
	t.Helper()
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	require.NoError(t, inst.Transition(MarkRestoring()))
}

func TestTaskRunInterruptRecordsWhetherARespawnEndedIt(t *testing.T) {
	t.Run("restore respawn", func(t *testing.T) {
		inst := taskRunSession(t)
		lostRestoreFence(t, inst)
		inst.markAgentRespawned()
		require.NoError(t, inst.Transition(ConfirmLive()))
		require.True(t, inst.TaskRunInterrupted())
		require.True(t, inst.ToInstanceData().TaskRunInterruptedByRespawn,
			"a respawn ran only after tmux confirmed the agent's session absent")
	})
	t.Run("restore reattach that could not be verified", func(t *testing.T) {
		inst := taskRunSession(t)
		lostRestoreFence(t, inst)
		inst.MarkAgentRuntimeReplacedForTest(true)
		require.NoError(t, inst.Transition(ConfirmLive()))
		require.True(t, inst.TaskRunInterrupted(), "#4222 still ends the run: its completion edge cannot be trusted")
		require.False(t, inst.ToInstanceData().TaskRunInterruptedByRespawn,
			"an unverified reattach proves nothing about the prompted process")
	})
	t.Run("a later replacement verdict retracts an earlier respawn", func(t *testing.T) {
		inst := taskRunSession(t)
		lostRestoreFence(t, inst)
		inst.markAgentRespawned()
		inst.MarkAgentRuntimeReplacedForTest(true)
		require.NoError(t, inst.Transition(ConfirmLive()))
		require.False(t, inst.ToInstanceData().TaskRunInterruptedByRespawn,
			"the verdict that ends the run is this attempt's, never a stale one")
	})
	t.Run("load respawn", func(t *testing.T) {
		inst := taskRunSession(t)
		inst.markAgentRespawned()
		require.True(t, inst.InterruptTaskRunIfRuntimeReplaced())
		require.True(t, inst.ToInstanceData().TaskRunInterruptedByRespawn)
	})
	t.Run("load reattach whose claim was retired", func(t *testing.T) {
		inst := taskRunSession(t)
		inst.MarkAgentRuntimeReplacedForTest(true)
		require.True(t, inst.InterruptTaskRunIfRuntimeReplaced())
		require.False(t, inst.ToInstanceData().TaskRunInterruptedByRespawn)
	})
	t.Run("a resumed run drops it with the interrupt", func(t *testing.T) {
		inst := taskRunSession(t)
		inst.markAgentRespawned()
		require.True(t, inst.InterruptTaskRunIfRuntimeReplaced())
		inst.mu.Lock()
		inst.taskRunInterrupted = false
		inst.mu.Unlock()
		require.False(t, inst.ToInstanceData().TaskRunInterruptedByRespawn,
			"the proof is written only alongside the interrupt it qualifies")
	})
}

// The real paths, through the local backend with tmux mocked. Recover's
// reattach to a live agent whose pane identity cannot be verified (no recorded
// pid) is R1's exact mechanism; its respawn of a confirmed-absent session is
// the one that proves the agent gone. The load path draws the same line.
func TestLocalBackendTaskRunInterruptCause(t *testing.T) {
	const tmuxName = "af_7518_interrupt_cause"
	local := func(t *testing.T, alive bool, liveness Liveness) *Instance {
		t.Helper()
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := initTempGitRepo(t)
		gw, err := git.NewGitWorktreeFromStorage(repoRoot, repoRoot, tmuxName, "main", "", true, false)
		require.NoError(t, err)
		cmdExec := nameKeyedExec(map[string]bool{tmuxName: alive})
		ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(tmuxName, "claude", persistPtyFactory{t: t, cmdExec: cmdExec}, cmdExec)
		return &Instance{
			Title: "interrupt-cause", TaskID: "task-1", Path: repoRoot, Program: "claude",
			backend: &LocalBackend{}, liveness: liveness, started: true, taskRunActive: true,
			gitWorktree: gw, Tabs: []*Tab{newAgentTab(ts)},
		}
	}
	recoverAndConfirm := func(t *testing.T, inst *Instance) {
		t.Helper()
		require.NoError(t, inst.Transition(MarkRestoring()))
		require.NoError(t, inst.backend.Recover(inst))
		require.NoError(t, inst.Transition(ConfirmLive()))
		require.True(t, inst.TaskRunInterrupted())
	}

	t.Run("recover reattaches a live agent it cannot verify", func(t *testing.T) {
		inst := local(t, true, LiveLost)
		recoverAndConfirm(t, inst)
		require.False(t, inst.ToInstanceData().TaskRunInterruptedByRespawn,
			"the live agent may be the one running the prompt")
	})
	t.Run("recover respawns a confirmed-absent agent", func(t *testing.T) {
		inst := local(t, false, LiveLost)
		recoverAndConfirm(t, inst)
		require.True(t, inst.ToInstanceData().TaskRunInterruptedByRespawn)
	})
	t.Run("load reattaches a live agent it cannot verify", func(t *testing.T) {
		inst := local(t, true, LiveReady)
		inst.runtimeProgram = "/old/claude" // a recorded claim the reattach cannot prove
		require.NoError(t, inst.Start(false))
		require.True(t, inst.InterruptTaskRunIfRuntimeReplaced())
		require.False(t, inst.ToInstanceData().TaskRunInterruptedByRespawn)
	})
	t.Run("load respawns a confirmed-absent agent", func(t *testing.T) {
		inst := local(t, false, LiveReady)
		require.NoError(t, inst.Start(false))
		require.True(t, inst.InterruptTaskRunIfRuntimeReplaced())
		require.True(t, inst.ToInstanceData().TaskRunInterruptedByRespawn)
	})
}

// The proof is durable with the interrupt it qualifies, and never without it.
func TestTaskRunInterruptedByRespawnSurvivesAReload(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	for _, tc := range []struct {
		name               string
		interrupted, proof bool
		want               bool
	}{
		{"respawned", true, true, true},
		{"interrupted without proof", true, false, false},
		{"no interrupt", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded, err := FromInstanceData(InstanceData{
				Title: "reload", BackendType: "docker", Liveness: LiveArchived,
				TaskRunInterrupted: tc.interrupted, TaskRunInterruptedByRespawn: tc.proof,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, loaded.ToInstanceData().TaskRunInterruptedByRespawn)
		})
	}
}
