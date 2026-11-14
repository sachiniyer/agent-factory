package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// TestTUIViewStateRelaunchShowsTheRestoredCursorsSession is the #7293 play-test
// repro. Before the quit, alpha's pane previews beta under the tree cursor. The
// driver's quit presses Esc, which dismisses that preview and focuses alpha's
// pane, then q. So the saved state is: alpha's pane focused, the cursor on beta.
//
// On relaunch the cursor came back on beta and the preview tick painted beta
// over alpha's FOCUSED pane. The driver then pressed Tab to reach the tree, and
// Tab dismisses a preview as the user's own choice and suppresses it. The
// workspace was left on alpha beside `— cursor: beta`, until the first cursor
// move.
//
// The relaunch must show the session under the restored cursor, and reaching
// the tree must not undo that. The cursor never moves to match the display.
func TestTUIViewStateRelaunchShowsTheRestoredCursorsSession(t *testing.T) {
	before := newTestHome(t)
	before.initialPaneOpened = false
	before.store.AddInstance(tuiStateTestInstance(t, "alpha"))
	before.store.AddInstance(tuiStateTestInstance(t, "beta"))
	before.sidebar.SetSelectedInstance(0)
	_ = before.selectionChanged() // the startup auto-open puts alpha in the first pane
	resizeHome(before, 100, 30)
	before.sidebar.SetSelectedInstance(1)
	_ = before.selectionChanged()
	require.Contains(t, before.View(), "beta · Agent · preview", "premise: alpha's pane previews beta")
	_, _ = before.Update(tea.KeyMsg{Type: tea.KeyEsc}) // af_quit's Esc, before its q
	require.Contains(t, before.View(), "alpha · Agent — cursor: beta · Agent",
		"premise: Esc dismissed the preview before the quit")
	saved := before.captureTUIViewState()
	require.NotNil(t, saved.Focus)
	require.Equal(t, tuiFocusRegionPane, saved.Focus.Region, "premise: the quit saved focus on alpha's pane")

	h := newTestHome(t)
	h.initialPaneOpened = false
	h.store.AddInstance(tuiStateTestInstance(t, "alpha"))
	beta := tuiStateTestInstance(t, "beta")
	h.store.AddInstance(beta)
	require.NoError(t, config.SaveTUIRepoViewState(h.repoID, saved))
	require.Equal(t, 1, h.restoreTUIViewStateOnLaunch())
	resizeHome(h, 100, 30)
	_, _ = h.Update(previewTickMsg{})
	requireWorkspaceShowsCursor(t, h, "beta · Agent · preview", "before any keypress")

	// af_relaunch's af_focus_tree: Tab until the tree has focus, as a user would
	// to move the cursor.
	for i := 0; i < 4 && h.ring.Active() != layout.RegionTree; i++ {
		pressTab(t, h, false)
		_, _ = h.Update(previewTickMsg{})
	}
	require.Equal(t, layout.RegionTree, h.ring.Active())
	requireWorkspaceShowsCursor(t, h, "beta · Agent · preview", "once the tree has focus")
	assert.Same(t, beta, h.sidebar.GetSelectedInstance(), "the cursor stays where it was restored")
}

// TestTUIViewStateRelaunchBringsTheCursorsOwnPaneForward covers the other split
// a relaunch can restore (#7293). The cursor's tab has a pane of its own, but
// the pane fitting keeps the most recently focused panes, and only one fits.
// That was alpha's, so beta's pane stayed hidden: nothing painted the cursor's
// session, and a background tick never brings a hidden pane forward.
func TestTUIViewStateRelaunchBringsTheCursorsOwnPaneForward(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false
	alpha := tuiStateTestInstance(t, "alpha")
	beta := tuiStateTestInstance(t, "beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)
	agent := func(title, id string, rank uint64) config.TUIStateOpenPane {
		return config.TUIStateOpenPane{
			Key:        tuiPaneKey(id, title, "agent"),
			InstanceID: id,
			Title:      title,
			TabName:    "agent",
			FocusRank:  rank,
		}
	}
	betaAgent := &config.TUIStateTarget{InstanceID: beta.ID, Title: beta.Title, TabName: "agent"}
	require.NoError(t, config.SaveTUIRepoViewState(h.repoID, config.TUIRepoViewState{
		Selected:  betaAgent,
		ActiveTab: betaAgent,
		Focus: &config.TUIStateFocus{
			Region:  tuiFocusRegionPane,
			PaneKey: tuiPaneKey(alpha.ID, alpha.Title, "agent"),
		},
		OpenPanes: []config.TUIStateOpenPane{
			agent(alpha.Title, alpha.ID, 2), // focused last, so the fitting keeps it
			agent(beta.Title, beta.ID, 1),
		},
	}))

	require.Equal(t, 2, h.restoreTUIViewStateOnLaunch())
	resizeHome(h, 100, 30)
	require.Equal(t, 1, h.lastLayout.PaneCount(), "premise: one pane fits")
	_, _ = h.Update(previewTickMsg{})

	assert.Equal(t, []string{"beta"}, visibleTitles(h), "the cursor's own pane is the one shown")
	requireWorkspaceShowsCursor(t, h, "beta · Agent", "before any keypress")
	assert.Same(t, beta, h.sidebar.GetSelectedInstance(), "the cursor stays where it was restored")
}

// requireWorkspaceShowsCursor asserts the #7293 property: the workspace shows
// the session under the tree cursor, so no pane header carries the `— cursor:`
// clause that discloses a split.
func requireWorkspaceShowsCursor(t *testing.T, h *home, header, when string) {
	t.Helper()
	view := h.View()
	require.Contains(t, view, header, "%s: the workspace shows the cursor's session", when)
	require.NotContains(t, view, "— cursor:", "%s: the shown session and the cursor agree", when)
}
