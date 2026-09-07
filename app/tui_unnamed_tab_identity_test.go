package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

func TestTUIViewStateKeepsUnnamedMetadataPanesDistinctByID(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "unnamed-tabs")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("", "http://localhost:3000")
	inst.AddWebTabForTest("", "http://localhost:3001")
	h.store.AddInstance(inst)

	require.NotNil(t, h.openPaneWindow(inst, 1))
	require.NotNil(t, h.openPaneWindow(inst, 2))
	state := h.captureTUIViewState()
	require.Len(t, state.OpenPanes, 2)
	require.NotEqual(t, state.OpenPanes[0].Key, state.OpenPanes[1].Key,
		"stable tab IDs must keep duplicate empty names from collapsing to one pane key")

	restored := newTestHome(t)
	restored.store.AddInstance(inst)
	require.Equal(t, 2, restored.applyTUIViewState(state))
	require.Len(t, restored.store.OpenPanes(), 2)
}

func TestReplacementResolverPrefersCarriedIDForUnnamedTabs(t *testing.T) {
	inst := instanceWithFakeBackend(t, "unnamed-replacement")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("", "http://localhost:3000")
	inst.AddWebTabForTest("", "http://localhost:3001")
	tabs := inst.GetTabs()

	idx, ok := newTabSlotResolver(inst, replacedSessionTabs).resolve(tabSlotKey{
		id: tabs[1].ID, name: tabs[1].Name,
	})
	require.True(t, ok)
	require.Equal(t, 1, idx, "a carried stable ID must win over an ambiguous empty replacement name")
}
