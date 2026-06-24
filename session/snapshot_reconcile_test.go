package session

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReconcileTestInstance builds a started local instance with a single mock-
// backed agent tab and a worktree, hermetically (no real tmux server). The
// shared persistPtyFactory/nameKeyedExec helpers (tab_persist_test.go) make the
// agent session — and any sibling the reconcile reconnects — report alive so
// Restore reconnects rather than spawning.
func newReconcileTestInstance(t *testing.T, agentName string, alive map[string]bool) (*Instance, string) {
	t.Helper()
	log.Initialize(false)
	t.Cleanup(log.Close)
	// Isolate config reads from the developer's real ~/.agent-factory (see
	// tab_persist_test.go for the full #837 rationale).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	exec := nameKeyedExec(alive)
	pty := persistPtyFactory{t: t, cmdExec: exec}

	worktreePath := filepath.Join(t.TempDir(), "wt")
	gw, err := git.NewGitWorktreeFromStorage(
		"/tmp/snap-reconcile-repo", worktreePath, "snap",
		"snap-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	inst := &Instance{
		Title:       "snap",
		Path:        "/tmp/snap-reconcile-repo",
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs)},
	}
	return inst, worktreePath
}

// TestReconcileTabsFromData_AddsOutOfBandTab is the #959 live-display reconnect:
// the daemon spawned a shell tab out-of-band, so the snapshot's tab list grows;
// ReconcileTabsFromData must add the tab to the live instance and reconnect it to
// its EXACT persisted tmux session by name, leaving it immediately attachable.
func TestReconcileTabsFromData_AddsOutOfBandTab(t *testing.T) {
	const agentName = "af_snap_agent"
	shellName := agentName + shellTmuxSuffix
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true, shellName: true})

	require.Len(t, inst.GetTabs(), 1, "instance starts with only the agent tab")

	target := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	}

	changed, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.True(t, changed, "adding an out-of-band tab is a change")

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "the out-of-band shell tab must appear on the live instance")
	assert.Equal(t, TabKindShell, tabs[1].Kind)
	assert.Equal(t, shellName, tabs[1].tmux.SanitizedName(),
		"the added tab must bind to its EXACT persisted tmux session (reconnect by name)")
	assert.True(t, inst.TabAlive(1), "the reconnected tab must be live (attachable) without a restart")

	// Reconciling the same target again is a no-op (no flicker / repaint).
	changedAgain, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.False(t, changedAgain, "an unchanged snapshot must not report a change")
	assert.Len(t, inst.GetTabs(), 2, "no duplicate tab on a repeat reconcile")
}

// TestReconcileTabsFromData_DropsClosedTab covers the close side: the daemon
// closed a tab out-of-band (it leaves the snapshot), so the live instance must
// drop it locally WITHOUT re-killing the already-gone tmux session.
func TestReconcileTabsFromData_DropsClosedTab(t *testing.T) {
	const agentName = "af_snap_agent2"
	shellName := agentName + shellTmuxSuffix
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true, shellName: true})

	// Add the shell tab first.
	withShell := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	}
	_, err := inst.ReconcileTabsFromData(withShell)
	require.NoError(t, err)
	require.Len(t, inst.GetTabs(), 2)

	// The daemon closed it: the snapshot now lists only the agent tab.
	agentOnly := []TabData{{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName}}
	changed, err := inst.ReconcileTabsFromData(agentOnly)
	require.NoError(t, err)
	assert.True(t, changed, "dropping a closed tab is a change")
	require.Len(t, inst.GetTabs(), 1, "the closed tab must be dropped locally")
	assert.Equal(t, TabKindAgent, inst.GetTabs()[0].Kind, "the agent tab is never dropped")
}

// TestReconcileTabsFromData_NotStartedIsNoOp guards the not-started branch: an
// unstarted instance (e.g. a Loading placeholder) must never attempt a reconnect.
func TestReconcileTabsFromData_NotStartedIsNoOp(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "pending", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	changed, rerr := inst.ReconcileTabsFromData([]TabData{
		{Name: "agent", Kind: TabKindAgent},
		{Name: "shell", Kind: TabKindShell, TmuxName: "whatever__shell"},
	})
	require.NoError(t, rerr)
	assert.False(t, changed, "a not-started instance reconcile is a no-op")
}
