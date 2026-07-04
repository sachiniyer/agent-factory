package session

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExec wraps countingExec and additionally captures every
// new-session command string, so tests can assert on the spawned program.
func recordingExec(alive map[string]bool, newSessions *int, spawns *[]string) cmd_test.MockCmdExec {
	inner := countingExec(alive, newSessions)
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "new-session") {
				*spawns = append(*spawns, cmd.String())
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
}

// lostInstanceForRecover loads a Lost instance through the production path
// (FromInstanceData → Start(false) → #970 guard, no re-spawn) with an
// EXISTING worktree directory, ready for Recover.
func lostInstanceForRecover(t *testing.T, agentName, shellName string, exec cmd_test.MockCmdExec) *Instance {
	t.Helper()
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	return restored
}

// TestRecover_RespawnsLostSession: the daemon's explicit Recover (#1108 PR 2)
// re-spawns a Lost instance's tmux session in its worktree — the operation the
// #970 guard forbids at load time — and flips the instance Running like a
// fresh create. The spawned program must carry the resolved-program injection
// (#1132 choke-point) exactly once.
func TestRecover_RespawnsLostSession(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_agent"
	shellName := agentName + shellTmuxSuffix
	var newSessions int
	var spawns []string
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	require.NoError(t, restored.Recover())

	assert.Greater(t, newSessions, 0, "Recover must re-spawn the missing tmux session")
	assert.Equal(t, Running, restored.GetStatus(),
		"a recovered session is booting its program: Running, like a fresh create")
	assert.True(t, restored.TabAlive(0), "the agent session must exist server-side after Recover")

	// The #1132 choke-point injected claude's --plugin-dir into the spawn —
	// exactly once, recomputed from the clean persisted Program (repeated
	// attempts must never accumulate flags).
	require.NotEmpty(t, spawns)
	agentSpawn := spawns[0]
	assert.Equal(t, 1, strings.Count(agentSpawn, "--plugin-dir"),
		"resolved-program injection must appear exactly once in the spawn: %s", agentSpawn)
}

// TestRecover_RetryAfterFailureInjectsFlagsOnce: a failed first attempt (the
// outage still biting) must leave the instance retryable — still Lost, tmux
// refs intact — and the eventual successful spawn must still carry the
// injected flags exactly once.
func TestRecover_RetryAfterFailureInjectsFlagsOnce(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_retry"
	shellName := agentName + shellTmuxSuffix
	var newSessions, ptyNewSessions int
	var spawns []string
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)

	// First new-session fails (server still hostile), later ones succeed.
	pty := failFirstNewSessionPty{t: t, cmdExec: exec, count: &ptyNewSessions}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	require.Error(t, restored.Recover(), "first attempt must surface the spawn failure")
	assert.Equal(t, Lost, restored.GetStatus(), "a failed recover leaves the session Lost")
	assert.True(t, restored.Started(), "a failed recover must keep the row killable")

	require.NoError(t, restored.Recover(), "retry must succeed once the server behaves")
	assert.Equal(t, Running, restored.GetStatus())
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, shellTmuxSuffix) {
			assert.Equal(t, 1, strings.Count(spawn, "--plugin-dir"),
				"every attempt recomputes injection from the clean Program: %s", spawn)
		}
	}
}

// TestRecover_FailsWithoutWorktree: a deleted worktree is the expected
// permanent-failure shape; Recover must name it (the restore loop's
// escalation log leans on this) and leave the session Lost and killable.
func TestRecover_FailsWithoutWorktree(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_nowt"
	shellName := agentName + shellTmuxSuffix
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// Worktree path deliberately NOT created.
	restored, err := FromInstanceData(deadInstanceData(t, Lost, agentName, shellName))
	require.NoError(t, err)

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree", "the failure must name the missing worktree")
	assert.Equal(t, 0, newSessions, "no spawn may happen without a worktree")
	assert.Equal(t, Lost, restored.GetStatus())
}

// TestRecover_RefusesNonLostAndTombstoned: Recover is for Lost sessions only —
// a live session must never be re-spawned over (adopt, never clobber), and a
// tombstoned record's only future is having its kill finished.
func TestRecover_RefusesNonLostAndTombstoned(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_guard"
	shellName := agentName + shellTmuxSuffix
	var newSessions int
	exec := countingExec(map[string]bool{agentName: true, shellName: true}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	live, err := FromInstanceData(deadInstanceData(t, Running, agentName, shellName))
	require.NoError(t, err)
	require.Error(t, live.Recover(), "a non-Lost session must be refused")

	tombstoned := lostInstanceForRecover(t, "af_recover_guard2", "af_recover_guard2"+shellTmuxSuffix,
		countingExec(map[string]bool{}, &newSessions))
	tombstoned.MarkUserKilled()
	require.Error(t, tombstoned.Recover(), "a tombstoned session must be refused")
}
