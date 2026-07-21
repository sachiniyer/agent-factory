package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
)

func shiftedMutationTabs(t *testing.T, agentName string) (*Instance, *Tab, *Tab) {
	t.Helper()
	inst := startedMockInstance(t, agentName)
	_, err := inst.AddProcessTab("a", "a")
	require.NoError(t, err)
	b, err := inst.AddProcessTab("b", "b")
	require.NoError(t, err)
	c, err := inst.AddProcessTab("c", "c")
	require.NoError(t, err)

	snapshot := inst.GetTabs()
	resolved, err := ResolveTabIndex(snapshot, b.ID, "", 0)
	require.NoError(t, err)
	require.Equal(t, 2, resolved, "premise: b starts at ordinal 2")

	closed := make(chan error, 1)
	go func() { closed <- inst.CloseTab(1) }()
	require.NoError(t, <-closed)
	requireIndex(t, inst, b.ID, 1)
	requireIndex(t, inst, c.ID, 2)
	return inst, b, c
}

// TestTabMutationsByIDSurviveConcurrentOrdinalShift audits the other
// ResolveTabIndex consumers from #2200. Rename, reorder, and close carry the
// selected stable ID into the Instance mutation; none applies the stale ordinal
// that now names c after a is closed.
func TestTabMutationsByIDSurviveConcurrentOrdinalShift(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	t.Run("rename", func(t *testing.T) {
		inst, b, c := shiftedMutationTabs(t, "af_mutation_id_rename")
		name, err := inst.RenameTabByID(b.ID, "renamed")
		require.NoError(t, err)
		require.Equal(t, "renamed", name)
		requireIndex(t, inst, b.ID, 1)
		require.Equal(t, "renamed", inst.GetTabs()[1].Name)
		require.Equal(t, "c", inst.GetTabs()[2].Name)
		requireIndex(t, inst, c.ID, 2)
	})

	t.Run("reorder", func(t *testing.T) {
		inst, b, c := shiftedMutationTabs(t, "af_mutation_id_reorder")
		require.NoError(t, inst.ReorderTabByID(b.ID, 2))
		requireIndex(t, inst, c.ID, 1)
		requireIndex(t, inst, b.ID, 2)
	})

	t.Run("close", func(t *testing.T) {
		inst, b, c := shiftedMutationTabs(t, "af_mutation_id_close")
		require.NoError(t, inst.CloseTabByID(b.ID))
		_, exists := inst.TabIndexByID(b.ID)
		require.False(t, exists, "the selected b must be closed")
		requireIndex(t, inst, c.ID, 1)
	})
}
