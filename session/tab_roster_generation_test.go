package session

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTabRosterGenerationReconciliation(t *testing.T) {
	for _, change := range []string{"noop", "add", "drop", "replace", "reorder", "rename", "adopt-id", "legacy-row", "legacy-identical"} {
		t.Run(change, func(t *testing.T) {
			inst, _ := newReconcileTestInstance(t, "generation-agent", map[string]bool{"generation-agent": true})
			roster := append(inst.ToInstanceData().Tabs,
				TabData{ID: "web-one", Name: "one", Kind: TabKindWeb, URL: "https://one.example"},
				TabData{ID: "web-two", Name: "two", Kind: TabKindWeb, URL: "https://two.example"})
			_, err := inst.ReconcileTabsFromData(roster)
			require.NoError(t, err)
			original := inst.GetTabs()[1]
			switch change {
			case "add":
				roster = append(roster, TabData{ID: "web-three", Name: "three", Kind: TabKindWeb})
			case "drop":
				roster = roster[:2]
			case "replace":
				roster[1].ID = "replacement"
			case "reorder":
				roster[1], roster[2] = roster[2], roster[1]
			case "rename":
				roster[1].Name = "renamed"
			case "adopt-id":
				original.ID = ""
			case "legacy-row":
				original.ID, roster[1].ID = "", ""
				roster[1].URL = "https://replacement.example"
			case "legacy-identical":
				original.ID, roster[1].ID = "", ""
			}
			before := inst.TabRosterGeneration()
			changed, err := inst.ReconcileTabsFromData(roster)
			require.NoError(t, err)
			if change == "noop" {
				require.False(t, changed)
				require.Equal(t, before, inst.TabRosterGeneration())
			} else {
				require.Greater(t, inst.TabRosterGeneration(), before)
			}
			if change == "legacy-row" || change == "legacy-identical" {
				require.False(t, changed, "the existing pane-facing reconcile result is unchanged")
				require.Same(t, original, inst.GetTabs()[1], "legacy pointer preservation is unchanged")
			}
		})
	}
}
