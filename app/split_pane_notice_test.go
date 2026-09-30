package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// pressSplitPane drives `S` through the default key dispatch and returns the
// command it scheduled.
func pressSplitPane(h *home) tea.Cmd {
	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}, keys.KeySplitPane)
	return cmd
}

// TestSplitPaneWithoutPreviewSaysThePaneIsOpen: `S` on a selection whose tab
// already has a pane has no preview to keep. It must say so instead of
// silently doing nothing (#4957), and it must still change nothing.
func TestSplitPaneWithoutPreviewSaysThePaneIsOpen(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes())
	require.Nil(t, h.panePreviewTxn, "the selected tab's own pane is open, so nothing is previewed")
	before := visibleTitles(h)
	active := h.ring.Active()

	cmd := pressSplitPane(h)

	assert.NotNil(t, cmd, "the notice schedules its auto-clear")
	assert.Contains(t, h.errBox.String(), "The pane is already open")
	assert.Equal(t, before, visibleTitles(h), "a dead S must not change the workspace")
	assert.Equal(t, active, h.ring.Active(), "a dead S must not move focus")
	assert.Nil(t, h.panePreviewTxn)
}

// TestSplitPaneWithNoPanesSaysThereIsNoPreview: with no pane open there is no
// owner to preview into, so the notice cannot claim a pane is open.
func TestSplitPaneWithNoPanesSaysThereIsNoPreview(t *testing.T) {
	h := paneTestHome(t)
	require.Equal(t, 0, h.store.NumOpenPanes())
	require.Nil(t, h.panePreviewTxn)

	cmd := pressSplitPane(h)

	assert.NotNil(t, cmd, "the notice schedules its auto-clear")
	status := h.errBox.String()
	assert.Contains(t, status, "No preview to keep")
	assert.NotContains(t, status, "The pane is already open")
	assert.Equal(t, 0, h.store.NumOpenPanes(), "a dead S must not open a pane")
}

// TestSplitPaneWithPreviewStillCommitsSilently: the #4957 notice is only for
// the no-preview case. A real preview commits alongside exactly as before —
// the owner keeps its binding, the target gains a focused pane — and no notice
// is raised.
func TestSplitPaneWithPreviewStillCommitsSilently(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	pressKey(t, h, "s")
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)

	cmd := pressSplitPane(h)

	require.NotNil(t, cmd)
	assert.Nil(t, h.panePreviewTxn)
	assert.Equal(t, []string{"alpha", "beta"}, visibleTitles(h))
	panes := h.store.OpenPanes()
	assert.Same(t, alpha, panes[0].Instance(), "the owner keeps its original binding")
	assert.Same(t, beta, panes[1].Instance(), "the preview target gains its own pane")
	assert.Equal(t, 0, panes[1].Tab())
	assert.Equal(t, layout.PaneRegion(panes[1].ID()), h.ring.Active(), "the new pane takes focus")
	status := h.errBox.String()
	assert.NotContains(t, status, "The pane is already open")
	assert.NotContains(t, status, "No preview to keep")
}
