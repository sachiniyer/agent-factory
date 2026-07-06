package app

import (
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSelectionChanged_DispatchesPaneRefreshOffEventLoop is the regression
// test for issue #579. The selectionChanged handler used to call
// tw.UpdatePreview + tw.UpdateTerminal synchronously on the bubbletea Update
// goroutine. Each call shells out to `tmux capture-pane` (~3–5ms locally),
// so the post-detach repaint paid ~7–10ms of event-loop blocking on top of
// waiting up to a full previewTickMsg cycle (~100ms) for the first paint.
//
// After the fix, selectionChanged returns a tea.Cmd that runs both captures
// in a goroutine and emits panesRefreshedMsg{}. The Update goroutine itself
// returns essentially instantly.
func TestSelectionChanged_DispatchesPaneRefreshOffEventLoop(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 0)

	start := time.Now()
	cmd := h.selectionChanged()
	elapsed := time.Since(start)

	require.NotNil(t, cmd, "selectionChanged must return the off-loop pane refresh cmd")
	// 5ms is generous; the synchronous path on real tmux was 7–10ms total.
	// FakeBackend's Preview/PreviewFullHistory return instantly so this is
	// well below 1ms in practice — the budget exists to absorb scheduler
	// noise on busy CI runners.
	assert.Less(t, elapsed, 5*time.Millisecond,
		"selectionChanged must not block on tmux captures; saw %s", elapsed)
}

// TestRepaintAfterDetachMsg_KicksOffRefresh ensures the post-detach repaint
// path goes through selectionChanged (and therefore through the off-loop
// refresh cmd). Before the fix, the attach goroutine set m.state=stateDefault
// but emitted no tea.Msg, so the first paint waited for the next ticker
// (up to ~100ms). After the fix, the goroutine returns
// repaintAfterDetachMsg{}, which Update handles by calling selectionChanged.
func TestRepaintAfterDetachMsg_KicksOffRefresh(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 0)
	statePath, err := config.TUIStatePath()
	require.NoError(t, err)

	start := time.Now()
	_, cmd := h.Update(repaintAfterDetachMsg{})
	elapsed := time.Since(start)

	require.NotNil(t, cmd,
		"repaintAfterDetachMsg must return a refresh cmd so bubbletea repaints "+
			"with fresh content immediately after detach")
	assert.Less(t, elapsed, 5*time.Millisecond,
		"Update for repaintAfterDetachMsg must not block; saw %s", elapsed)
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr),
		"repaintAfterDetachMsg must not synchronously flush TUI view-state")
}

// TestRefreshPanesCmd_ProducesPanesRefreshedMsg verifies the goroutine body
// itself: with a FakeBackend (no real tmux), the cmd must complete and
// return panesRefreshedMsg so bubbletea re-runs View() against the freshly
// captured content.
func TestRefreshPanesCmd_ProducesPanesRefreshedMsg(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	resizeHome(h, 120, 40)
	p := openTestPane(t, h, inst, 0)
	tw := h.paneWindows[p.ID()]
	cmd := refreshPanesCmd(tw, inst)
	require.NotNil(t, cmd)

	got := cmd()
	_, ok := got.(panesRefreshedMsg)
	require.True(t, ok, "refreshPanesCmd must return panesRefreshedMsg, got %T", got)

	// And the handler for the msg must not itself schedule more work
	// (otherwise we'd loop forever on every repaint).
	_, followup := h.Update(got)
	assert.Nil(t, followup,
		"panesRefreshedMsg handler must return nil — bubbletea will re-render "+
			"after Update returns, no extra work needed")
}
