package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFromInstanceData_ArchivedLoadsInert: an Archived record (#1028) loads
// WITHOUT calling Start — no tmux is spawned or reconnected, the instance stays
// started=false, and its gitWorktree is bound to the persisted (archived) path.
// This is the invariant that makes the status poll, the Lost-restore loop, and
// EnsureRootAgents all pass an archived session by; it is also #970-consistent
// (a load never un-archives). Unlike the Lost path, the worktree directory need
// not even exist on disk — inertness means nothing touches it at load.
func TestFromInstanceData_ArchivedLoadsInert(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_archived_agent"
	shellName := agentName + shellTmuxSuffix

	// Inject a spawn-counting exec so we can prove no tmux session is created on
	// load. Because the Archived branch skips Start entirely, no Restore/spawn
	// ever runs — the counter must stay at zero.
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Archived, agentName, shellName)
	data.Prompt = "run the nightly report"
	// Deliberately DO NOT create data.Worktree.WorktreePath: an inert load must
	// not depend on the worktree existing (the Lost path would MkdirAll it).

	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	assert.Equal(t, Archived, restored.GetStatus(), "status round-trips as Archived")
	assert.False(t, restored.Started(), "an archived session loads inert: Start is skipped, started=false")
	assert.False(t, restored.TabAlive(0), "no tmux session is spawned or reconnected on load")
	assert.Equal(t, 0, newSessions, "loading an archived session must never spawn tmux")
	assert.Equal(t, data.Prompt, restored.Prompt, "persisted prompts must restore onto the instance")
	assert.Equal(t, data.Worktree.WorktreePath, restored.GetWorktreePath(),
		"gitWorktree is bound to the persisted archived path so restore knows where the worktree lives")

	// Round-trip: re-serializing preserves Archived + the archived worktree path.
	out := restored.ToInstanceData()
	assert.Equal(t, Archived, out.Status)
	assert.Equal(t, data.Worktree.WorktreePath, out.Worktree.WorktreePath)
}

// TestArchivedInstance_NotRecoverable: Recover refuses an Archived session even
// if something forced it started — Recover is the Lost off-ramp only. This locks
// the boundary between the two states at the backend level (belt-and-suspenders
// alongside the daemon restore loop's ==Lost gate).
func TestArchivedInstance_NotRecoverable(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	inst := &Instance{Title: "arch", liveness: LiveArchived, backend: &LocalBackend{}}
	err := inst.Recover()
	require.Error(t, err, "an archived session is not Lost, so Recover must reject it")
}
