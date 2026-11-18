package session

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The loader's half of #7518. A row the loader defers to the lost-restore loop
// records whether the agent its prompt went to is proven gone (LostAgentGone),
// and enrolls the load checkpoint so the daemon writes that verdict before its
// boot replay of a pending cron fire reads the row. The daemon's half, which
// decides from the verdict whether the fire delivers fresh or is held, is in
// daemon/taskrun_fires_lostrow_test.go.

// installRestoreExec routes every restored tmux session through exec.
func installRestoreExec(t *testing.T, cmdExec cmd_test.MockCmdExec) {
	t.Helper()
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })
}

// The missing-worktree shape #7518 reports: tmux confirms the agent's session
// gone and the spawn-dir seam refuses its respawn. The agent is proven gone.
func TestDeferredLoad_AgentSpawnDirRefusalProvesTheAgentGone(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_7518_gone_agent"
	var newSessions int
	installRestoreExec(t, countingExec(map[string]bool{}, &newSessions))
	data := deadInstanceData(t, Running, agentName, agentName+tmuxTabSeparator+shellTabName)
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	require.Zero(t, newSessions, "nothing may be spawned into a worktree that is not there")
	assert.True(t, restored.ToInstanceData().LostAgentGone,
		"tmux confirmed the agent's own session absent before the respawn was refused, so its process is gone")
	assert.True(t, restored.ConsumeLoadRuntimeReplacement(),
		"the Lost verdict must be checkpointed before the restored map installs, or the fire replay reads the pre-load row")
}

// The case the first version of #7524 got wrong: the socket is gone under a
// possibly-live server, so the agent may still be working on its prompt.
func TestDeferredLoad_UnverifiedSocketDoesNotProveTheAgentGone(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.PinServerProbeForTest(os.Getpid()))

	const agentName = "af_7518_unverified_agent"
	var newSessions int
	installRestoreExec(t, socketGoneExec(t, &newSessions))

	restored, err := FromInstanceData(deadInstanceData(t, Running, agentName, agentName+tmuxTabSeparator+shellTabName))
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	require.Zero(t, newSessions)
	assert.False(t, restored.ToInstanceData().LostAgentGone,
		"an unverified socket proves nothing about the agent: a live server may still be running it")
	assert.True(t, restored.ConsumeLoadRuntimeReplacement(),
		"the Lost verdict is checkpointed here too, so the replay sees Lost rather than the pre-load Running row")
}

// A sibling's spawn-dir refusal lands beside an agent that was just reattached
// alive. The row loads Lost on the sibling's refusal, and the agent it names is
// still running, so the refusal proves nothing about it.
func TestDeferredLoad_SiblingSpawnDirRefusalBesideALiveAgentDoesNotProveItGone(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_7518_live_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName
	installRestoreExec(t, nameKeyedExec(map[string]bool{agentName: true}))
	data := deadInstanceData(t, Running, agentName, shellName)
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus(), "the sibling's refusal routes the row Lost")
	require.True(t, restored.TabAlive(0), "the agent was reattached alive and must stay so")
	assert.False(t, restored.ToInstanceData().LostAgentGone,
		"only the agent's own refusal can prove the agent gone; this one is running")
}

func TestAgentProvenGone(t *testing.T) {
	spawnDir := fmt.Errorf("%w: tmux session %q missing on Restore and its worktree %s is unavailable: %w",
		tmux.ErrSpawnDirMissing, "af_x", "/gone", os.ErrNotExist)
	notDurable := fmt.Errorf("%w: session %q, refused conversation %s: %w",
		ErrReplacementNotDurable, "x", "c1", errors.New("disk full"))
	unverified := fmt.Errorf("restore deferred: %w — not respawning session %q", tmux.ErrSessionSocketUnverified, "af_x")
	agent := func(err error) error {
		return fmt.Errorf("failed to restore existing session: %w", &agentRestoreRefusal{err})
	}
	sibling := func(err error) error { return fmt.Errorf("restore tab %q for %q: %w", "shell", "x", err) }

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"agent spawn dir missing", agent(spawnDir), true},
		{"agent replacement not durable", agent(notDurable), true},
		{"agent socket unverified", agent(unverified), false},
		{"sibling spawn dir missing", sibling(spawnDir), false},
		{"sibling replacement not durable", sibling(notDurable), false},
		{"unwrapped spawn dir missing", spawnDir, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, restoreDeferred(tc.err), "every case is a deferral; only what it proves differs")
			assert.Equal(t, tc.want, agentProvenGone(tc.err))
		})
	}
	assert.Equal(t, agent(spawnDir).Error(), "failed to restore existing session: "+spawnDir.Error(),
		"marking the agent's refusal must not change its message")
}

// The proof describes one Lost episode. It is written only while the row is
// Lost, and once the row leaves Lost no later Lost inherits it.
func TestLostAgentGoneLivesForOneLostEpisode(t *testing.T) {
	i := &Instance{Title: "episode", liveness: LiveLost, lostAgentGone: true}
	require.True(t, i.ToInstanceData().LostAgentGone)

	i.mu.Lock()
	i.assignLivenessLocked(LiveLost)
	i.mu.Unlock()
	require.True(t, i.ToInstanceData().LostAgentGone, "a Lost-to-Lost write keeps the episode")

	i.mu.Lock()
	i.assignLivenessLocked(LiveRunning)
	i.mu.Unlock()
	require.False(t, i.ToInstanceData().LostAgentGone, "a rebind or respawn ends the episode")

	i.mu.Lock()
	i.assignLivenessLocked(LiveLost)
	i.mu.Unlock()
	require.False(t, i.ToInstanceData().LostAgentGone, "a later Lost must not inherit an earlier episode's proof")

	stale := &Instance{Title: "stale", liveness: LiveRunning, lostAgentGone: true}
	assert.False(t, stale.ToInstanceData().LostAgentGone, "the proof is never written for a row that is not Lost")
}

// The proof is durable: a row persisted Lost with it reloads inert and keeps
// it, so a later boot's replay reads the same verdict.
func TestLostAgentGoneSurvivesAReload(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_7518_reload_agent"
	var newSessions int
	installRestoreExec(t, countingExec(map[string]bool{}, &newSessions))
	data := deadInstanceData(t, Lost, agentName, agentName+tmuxTabSeparator+shellTabName)
	data.Liveness = LiveLost
	data.LostAgentGone = true

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	require.Zero(t, newSessions)
	assert.True(t, restored.ToInstanceData().LostAgentGone)

	// A row persisted outside Lost never loads the proof. A live sandbox row
	// loads as a provisional Lost while its reattach runs, and that Lost proves
	// nothing either: the agent may still be running in the sandbox.
	for _, live := range []Liveness{LiveRunning, LiveReady, LiveArchived} {
		loaded, err := FromInstanceData(InstanceData{Title: "sandbox", BackendType: "docker", Liveness: live, LostAgentGone: true})
		require.NoError(t, err)
		assert.Falsef(t, loaded.ToInstanceData().LostAgentGone, "a row persisted %v must not load the proof", live)
	}
}
