package app

import (
	"encoding/base64"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/tree"
	"github.com/stretchr/testify/require"
)

func TestDeleteTabConsent(t *testing.T) {
	for _, kind := range []session.TabKind{session.TabKindShell, session.TabKindProcess, session.TabKindVSCode, session.TabKindWeb} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			h, inst := multiTabHome(t)
			inst.GetTabs()[2].Kind = kind
			switch kind {
			case session.TabKindWeb:
				inst.GetTabs()[2].Name = "web"
			case session.TabKindVSCode:
				inst.GetTabs()[2].Name = "vscode"
			}
			h.store.SetActiveTab(2)
			calls := recordCloseTab(t, h)
			_, _ = h.handleCloseTab()
			require.Empty(t, *calls, "w must ask before deleting the tab")
			require.Equal(t, stateConfirm, h.state)
			label, ok := tree.TabLabelAt(inst, 2)
			require.True(t, ok)
			require.Contains(t, h.confirmationOverlay.Render(), fmt.Sprintf("Delete tab %q", label))
			require.Contains(t, h.confirmationOverlay.Render(), "Hiding a pane")
			_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
			require.Empty(t, *calls, "repeating w is not consent")
			_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyEsc})
			require.Empty(t, *calls)
			require.Equal(t, 4, inst.TabCount())
		})
	}
}

func TestDeleteTabConsentKeepsIdentity(t *testing.T) {
	h, inst := multiTabHome(t)
	h.store.SetActiveTab(2)
	calls := recordCloseTab(t, h)
	_, _ = h.handleCloseTab()
	require.Empty(t, *calls)
	require.NotNil(t, h.confirmationOverlay)
	require.NoError(t, inst.ReorderTabByID("id-shell2", 3))
	h.store.SetActiveTab(1)
	_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	require.Equal(t, []string{"shell-2"}, *calls)
}

func TestDeleteTabConsentEvidence(t *testing.T) {
	configureDesignStillsOutput(t)
	for _, mode := range []string{"light", "dark"} {
		h, inst := newDesignDriverSceneHome(t, mode, nil)
		inst.AddTabForTest("agent", session.TabKindAgent)
		inst.AddTabForTest("build logs", session.TabKindShell)
		h.store.SetActiveTab(1)
		h.sidebar.SyncCursorToActiveTab()
		_, _ = h.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		recordCloseTab(t, h)
		_, _ = h.handleCloseTab()
		t.Logf("DELETE_TAB_%s_SVG=%s", mode, base64.StdEncoding.EncodeToString([]byte(recoverySVG(h.View(), mode, 80, 24))))
	}
}

// Existing reconciliation tests exercise the mutation after deliberate consent.
func confirmTabDeletionForTest(h *home) {
	if h.confirmationOverlay != nil {
		_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	}
}

func TestDeleteTabConsentRejectsReplacement(t *testing.T) {
	for _, stableID := range []bool{true, false} {
		t.Run(fmt.Sprint(stableID), func(t *testing.T) {
			h, inst := multiTabHome(t)
			if !stableID {
				inst.GetTabs()[2].ID = ""
			}
			h.store.SetActiveTab(2)
			calls := recordCloseTab(t, h)
			_, _ = h.handleCloseTab()
			require.NotNil(t, h.confirmationOverlay)
			require.NoError(t, inst.DropClosedTab(2))
			inst.AddTabForTest("shell-2", session.TabKindShell)
			_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			require.Empty(t, *calls, "a same-name replacement is not the confirmed tab")
		})
	}
}

func TestDeleteTabConsentReportsRefusal(t *testing.T) {
	for _, reason := range []string{"vanished", "busy"} {
		t.Run(reason, func(t *testing.T) {
			h, inst := multiTabHome(t)
			h.store.SetActiveTab(2)
			calls := recordCloseTab(t, h)
			_, _ = h.handleCloseTab()
			if reason == "vanished" {
				require.NoError(t, inst.DropClosedTab(2))
			} else {
				inst.SetInFlightOpForTest(session.OpRestoring)
			}
			_, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			require.Empty(t, *calls, "refused confirmation must not mutate a tab")
			require.Equal(t, stateDefault, h.state)
			require.Nil(t, h.confirmationOverlay)
			require.NotNil(t, cmd, "schedule the refusal notice expiry through the loop")
			if reason == "vanished" {
				require.Contains(t, h.errBox.String(), "is no longer available")
				require.Contains(t, h.errBox.String(), "Terminal")
			} else {
				require.Contains(t, h.errBox.String(), "is busy; try again")
			}
		})
	}
}
