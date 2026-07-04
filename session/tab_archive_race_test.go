package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flipStatus sets Status under the same lock the daemon archive op uses, so the
// pre-spawn guard and the post-spawn recheck in AddShellTab/AddProcessTab observe
// an archiving instance — the deterministic stand-in for a concurrent
// ArchiveSession flipping status Deleting→Archived while started stays true
// (#1195).
func flipStatus(i *Instance, s Status) {
	i.mu.Lock()
	i.Status = s
	i.mu.Unlock()
}

// TestAddShellTab_ArchivedInstanceRejected: adding a shell tab to an Archived
// instance is refused UP FRONT (before any tmux new-session) with an actionable
// message. ArchiveTeardown deliberately keeps started=true (so a failed move can
// self-heal to Lost via the #1108 rollback), which means the #990 started guard
// never fires for an archive — the status guard is what stops a tab spawning into
// the moved-away worktree (#1195).
func TestAddShellTab_ArchivedInstanceRejected(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_shell_archived"
	var inst *Instance
	spawned := false
	inst, isAlive := raceMockInstance(t, agentName, func() { spawned = true })
	// started stays true; only the status flips — exactly ArchiveTeardown's shape.
	flipStatus(inst, Archived)

	tab, err := inst.AddShellTab()
	require.Error(t, err, "a shell tab on an archived session must be refused")
	assert.Contains(t, err.Error(), "archived")
	assert.Nil(t, tab)
	assert.False(t, spawned, "the guard must reject before spawning any tmux session")
	assert.False(t, isAlive(agentName+"__"+shellTabName), "no shell tmux session may exist")
	assert.Equal(t, 1, inst.TabCount(), "no tab may be appended")
}

// TestAddProcessTab_ArchivedInstanceRejected is the AddProcessTab counterpart:
// a process tab on an Archived instance is refused up front, spawning nothing.
func TestAddProcessTab_ArchivedInstanceRejected(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_proc_archived"
	var inst *Instance
	spawned := false
	inst, isAlive := raceMockInstance(t, agentName, func() { spawned = true })
	flipStatus(inst, Archived)

	tab, err := inst.AddProcessTab("btop", "")
	require.Error(t, err, "a process tab on an archived session must be refused")
	assert.Contains(t, err.Error(), "archived")
	assert.Nil(t, tab)
	assert.False(t, spawned, "the guard must reject before spawning any tmux session")
	assert.False(t, isAlive(agentName+"__btop"), "no process tmux session may exist")
	assert.Equal(t, 1, inst.TabCount(), "no tab may be appended")
}

// TestAddShellTab_ArchiveRaceDoesNotLeakSession is the post-spawn backstop for
// the #1195 archive-orphan class: if the session begins archiving (status flipped
// to Deleting, started left true, mirroring ArchiveTeardown) in the window after
// the shell tab's tmux session is spawned but before it is appended, AddShellTab
// must NOT append the tab, MUST tear down the spawned session (no orphan
// referencing the worktree being moved), and MUST return an error.
func TestAddShellTab_ArchiveRaceDoesNotLeakSession(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_shell_archrace"
	var inst *Instance
	var isAlive func(string) bool
	inst, isAlive = raceMockInstance(t, agentName, func() { flipStatus(inst, Deleting) })

	tab, err := inst.AddShellTab()
	require.Error(t, err, "a tab created during archive teardown must be refused")
	assert.Nil(t, tab)
	assert.Equal(t, 1, inst.TabCount(), "the raced tab must not be appended")
	assert.False(t, isAlive(agentName+"__"+shellTabName), "the spawned tmux session must be torn down (no orphan)")
}

// TestAddProcessTab_ArchiveRaceDoesNotLeakSession is the AddProcessTab
// counterpart of the archive-race backstop (#1195).
func TestAddProcessTab_ArchiveRaceDoesNotLeakSession(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_proc_archrace"
	var inst *Instance
	var isAlive func(string) bool
	inst, isAlive = raceMockInstance(t, agentName, func() { flipStatus(inst, Deleting) })

	tab, err := inst.AddProcessTab("btop", "")
	require.Error(t, err, "a process tab created during archive teardown must be refused")
	assert.Nil(t, tab)
	assert.Equal(t, 1, inst.TabCount(), "the raced tab must not be appended")
	assert.False(t, isAlive(agentName+"__btop"), "the spawned tmux session must be torn down (no orphan)")
}
