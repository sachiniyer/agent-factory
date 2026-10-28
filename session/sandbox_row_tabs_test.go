package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func sandboxRowData(liveness Liveness, tabs ...TabData) InstanceData {
	return InstanceData{
		Title:       "sbx",
		Path:        "/repo",
		Branch:      "sbx",
		Program:     "claude",
		BackendType: "docker",
		Liveness:    liveness,
		Tabs:        tabs,
	}
}

// W4.6: a sandbox row built from a snapshot has no tmux or worktree, yet the
// snapshot's roster must still reach it and follow out-of-band edits.
func TestReconcileTabsFromData_ProjectsSandboxRoster(t *testing.T) {
	inst, err := FromInstanceData(sandboxRowData(LiveReady))
	require.NoError(t, err)
	require.Empty(t, inst.GetTabs())

	roster := []TabData{
		{ID: "a1", Name: "agent", Kind: TabKindAgent},
		{ID: "t1", Name: "docs", Kind: TabKindWeb, URL: "https://example.com"},
		{ID: "t2", Name: "logs", Kind: TabKindProcess, Command: "tail -f x"},
	}
	changed, err := inst.ReconcileTabsFromData(roster)
	require.NoError(t, err)
	require.True(t, changed)
	tabs := inst.GetTabs()
	require.Len(t, tabs, 3)
	require.Equal(t, "a1", tabs[0].ID)
	require.Equal(t, TabKindAgent, tabs[0].Kind)

	// Idempotent: an identical snapshot is not a change and keeps pointers.
	changed, err = inst.ReconcileTabsFromData(roster)
	require.NoError(t, err)
	require.False(t, changed)
	require.Same(t, tabs[1], inst.GetTabs()[1])

	// Out-of-band reorder + close + rename.
	next := []TabData{roster[0], {ID: "t2", Name: "tail", Kind: TabKindProcess, Command: "tail -f x"}}
	changed, err = inst.ReconcileTabsFromData(next)
	require.NoError(t, err)
	require.True(t, changed)
	tabs = inst.GetTabs()
	require.Len(t, tabs, 2)
	require.Equal(t, "tail", tabs[1].Name)
	require.True(t, inst.TabRosterMutable())
}

// W4.7: a Lost sandbox row publishes its persisted roster, agent first.
func TestToInstanceData_LostSandboxRowPublishesRoster(t *testing.T) {
	inst, err := FromInstanceData(sandboxRowData(LiveLost,
		TabData{ID: "a1", Name: "agent", Kind: TabKindAgent},
		TabData{ID: "w1", Name: "docs", Kind: TabKindWeb, URL: "https://example.com"}))
	require.NoError(t, err)
	d := inst.ToInstanceData()
	require.Len(t, d.Tabs, 2)
	require.Equal(t, TabKindAgent, d.Tabs[0].Kind)
	require.Equal(t, "docs", d.Tabs[1].Name)
}

// A byte-identical id-less row cannot distinguish "unchanged" from close +
// recreate under one name, so a delete confirmation armed before the snapshot
// must refuse afterwards even though the roster looks identical.
func TestReconcileTabsFromData_IdlessRowAdvancesGeneration(t *testing.T) {
	inst, err := FromInstanceData(sandboxRowData(LiveReady))
	require.NoError(t, err)
	roster := []TabData{
		{ID: "a1", Name: "agent", Kind: TabKindAgent},
		{Name: "logs", Kind: TabKindProcess, Command: "tail -f x"},
	}
	_, err = inst.ReconcileTabsFromData(roster)
	require.NoError(t, err)
	armed := inst.TabRosterGeneration()

	changed, err := inst.ReconcileTabsFromData(roster)
	require.NoError(t, err)
	require.False(t, changed, "pane-facing result stays unchanged")
	require.Greater(t, inst.TabRosterGeneration(), armed)

	// Control: an all-id roster does not churn the generation.
	idd := []TabData{roster[0], {ID: "t1", Name: "logs", Kind: TabKindProcess, Command: "tail -f x"}}
	_, err = inst.ReconcileTabsFromData(idd)
	require.NoError(t, err)
	g := inst.TabRosterGeneration()
	_, err = inst.ReconcileTabsFromData(idd)
	require.NoError(t, err)
	require.Equal(t, g, inst.TabRosterGeneration())
}
