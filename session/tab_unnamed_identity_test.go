package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconcileTabsFromDataKeepsUnnamedMetadataOrderByID(t *testing.T) {
	const agentName = "af_snap_unnamed_metadata"
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true})

	first, err := inst.AddVSCodeTab("")
	require.NoError(t, err)
	second, err := inst.AddVSCodeTab("")
	require.NoError(t, err)
	require.Empty(t, first.Name)
	require.Empty(t, second.Name)

	agent := inst.GetTabs()[0]
	target := []TabData{
		{ID: agent.ID, Name: agent.Name, Kind: TabKindAgent, TmuxName: agentName},
		{ID: first.ID, Kind: TabKindVSCode},
		{ID: second.ID, Kind: TabKindVSCode},
	}
	changed, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	require.False(t, changed, "an unchanged stable-ID order must not be reversed by duplicate empty names")

	tabs := inst.GetTabs()
	require.Equal(t, []string{agent.ID, first.ID, second.ID}, []string{tabs[0].ID, tabs[1].ID, tabs[2].ID})
}
