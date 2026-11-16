package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// watchdogReproHome builds the scene the bug report's investigation pinned for
// the "SLOW DETACH" false-positive (#1089 live termpane skip + #4020 auto-hide
// notice trigger): three started instances, each open as a pane at tab 0; the
// sidebar cursor is parked on beta while the focus ring rests on alpha's pane
// — the pane/selection divergence updatePanePreview's already-open-tab branch
// fires on. The terminal is then narrowed so only two panes fit, which
// pane-count fitting uses to auto-hide gamma's pane and leave a
// "gamma hidden — too narrow" status pending — the payload that turns the
// repaint's cmd non-nil.
//
// instanceWithFakeBackend (no real tmux) keeps the test host-safe — the live
// attachment is force-installed via SetLive rather than won through
// syncLiveTermPane, exactly the window state a real post-detach preview tick
// leaves on every pane that re-binds.
func watchdogReproHome(t *testing.T) (h *home, alphaPane, betaPane, gammaPane *store.OpenPane) {
	t.Helper()
	h = newTestHome(t)
	alpha := instanceWithFakeBackend(t, "alpha")
	alpha.AddTabForTest("agent", session.TabKindAgent)
	beta := instanceWithFakeBackend(t, "beta")
	beta.AddTabForTest("agent", session.TabKindAgent)
	gamma := instanceWithFakeBackend(t, "gamma")
	gamma.AddTabForTest("agent", session.TabKindAgent)
	selectInstance(h, alpha) // adds alpha, cursor on alpha (index 0)
	selectInstance(h, beta)  // cursor on beta (index 1)
	selectInstance(h, gamma) // cursor on gamma (index 2)

	// 200 cols fits three panes (PaneMinWidth=40 + tree); open gamma first so
	// its pane ends up least-recently-focused, then beta, then alpha. After
	// all three opens the focus ring rests on alpha's pane and the MRU order
	// is [alpha, beta, gamma].
	resizeHome(h, 200, 40)
	gammaP := openTestPane(t, h, gamma, 0)
	betaP := openTestPane(t, h, beta, 0)
	alphaP := openTestPane(t, h, alpha, 0)

	// Cursor on beta (selection target), but the preview-owner pane remains
	// alpha's pane (the pane/selection divergence updatePanePreview's
	// already-open-tab branch keys off). selectionChanged will catch up the
	// store-side selected instance on the next repaint — no need to call it
	// here, which would otherwise drain the pending status via
	// consumePaneAutoHideStatus.
	h.sidebar.SetSelectedInstance(1)

	// Narrow the terminal so only two panes fit (PaneMinWidth=40). Drive the
	// relayout directly: resizeHome would route through
	// updateHandleWindowSizeEvent, whose consumePaneAutoHideStatus return
	// would drain the pending notice we are about to assert on.
	h.termWidth = 120
	h.termHeight = 40
	h.relayout()
	require.NotEmpty(t, h.pendingPaneAutoHideStatus,
		"the narrower relayout must have left an auto-hide status pending for gamma")
	require.Len(t, h.visiblePanes, 2,
		"exactly two panes must fit at width 120; gamma must be auto-hidden")
	return h, alphaP, betaP, gammaP
}

// installFakeLiveOnVisible forces HasLive() true on every currently visible
// pane's window — the post-detach state a queued previewTickMsg's
// syncLiveTermPane reaches through the production bind path. panesRefresh
// subsequently skips every visible pane (the #1089 live skip), so no
// panesRefreshedMsg will ever arrive to end the slow-detach watchdog.
func installFakeLiveOnVisible(t *testing.T, h *home) {
	t.Helper()
	for _, p := range h.visiblePanes {
		w := h.paneWindows[p.ID()]
		require.NotNil(t, w, "every visible pane must have a window")
		w.SetLive(newFakeLiveTerm())
		require.True(t, w.HasLive(),
			"forced live attachment must register so panesRefresh skips the pane")
	}
}

// armedWatchdog reports whether the package-global slow-detach watchdog is
// currently armed (a detach cycle that has not been ended). Used inline so the
// assertion reads next to the begin/end pair under test.
func armedWatchdog() bool {
	detachWatchdogMu.Lock()
	defer detachWatchdogMu.Unlock()
	return detachWatchdogDone != nil
}

// TestWatchdogCanceled_WhenPostDetachCmdIsNonCaptureOnly is the regression
// test for the "SLOW DETACH" false-positive the slow-detach watchdog (#599,
// always-on since #788) emits when a post-detach repaint hands back a non-nil
// cmd carrying ONLY non-capture work. Before the fix the
// repaintAfterDetachMsg handler disarmed the watchdog only on a nil cmd or no
// visible panes, so the watchdog stayed armed when:
//
//   - every visible pane is rendered through a live termpane attachment (the
//     queued post-detach previewTickMsg's syncLiveTermPane re-binds each
//     one), so panesRefresh's HasLive skip (#1089) returns nil and no
//     panesRefreshedMsg can arrive to disarm it, AND
//   - the cmd is non-capture-only: the auto-hide notice clear timer from
//     updatePanePreview's already-open-tab branch (consumePaneAutoHideStatus),
//     reached because the cursor's (instance, activeTab) is open as a visible
//     pane different from the focused preview-owner pane, and a relayout
//     leaves a "N hidden — too narrow" status pending on that tab (#4020).
//
// tea.Batch(noticeTimer, nil) collapses to the notice timer, so the prior
// `cmd == nil || len(m.visiblePanes) == 0` guard did not fire. After the fix,
// the handler uses panesRefresh's "did any capture actually dispatch" flag
// (selectionChangedCmds' second return) and ends the watchdog when nothing
// did — covering the non-capture-only cmd the prior guard missed.
func TestWatchdogCanceled_WhenPostDetachCmdIsNonCaptureOnly(t *testing.T) {
	resetDetachWatchdog(t)
	h, _, _, _ := watchdogReproHome(t)

	// The post-detach preview tick has re-bound every visible pane live —
	// the production state syncLiveTermPane leaves — so panesRefresh will
	// skip both and dispatch no capture.
	installFakeLiveOnVisible(t, h)

	beginDetachWatchdog("repro-all-live")
	require.True(t, armedWatchdog(), "watchdog must be armed exactly as attach does")

	// The post-detach repaint: panesRefresh skips both visible (live) panes,
	// and updatePanePreview's already-open-tab branch — cursor on beta, owner
	// on alpha (different panes) — returns the auto-hide notice clear timer
	// for gamma. tea.Batch collapses it down to the timer itself.
	_, cmd := h.Update(repaintAfterDetachMsg{})
	require.NotNil(t, cmd,
		"the post-detach repaint must return the non-capture notice timer (the only surviving cmd)")

	// The watchdog must end here: no capture work dispatched (all panes
	// live), so no panesRefreshedMsg will ever arrive to do it. Before the
	// fix this assertion failed — the watchdog stayed armed and dumped a
	// goroutine stack to detach-slow.log after slowDetachThreshold (#1089 +
	// #4020).
	require.False(t, armedWatchdog(),
		"watchdog must be canceled when panesRefresh dispatched no capture work, "+
			"even if the returned cmd is a non-nil non-capture notice timer")
}

// TestWatchdog_NonLiveSceneDisarmedByPanesRefreshedMsg is the control that
// proves the fix did not regress the panesRefreshedMsg disarm path. The same
// scene WITHOUT forced live attachments — panesRefresh dispatches capture
// for every visible pane — so the repaint handler leaves the watchdog armed
// (something IS in flight to disarm it), and the panesRefreshedMsg the
// capture goroutine emits is what ends it.
func TestWatchdog_NonLiveSceneDisarmedByPanesRefreshedMsg(t *testing.T) {
	resetDetachWatchdog(t)
	h, _, _, _ := watchdogReproHome(t)
	// Deliberately do NOT install fake live attachments: the visible panes
	// fall back to daemon-Preview capture, so panesRefresh dispatches and
	// panesRefreshedMsg will eventually end the watchdog.
	for _, p := range h.visiblePanes {
		require.False(t, h.paneWindows[p.ID()].HasLive(),
			"the non-live scene must NOT have live attachments, so panesRefresh dispatches capture")
	}

	beginDetachWatchdog("repro-no-live")
	require.True(t, armedWatchdog(), "watchdog must be armed")

	_, cmd := h.Update(repaintAfterDetachMsg{})
	require.NotNil(t, cmd,
		"the repaint must return a non-nil cmd (notice timer + capture refreshes) "+
			"so its returned cmd is a realistic post-detach batch")

	// Capture WAS dispatched, so the repaint handler must leave the watchdog
	// armed — the in-flight captures' panesRefreshedMsg is the watchdog's
	// disarm signal, not the handler's check.
	require.True(t, armedWatchdog(),
		"watchdog must stay armed when capture is dispatched — panesRefreshedMsg disarms it, not the repaint handler")

	// The capture goroutine emits panesRefreshedMsg; route one such msg back
	// through Update the way bubbletea does. The handler ends the watchdog.
	_, refreshFollowup := h.Update(panesRefreshedMsg{})
	assert.Nil(t, refreshFollowup,
		"panesRefreshedMsg must not re-arm more refresh work (rerun-on-every-msg would loop forever)")
	require.False(t, armedWatchdog(),
		"panesRefreshedMsg must end the watchdog once the captures it dispatched land")
}
