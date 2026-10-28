package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDeleteTabConsentLegacySnapshotGeneration(t *testing.T) {
	for _, snapshot := range []string{"none", "changed-row", "identical-row"} {
		t.Run(snapshot, func(t *testing.T) {
			h := newTestHome(t)
			inst := startedLocalInstance(t, "legacy-consent")
			selectInstance(h, inst)
			resizeHome(h, 200, 40)
			tab := inst.GetTabs()[1]
			tab.ID = ""
			h.store.SetActiveTab(1)
			calls := recordCloseTab(t, h)
			_, _ = h.handleCloseTab()
			require.NotNil(t, h.confirmationOverlay)
			if snapshot != "none" {
				data := inst.ToInstanceData()
				if snapshot == "changed-row" {
					data.Tabs[1].TmuxName += "-replacement"
				}
				h.updateInstanceFromSnapshot(inst, data)
				require.Same(t, tab, inst.GetTabs()[1], "legacy reconciliation deliberately preserves the pointer")
			}
			_, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			if snapshot == "none" {
				require.Equal(t, []string{tab.Name}, *calls, "unchanged legacy roster still accepts consent")
			} else {
				require.Empty(t, *calls, "a same-name legacy snapshot cannot prove continuity")
				require.NotNil(t, cmd)
				require.Contains(t, h.errBox.String(), "changed while the dialog was open; reopen it and try again")
			}
		})
	}
}

func TestDeleteTabConsentStableIDSurvivesRosterGeneration(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "stable-consent")
	selectInstance(h, inst)
	resizeHome(h, 200, 40)
	h.store.SetActiveTab(1)
	calls := recordCloseTab(t, h)
	_, _ = h.handleCloseTab()
	before := inst.TabRosterGeneration()
	data := inst.ToInstanceData()
	data.Tabs = append(data.Tabs, session.TabData{ID: "new-web", Name: "web", Kind: session.TabKindWeb})
	h.updateInstanceFromSnapshot(inst, data)
	require.Greater(t, inst.TabRosterGeneration(), before)
	_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	require.Equal(t, []string{"shell"}, *calls, "stable-ID consent survives unrelated roster changes")
}
