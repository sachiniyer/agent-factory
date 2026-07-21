package app

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

func appNumberedHistory(lines int) string {
	out := make([]string, lines)
	for i := range out {
		out[i] = fmt.Sprintf("input-history-%03d", i+1)
	}
	return strings.Join(out, "\n")
}

func firstRenderedHistoryMarker(view string, maxLines int) int {
	for i := 1; i <= maxLines; i++ {
		if strings.Contains(view, fmt.Sprintf("input-history-%03d", i)) {
			return i
		}
	}
	return 0
}

// TestPreviewScrollFirstIntentConformanceKeyboardAndWheel proves both root
// input paths submit the same semantic first intent to host-history preview.
// The size matrix is the real outer-terminal contract requested by #2192; the
// expected top marker is derived from each resulting pane height.
func TestPreviewScrollFirstIntentConformanceKeyboardAndWheel(t *testing.T) {
	const historyLines = 100
	history := appNumberedHistory(historyLines)

	for _, size := range []struct {
		name          string
		width, height int
	}{
		{name: "80x24", width: 80, height: 24},
		{name: "120x40", width: 120, height: 40},
	} {
		for _, input := range []string{"keyboard", "wheel"} {
			t.Run(size.name+"/"+input, func(t *testing.T) {
				h := newTestHome(t)
				h.previewFetcher = func(req daemon.PreviewRequest) (daemon.PreviewResponse, error) {
					if req.Full {
						return testPreviewResponse(history), nil
					}
					return testPreviewResponse("visible-line"), nil
				}
				inst := instanceWithFakeBackend(t, "scroll-input")
				inst.AddTabForTest("agent", session.TabKindAgent)
				h.store.AddInstance(inst)
				h.sidebar.SetSelectedInstance(0)
				_ = h.selectionChanged()

				resizeHome(h, size.width, size.height)
				pane := openTestPane(t, h, inst, 0)
				w := h.paneWindows[pane.ID()]
				require.NotNil(t, w)
				require.NoError(t, w.UpdateContent(inst),
					"the first detached snapshot establishes host ownership")

				switch input {
				case "keyboard":
					_, _ = h.handleDefaultKeyPress(
						tea.KeyMsg{Type: tea.KeyCtrlU}, keys.KeyShiftUp)
				case "wheel":
					region := layout.PaneRegion(pane.ID())
					body := zoneRect(t, h, zones.PaneBody(region))
					wheel(h, body.X+1, body.Y+1, true)
				}

				require.True(t, w.IsInScrollMode(), "first %s input enters scroll mode", input)
				require.Equal(t, ui.ScrollOwnerHostHistory, w.ScrollOwner())
				require.NoError(t, w.UpdateContent(inst))

				_, paneHeight := w.GetPreviewSize()
				// AF chrome lives in the pane header, outside these 100 terminal
				// rows. Bottom's first marker is 101-paneHeight; one preserved up
				// intent makes it 100-paneHeight.
				require.Equal(t, 100-paneHeight, firstRenderedHistoryMarker(w.View(), historyLines),
					"first %s intent must be visible after fill at %s", input, size.name)

				// Exercise the matching down route too. Once history is ready this
				// applies synchronously and must return to the newest viewport.
				switch input {
				case "keyboard":
					_, _ = h.handleDefaultKeyPress(
						tea.KeyMsg{Type: tea.KeyCtrlD}, keys.KeyShiftDown)
				case "wheel":
					region := layout.PaneRegion(pane.ID())
					body := zoneRect(t, h, zones.PaneBody(region))
					wheel(h, body.X+1, body.Y+1, false)
				}
				require.Equal(t, 101-paneHeight, firstRenderedHistoryMarker(w.View(), historyLines),
					"%s down intent must return to bottom at %s", input, size.name)
			})
		}
	}
}

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
	h.previewFetcher = func(req daemon.PreviewRequest) (daemon.PreviewResponse, error) {
		if req.Full {
			atomic.AddInt32(&fullCaptures, 1)
			<-release
		}
		return testPreviewResponse("scrollback-line"), nil
	}

	pane := openTestPane(t, h, inst, 0) // captures the gated fetcher above
	w := h.paneWindows[pane.ID()]
	require.NotNil(t, w)
	require.NoError(t, w.UpdateContent(inst),
		"the first detached snapshot establishes host ownership")

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
