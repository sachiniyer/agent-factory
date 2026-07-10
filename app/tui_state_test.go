package app

import (
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
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

// TestTUIViewStateRestoredPanesAutoHideShowsStatus is the #1535 repro: panes
// restored from persisted TUI state are laid out for the first time on the
// first real WindowSizeMsg (the restore-time relayout runs at (0,0) and falls
// through to fallback with no visible panes). When that first size is too
// narrow to fit every restored pane, the "N hidden: terminal too narrow" status
// must still surface — pre-fix newlyAutoHiddenPane saw an empty previousVisible
// and returned nil, so the user got a silently missing pane with no explanation.
func TestTUIViewStateRestoredPanesAutoHideShowsStatus(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false
	alpha := tuiStateTestInstance(t, "alpha")
	beta := tuiStateTestInstance(t, "beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)

	state := config.TUIRepoViewState{
		OpenPanes: []config.TUIStateOpenPane{
			{
				Key:        tuiPaneKeyForInstance(alpha, "agent"),
				InstanceID: alpha.ID,
				Title:      alpha.Title,
				TabName:    "agent",
				FocusRank:  1,
			},
			{
				Key:        tuiPaneKeyForInstance(beta, "agent"),
				InstanceID: beta.ID,
				Title:      beta.Title,
				TabName:    "agent",
				FocusRank:  2,
			},
		},
	}
	require.NoError(t, config.SaveTUIRepoViewState(h.repoID, state))

	// Restore opens both panes; the restore-time relayout runs at (0,0) →
	// fallback → visiblePanes stays nil.
	require.Equal(t, 2, h.restoreTUIViewStateOnLaunch())
	require.Empty(t, h.visiblePanes, "the restore-time relayout falls through at (0,0)")
	require.Len(t, h.restoredPaneBaseline, 2, "restore seeds the auto-hide baseline")

	// A snapshot-driven relayout can land between restore and the first
	// WindowSizeMsg (sync.go relays out on reconcile). It must NOT consume the
	// baseline out from under the pending resize (#1551): the baseline survives
	// until the first sized relayout actually uses it.
	h.relayout()
	require.Len(t, h.restoredPaneBaseline, 2,
		"an intermediate no-dims relayout must not clear the restore baseline")

	// First real relayout at a width that fits only one pane: the other restored
	// pane auto-hides and the status must surface.
	resizeHome(h, layout.MultiPaneMinWidth-1, 40)

	require.Equal(t, 1, h.lastLayout.PaneCount())
	require.Equal(t, 2, h.store.NumOpenPanes(), "auto-hide retains both bindings")
	assert.Empty(t, h.restoredPaneBaseline, "the sized relayout consumes the baseline")
	assert.Contains(t, h.errBox.FullError(), "hidden: terminal too narrow for 2 panes",
		"a pane hidden on the first post-restore relayout must still surface the status (#1535)")
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
