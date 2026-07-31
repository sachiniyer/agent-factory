package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #2669 round-2 review finding. A startup-only retry policy leaves a window
// the daemon never reopens: an unconfirmed CloseTab followed by KillSession or
// archive BEFORE the next restart. teardownTabs enumerated only i.Tabs, so the
// pending tmux session escaped session-wide teardown entirely — archive would
// relocate the worktree while that process was still cwd'd in it, and a kill
// would delete the record, and with it the handle that was the session's only
// remaining pointer, while the process ran on.
//
// The fix routes pending handles through the SAME closeTab the live tabs use, so
// they inherit the pane-exit wait and the unknown-blocks-the-worktree gate, and
// retires them only when that gate confirms them dead.

// pendingTeardownInstance is a started local instance with one agent tab and the
// given unconfirmed tab-cleanup handles.
func pendingTeardownInstance(t *testing.T, handles ...TabCleanupData) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "reap", Path: "/tmp/reap-repo", Program: "claude"})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedName("af_reap_agent", "claude"))
	inst.SetStartedForTest(true)
	inst.SetPendingTabCleanupForTest(handles)
	return inst
}

// TestTeardownTabs_DestructiveModeTearsDownPendingHandles is the headline: a
// destructive teardown must present the pending session to closeTab, exactly
// like a live tab, so the same #802 pane-exit ordering runs before the worktree
// is moved or deleted.
func TestTeardownTabs_DestructiveModeTearsDownPendingHandles(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedName(name, program)
	}
	defer func() { restoreTmuxSession = prev }()

	const leaked = "af_reap_agent__btop"
	inst := pendingTeardownInstance(t, TabCleanupData{TabID: "closed-1", TmuxName: leaked})
	mode := &gateStubMode{
		closeState: stateKnown, worktreeState: stateKnown, reapPendingFlag: true,
	}

	require.NoError(t, inst.teardownTabs(mode))

	assert.Contains(t, mode.closedNames, leaked,
		"a destructive teardown skipped the pending tmux session; archive would move the worktree "+
			"out from under it and kill would delete the record that is its only handle")
	assert.True(t, mode.worktreeCalled, "every pane was confirmed dead, so the worktree step must run")
	assert.Empty(t, inst.PendingTabCleanup(), "a confirmed teardown must retire the handle it reaped")
}

// TestTeardownTabs_UnconfirmedPendingKillBlocksWorktreeAndKeepsRecord is the
// safety half. A pending session whose kill cannot be confirmed must stop the
// destructive step and report ErrPaneMayBeLive, so the caller RETAINS the record
// instead of deleting the handle while the process may still be running.
func TestTeardownTabs_UnconfirmedPendingKillBlocksWorktreeAndKeepsRecord(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedName(name, program)
	}
	defer func() { restoreTmuxSession = prev }()

	const leaked = "af_reap_agent__btop"
	inst := pendingTeardownInstance(t, TabCleanupData{TabID: "closed-1", TmuxName: leaked})
	// ONLY the pending session is unknown; the live agent tab answers cleanly. So a
	// blocked worktree step is attributable to the pending handle alone, not to any
	// tab in the pass.
	mode := &gateStubMode{
		closeState: stateKnown, worktreeState: stateKnown, reapPendingFlag: true,
		stateByName: map[string]teardownState{leaked: stateUnknown},
	}

	err := inst.teardownTabs(mode)
	require.Error(t, err)
	assert.True(t, TeardownStateUnknown(err),
		"an unconfirmed pending kill must report an unknown so the caller RETAINS the record; "+
			"deleting it would strand the tmux session with nothing naming it")
	assert.False(t, mode.worktreeCalled,
		"the worktree must not be moved or deleted while a pending pane may still be running in it")
	assert.False(t, mode.finalizeCalled, "finalize would clear the state a retry needs")
	assert.Equal(t, []TabCleanupData{{TabID: "closed-1", TmuxName: leaked}}, inst.PendingTabCleanup(),
		"an unconfirmed kill must keep its handle for the retry")
}

// TestTeardownTabs_ReleasePTYLeavesPendingHandlesAlone guards the non-destructive
// mode. Release-PTY runs no kill-session at all, so it can never confirm a
// pending session dead — retiring handles there would drop a durable pointer on
// the strength of a teardown that killed nothing, which is the exact
// unknown-read-as-finished mistake this whole mechanism exists to prevent. It
// also shares its tmux with the canonical instance (#867), which still needs them.
func TestTeardownTabs_ReleasePTYLeavesPendingHandlesAlone(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const leaked = "af_reap_agent__btop"
	handle := TabCleanupData{TabID: "closed-1", TmuxName: leaked}
	inst := pendingTeardownInstance(t, handle)
	mode := &gateStubMode{
		closeState: stateKnown, worktreeState: stateKnown, reapPendingFlag: false,
	}

	require.NoError(t, inst.teardownTabs(mode))

	assert.NotContains(t, mode.closedNames, leaked,
		"release-PTY must not touch a pending session: it shares tmux with the canonical instance")
	assert.Equal(t, []TabCleanupData{handle}, inst.PendingTabCleanup(),
		"release-PTY killed nothing, so it must not retire a handle")
}

// TestTeardownModes_DeclarePendingReapByDestructiveness pins the policy itself.
// The two modes that touch the workspace reap; the one that does not, does not.
// A future mode has to make this choice explicitly rather than inherit it.
func TestTeardownModes_DeclarePendingReapByDestructiveness(t *testing.T) {
	assert.True(t, teardownKill{}.reapsPendingTabCleanup(),
		"kill deletes the record, which is the handle's only home")
	assert.True(t, teardownArchive{}.reapsPendingTabCleanup(),
		"archive moves the worktree a pending process is still cwd'd in")
	assert.False(t, teardownReleasePTY{}.reapsPendingTabCleanup(),
		"release-PTY runs no kill-session, so it can confirm nothing dead")
}
