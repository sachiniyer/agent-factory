package app

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestPanesRefreshNoRedundantScrollFillCapture is the #1709 regression: while a
// scroll-fill capture is already in flight, a second panesRefresh cycle must NOT
// dispatch another one. Scroll entry marks a pending off-loop fill (#1637), and
// panesRefresh bypasses its throttle for a pane that NeedsScrollFill so the fill
// lands immediately. Before the fix NeedsScrollFill stayed true from scroll
// entry until the capture actually LANDED, so every refresh cycle in that window
// (rapid scroll input, or a slow daemon Preview RPC) fired a fresh, redundant
// `tmux capture-pane` — mutex-guarded so it couldn't corrupt state, but wasteful.
//
// The fetcher here blocks the fill capture so it stays in flight across two
// refresh cycles. The second cycle must be a no-op for that pane.
func TestPanesRefreshNoRedundantScrollFillCapture(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "alpha")
	inst.AddTabForTest("agent", session.TabKindAgent)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)

	// Gate the scroll-fill capture (Full==true) so it stays in flight, and count
	// how many times it is dispatched. The non-full path is left unblocked.
	release := make(chan struct{})
	var fullCaptures int32
	h.previewFetcher = func(req daemon.PreviewRequest) (string, bool, error) {
		if req.Full {
			atomic.AddInt32(&fullCaptures, 1)
			<-release
		}
		return "scrollback-line", false, nil
	}

	pane := openTestPane(t, h, inst, 0) // captures the gated fetcher above
	w := h.paneWindows[pane.ID()]
	require.NotNil(t, w)

	// Enter scroll mode: marks a pending off-loop fill, no capture on the loop.
	w.ScrollUp()
	require.True(t, w.NeedsScrollFill(), "scroll entry marks a pending off-loop fill")

	// First refresh cycle dispatches the fill. Run it off the event loop like
	// bubbletea would; it blocks in the gated capture, holding the fill in flight.
	cmd1 := h.panesRefresh(false)
	require.NotNil(t, cmd1, "the first refresh must dispatch the scroll fill")
	go cmd1()
	require.Eventually(t, func() bool { return atomic.LoadInt32(&fullCaptures) == 1 },
		3*time.Second, 5*time.Millisecond, "the first refresh dispatches exactly one fill capture")

	// Second refresh cycle WHILE the fill is still in flight: it must not dispatch
	// another capture. Before the fix this fired a redundant second capture.
	if cmd2 := h.panesRefresh(false); cmd2 != nil {
		go cmd2()
	}
	require.Never(t, func() bool { return atomic.LoadInt32(&fullCaptures) > 1 },
		250*time.Millisecond, 10*time.Millisecond,
		"a refresh while a fill is in flight must not dispatch a redundant capture (#1709)")

	close(release)
}
