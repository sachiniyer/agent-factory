package app

import (
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTUIViewStateRestorePanesSelectionAndFocus(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false
	alpha := tuiStateTestInstance(t, "alpha")
	beta := tuiStateTestInstance(t, "beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)

	state := config.TUIRepoViewState{
		Selected: &config.TUIStateTarget{
			InstanceID: beta.ID,
			Title:      beta.Title,
			TabName:    "shell",
		},
		ActiveTab: &config.TUIStateTarget{
			InstanceID: beta.ID,
			Title:      beta.Title,
			TabName:    "shell",
		},
		Focus: &config.TUIStateFocus{
			Region:  tuiFocusRegionPane,
			PaneKey: tuiPaneKeyForInstance(beta, "shell"),
		},
		OpenPanes: []config.TUIStateOpenPane{
			{
				Key:        tuiPaneKeyForInstance(alpha, "agent"),
				InstanceID: alpha.ID,
				Title:      alpha.Title,
				TabName:    "agent",
				FocusRank:  1,
			},
			{
				Key:        tuiPaneKeyForInstance(beta, "shell"),
				InstanceID: beta.ID,
				Title:      beta.Title,
				TabName:    "shell",
				FocusRank:  2,
			},
		},
	}
	require.NoError(t, config.SaveTUIRepoViewState(h.repoID, state))

	require.Equal(t, 2, h.restoreTUIViewStateOnLaunch())
	resizeHome(h, 200, 40)

	require.Equal(t, 2, h.store.NumOpenPanes())
	assert.Equal(t, []string{"alpha", "beta"}, visibleTitles(h))
	assert.Equal(t, 2, h.lastLayout.PaneCount(), "restored panes are laid out side by side")
	assert.True(t, h.initialPaneOpened, "restored panes suppress startup auto-open")

	require.Same(t, beta, h.store.GetSelectedInstance())
	assert.Equal(t, 1, h.store.ActiveTab(), "active tab restores by tab name")

	focused := h.focusedOpenPane()
	require.NotNil(t, focused)
	assert.Same(t, beta, focused.Instance())
	assert.Equal(t, 1, focused.Tab())
}

func TestTUIViewStateNoValidPanesKeepsAutoOpenFallback(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false
	alpha := tuiStateTestInstance(t, "alpha")
	h.store.AddInstance(alpha)

	state := config.TUIRepoViewState{
		OpenPanes: []config.TUIStateOpenPane{{
			Key:        "title:missing:tab:agent",
			InstanceID: "missing-id",
			Title:      "missing",
			TabName:    "agent",
			FocusRank:  1,
		}},
	}
	require.NoError(t, config.SaveTUIRepoViewState(h.repoID, state))

	require.Zero(t, h.restoreTUIViewStateOnLaunch())
	require.False(t, h.initialPaneOpened)

	_ = h.selectionChanged()
	require.Equal(t, 1, h.store.NumOpenPanes(), "ordinary cold-start auto-open still runs")
	assert.Same(t, alpha, h.store.OpenPanes()[0].Instance())
}

func TestTUIViewStatePreviewTickDoesNotWriteUnchangedState(t *testing.T) {
	h := newTestHome(t)
	alpha := tuiStateTestInstance(t, "alpha")
	h.store.AddInstance(alpha)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)
	h.rememberTUIViewState()

	path, err := config.TUIStatePath()
	require.NoError(t, err)
	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "baseline snapshot must not write by itself")

	_, _ = h.Update(previewTickMsg{})
	_, statErr = os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "unchanged preview tick must not write TUI state")

	h.keySent = true
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "a structural pane-open action must save TUI state")
	assert.Contains(t, string(raw), `"schema_version"`)
	assert.Contains(t, string(raw), `"open_panes"`)
}

func tuiStateTestInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst := instanceWithFakeBackend(t, title)
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddTabForTest("shell", session.TabKindShell)
	return inst
}
