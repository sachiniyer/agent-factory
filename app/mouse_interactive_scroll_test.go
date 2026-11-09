package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// ----------------------------------------------------------------------------
// Interactive mode × mouse wheel scrollback continuation + Esc scroll-exit
// (the #1748 follow-up regression). The original #1748 fix made a wheel over a
// focused live pane whose child is NOT mouse-tracking fall through to host
// scrollback for ONE notch. The next bubbletea re-render gated the term zone
// out (registerZones: liveShowing && !IsScrolling), so the same grid cells then
// resolved to the pane body zone, which forwardInteractiveMouse unconditionally
// consumed — every subsequent wheel was dead while interactive, and the
// advertised "Scroll · Esc exits" cue forwarded Esc into the child instead.
//
// The existing TestMouse_InteractiveWheelWithoutTrackingScrollsScrollback (in
// mouse_test.go) never re-rendered View() between the first wheel and its
// follow-up gesture, so its stale first-frame zone registry masked the bug.
// These tests re-render between gestures the way bubbletea does.
// ----------------------------------------------------------------------------

// interactiveScrollMouseHome is interactiveMouseHome over a live pane whose
// scrollback capture returns a deterministic numbered history, so a follow-up
// wheel has somewhere to scroll to (the bare interactiveMouseHome pane has no
// scrollback). The fetcher is installed BEFORE the pane opens so
// newTabPaneSource captures it for the off-loop scrollback fill. Returns the
// bound fake attachment, the pane's region, and the in-store instance.
func interactiveScrollMouseHome(t *testing.T) (*home, *fakeLiveTerm, string, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	history := appNumberedHistory(60)
	h.previewFetcher = func(req daemon.PreviewRequest) (daemon.PreviewResponse, error) {
		if req.Full {
			return testPreviewResponse(history), nil
		}
		return testPreviewResponse("visible-line"), nil
	}
	inst := startedLocalInstance(t, "live")
	selectInstance(h, inst)
	resizeHome(h, 140, 40)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	fakes, _ := stubLiveTermFactory(t)
	openTestPane(t, h, inst, 0)
	enterInteractive(t, h)
	require.True(t, h.interactive)
	require.Len(t, *fakes, 1)
	region := layout.PaneRegion(h.focusedOpenPane().ID())
	newFakeClock(h)
	return h, (*fakes)[0], region, inst
}

// TestMouse_InteractiveWheelKeepsScrollingAfterReRender is the core #1748
// follow-up regression. The existing
// TestMouse_InteractiveWheelWithoutTrackingScrollsScrollback never re-rendered
// View() between the first wheel and its follow-up gesture, so its stale
// first-frame zone registry (term zone still present) masked the bug. In
// production bubbletea resets/rebuilds the registry every frame: the first
// wheel enters scroll mode, the next render drops the term zone (registerZones
// gates it on !IsScrolling), and the same grid cells then resolve to the body
// zone — which forwardInteractiveMouse unconditionally consumed. Every
// subsequent wheel over the scrolled-back pane was dead while interactive. This
// test re-renders between the first and second wheel the way bubbletea does, so
// it pins the post-fix behavior: the second wheel still scrolls host scrollback
// one line, and the wheel is never forwarded into the child.
func TestMouse_InteractiveWheelKeepsScrollingAfterReRender(t *testing.T) {
	h, fake, region, inst := interactiveScrollMouseHome(t)
	require.False(t, fake.modes.MouseTrackingEnabled(),
		"the inner app owns no wheel until it enables tracking — the #1748 scenario")

	term := zoneRect(t, h, zones.PaneTerm(region))
	wx, wy := term.X+5, term.Y+3
	w := h.paneWindows[h.focusedOpenPane().ID()]

	// First wheel-up over the term grid: not forwarded, falls through to
	// handleWheel, enters scroll mode.
	wheel(h, wx, wy, true)
	assert.Empty(t, fake.mice, "the first untracked wheel must not forward into the inner app")
	require.True(t, w.IsInScrollMode(), "the first wheel must enter scroll mode")
	assert.True(t, h.interactive, "scrolling scrollback must not leave interactive mode")

	// Land the off-loop scrollback fill so the viewport has real content.
	require.NoError(t, w.UpdateContent(inst), "the scroll-fill capture must land the history")

	// Re-render the way bubbletea does after every mouse event. registerZones
	// now drops the term zone (liveShowing == live && !IsScrolling is false) and
	// the body zone covers the whole pane rect — the production frame state
	// the stale-registry test never observed.
	_ = h.View()
	_, termStillRegistered := h.zones.Find(zones.PaneTerm(region))
	require.False(t, termStillRegistered,
		"scroll mode must drop the term zone on re-render — the production frame the stale test missed")
	_, bodyRegistered := h.zones.Find(zones.PaneBody(region))
	require.True(t, bodyRegistered, "the body zone covers the whole pane while scrolling")

	// Capture the viewport AFTER the first scroll + fill + re-render.
	baseline := firstRenderedHistoryMarker(w.View(), 60)
	require.Greater(t, baseline, 1, "precondition: there is scrollback above the viewport to scroll into")

	// Second wheel-up at the SAME screen coordinates — now resolving to the
	// body zone. Before the fix this was swallowed (the pane view did not
	// advance); now it must scroll host scrollback one more line.
	wheel(h, wx, wy, true)

	assert.Empty(t, fake.mice, "the second wheel must still not forward into the inner app")
	assert.True(t, w.IsInScrollMode(), "the pane must remain in scroll mode after the second wheel")
	assert.True(t, h.interactive, "continued scrolling must not leave interactive mode")
	assert.Equal(t, baseline-1, firstRenderedHistoryMarker(w.View(), 60),
		"the second wheel must scroll host scrollback one more line (#1748 regression: was a no-op swallow after re-render)")

	// Control: a direct ScrollUp moves the ready viewport exactly one more
	// line, proving the second wheel's advance is real scrolling and not
	// "already at top" masquerading as a pass.
	w.ScrollUp()
	assert.Equal(t, baseline-2, firstRenderedHistoryMarker(w.View(), 60),
		"the viewport must still be scrollable after the second wheel — control for 'already at top'")
}

// TestMouse_InteractiveEscExitsScrollMode pins the second half of the #1748
// follow-up: while interactive and the focused pane is in scroll mode, Esc
// exits scroll mode (instead of being forwarded into the child) so the pane's
// own "Scroll · Esc exits" header cue stays truthful. The pane stays
// interactive so the user can resume typing. A pane NOT in scroll mode still
// forwards Esc as pane input — the interception is scoped to scroll mode
// alone, so alt-screen apps that rely on Esc keep receiving it.
func TestMouse_InteractiveEscExitsScrollMode(t *testing.T) {
	h, fake, region, inst := interactiveScrollMouseHome(t)
	w := h.paneWindows[h.focusedOpenPane().ID()]
	term := zoneRect(t, h, zones.PaneTerm(region))
	wx, wy := term.X+5, term.Y+3

	wheel(h, wx, wy, true)
	require.True(t, w.IsInScrollMode())
	require.NoError(t, w.UpdateContent(inst))
	_ = h.View()
	require.Contains(t, w.View(), "Scroll · Esc exits",
		"the pane advertises an Esc-driven scroll-mode exit — the cue that must stay truthful")

	// Esc must exit scroll mode, NOT forward into the child.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})

	assert.False(t, w.IsInScrollMode(), "Esc must exit scroll mode while interactive")
	assert.True(t, h.interactive, "Esc must NOT leave interactive mode — the user resumes typing")
	assert.Empty(t, fake.keys, "Esc while in scroll mode must not forward into the child (the cue is truthful)")

	// Now NOT in scroll mode: Esc forwards to the child unchanged — the
	// interception is scoped to scroll mode, so an alt-screen app's Esc still
	// reaches it.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, []string{"esc"}, fake.keys,
		"Esc outside scroll mode must forward to the child as pane input")
	assert.True(t, h.interactive, "a forwarded Esc must not leave interactive mode")

	// Final sanity: the pane still renders the live grid (scroll mode fully
	// cleared) and stays the focused interactive input target.
	_ = h.View()
	_, termRegistered := h.zones.Find(zones.PaneTerm(region))
	require.True(t, termRegistered, "the live terminal zone is restored after exiting scroll mode")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	assert.Equal(t, []string{"esc", "z"}, fake.keys, "typing resumes into the pane after Esc exits scroll mode")
}
