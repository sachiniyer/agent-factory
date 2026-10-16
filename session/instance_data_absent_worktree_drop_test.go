package session

import (
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFromInstanceData_LiveRowAbsentSessionFailingRespawnDropsRow proves the
// production materializer-drop half of #4812: a persisted Live row (Running
// status, existing worktree paths, and a confirmed-absent tmux session)
// reaches instance.Start(false) → RestoreWithResult → TmuxSession.Start(workDir)
// → tmux new-session, and when that respawn fails with a non-inert error,
// FromInstanceData returns (nil, err) so the row is dropped at the
// materializer. This is the link the daemon
// skip-set fix (daemon.refreshDaemonInstances) builds on: IF the materializer
// errors on every row, a startup-skipped repo's parses-but-zero-rows file would
// clear the skip set on the "parses" signal alone and silently serve [] on the
// wire. The test stages the real FromInstanceData (no stub of the materializer)
// through the same restoreTmuxSession seam dead_respawn_test.go already uses.
func TestFromInstanceData_LiveRowAbsentSessionFailingRespawnDropsRow(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_unloadable_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	// Every session is confirmed-absent: countingExec with an empty alive set
	// answers "no such session" to every has-session, so the agent's Restore
	// takes the confirmed-absent branch and calls TmuxSession.Start(workDir).
	var newSessions, ptyNewSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	// failFirstNewSessionPty fails the first tmux new-session it sees — the
	// agent's re-spawn — reproducing "the worktree was removed so
	// `tmux new-session -c $workdir` errors" (dead_respawn_test.go:245), then
	// lets a later fresh new-session succeed.
	pty := failFirstNewSessionPty{t: t, cmdExec: exec, count: &ptyNewSessions}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	// deadInstanceData(t, Running, ...) is a Live row with an existing worktree
	// path but a confirmed-absent tmux session. The row passes every inert
	// short-circuit in instance_data.go and reaches Start(false). (A row whose
	// worktree is ALSO missing no longer reaches here — it loads Lost for the
	// rebuild path; see TestFromInstanceData_LiveRowAbsentWorktreeLoadsLost.)
	restored, err := FromInstanceData(deadInstanceData(t, Running, agentName, shellName))

	// The real materializer, through the real LocalBackend.Start → launch →
	// RestoreWithResult → TmuxSession.Start path, drops the row: it returns
	// (nil, err) with a non-inert error (start.go:133 wraps the pty failure in
	// ErrSessionNotStarted, which retainsInertInstance reports false).
	require.Error(t, err, "a Live row whose confirmed-absent re-spawn fails must error, not load inert")
	require.Nil(t, restored, "the unloadable row must not materialize an Instance")
	require.False(t, retainsInertInstance(err),
		"the pty new-session failure is non-inert (ErrSessionNotStarted-wrapped), so the row is dropped, not retained")
	// Exactly one pty new-session was attempted — the agent's re-spawn — and it
	// failed; FromInstanceData returned before any shell tab processing.
	assert.Equal(t, 1, ptyNewSessions,
		"exactly the agent re-spawn was attempted (and failed) before the row dropped")
	// The exec's new-session counter stays 0: the pty failed before forwarding
	// to the mock executor, so no session was recorded as created.
	assert.Equal(t, 0, newSessions,
		"the failed re-spawn never reached the executor, so no session was recorded live")
}

// TestFromInstanceData_LiveRowAbsentWorktreeLoadsLost is the #5172 routing half
// of the same scenario: a persisted Live row whose tmux session is gone AND
// whose worktree vanished (the #5150 outage shape) must NOT be dropped and must
// NOT be respawned into tmux's fallback directory. The spawn-dir seam refuses
// the re-spawn, and FromInstanceData loads the row Lost — the daemon's
// lost-restore loop only iterates m.instances, so a dropped row could never
// reach Recover's rebuild path. Lost + started is the shape that loop admits
// (lostSessionWantsRestore → RuntimeActionRecoverLost requires both).
func TestFromInstanceData_LiveRowAbsentWorktreeLoadsLost(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_missing_wt_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))

	restored, err := FromInstanceData(data)
	require.NoError(t, err,
		"a Live row whose worktree vanished must load, not drop — a dropped row is invisible to RestoreLostSessions")
	require.NotNil(t, restored)
	assert.Equal(t, Lost, restored.GetStatus(),
		"the absent-session + missing-worktree row must surface as Lost")
	assert.True(t, restored.Started(),
		"the row must stay started so the lost-restore loop admits it for rebuild+respawn")
	assert.Equal(t, 0, newSessions,
		"no tmux spawn may be attempted for a worktree that is not there")
}
