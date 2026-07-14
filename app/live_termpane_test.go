package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/session"
)

// fakeLiveTerm drives the per-pane live-attachment state machine (#1592 Phase 2
// PR6) and the interactive-mode key/mouse forwarding without dialing real WS
// streams. It has no death signal: the real attachment self-heals via
// reconnect+replay, so the app never observes a "client died" event.
type fakeLiveTerm struct {
	closed bool
	// keys records every message forwarded through SendKey, as
	// tea.KeyMsg.String() values.
	keys []string
	// mice records every event forwarded through SendMouse with its grid-local
	// coordinates (#1024 R4 interactive forwarding).
	mice []forwardedMouse
	// mouseTracking is what MouseTrackingEnabled reports — the fake's stand-in for
	// the inner app having requested mouse reporting (#1024 wheel fix). Off by
	// default: a program sitting at a prompt owns no wheel.
	mouseTracking bool
}

// forwardedMouse is one SendMouse call as the fake recorded it.
type forwardedMouse struct {
	msg  tea.MouseMsg
	x, y int
}

func newFakeLiveTerm() *fakeLiveTerm { return &fakeLiveTerm{} }

func (f *fakeLiveTerm) Render(width, height int, showCursor bool) string { return "FAKE-LIVE-GRID" }
func (f *fakeLiveTerm) Resize(width, height int)                         {}
func (f *fakeLiveTerm) Close() error                                     { f.closed = true; return nil }
func (f *fakeLiveTerm) SendKey(msg tea.KeyMsg) bool {
	f.keys = append(f.keys, msg.String())
	return true
}

func (f *fakeLiveTerm) SendMouse(msg tea.MouseMsg, x, y int) bool {
	f.mice = append(f.mice, forwardedMouse{msg: msg, x: x, y: y})
	return true
}

func (f *fakeLiveTerm) MouseTrackingEnabled() bool { return f.mouseTracking }

// stubLiveTermFactory points the attachment seam at fake attachments and returns
// the created fakes + the session titles they were created for.
func stubLiveTermFactory(t *testing.T) (created *[]*fakeLiveTerm, titles *[]string) {
	t.Helper()
	var fakes []*fakeLiveTerm
	var names []string
	orig := newLiveTermPaneFn
	newLiveTermPaneFn = func(title, repoID string, tab, width, height int) liveTermAttachment {
		f := newFakeLiveTerm()
		fakes = append(fakes, f)
		names = append(names, title)
		return f
	}
	t.Cleanup(func() { newLiveTermPaneFn = orig })
	return &fakes, &names
}

// focusedFake returns the fakeLiveTerm bound to the focused pane, or nil. It is
// the per-pane replacement for the old single h.liveTerm cast.
func focusedFake(h *home) *fakeLiveTerm {
	lt, _ := h.focusedLiveTerm()
	f, _ := lt.(*fakeLiveTerm)
	return f
}

// liveTestHome is a home with one started (mock tmux) instance opened as the
// focused pane at a real layout size — the eligible-bind baseline.
func liveTestHome(t *testing.T) (*home, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	inst := startedLocalInstance(t, "live")
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 0)
	return h, inst
}

func TestSyncLiveTermPaneBindsFocusedPane(t *testing.T) {
	h, inst := liveTestHome(t)
	fakes, titles := stubLiveTermFactory(t)

	h.syncLiveTermPane()

	require.Len(t, *fakes, 1, "the visible eligible pane must bind a live attachment")
	assert.Equal(t, inst.Title, (*titles)[0], "attachment targets the pane's session title")
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	require.NotNil(t, h.liveTerms[p.ID()])
	assert.True(t, h.paneWindows[p.ID()].HasLive(), "the window renders through the attachment")

	// Steady state: same binding, no rebind churn.
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1)

	// A live pane's capture polling is skipped: panesRefresh must not dispatch a
	// daemon-Preview fetch for it.
	h.lastPaneCapture = map[int]time.Time{}
	_ = h.panesRefresh(false)
	assert.NotContains(t, h.lastPaneCapture, p.ID(), "live pane must not capture-poll")
}

// TestLivePaneScrollFillCapturesDespiteLiveSkip is the #1704 regression. When a
// live pane enters scroll mode (Ctrl+U) it stops rendering the streamed live
// grid and shows the capture viewport instead — which is EMPTY until the
// off-loop scrollback fill runs. panesRefresh normally skips capture for a live
// pane, but that skip must NOT apply while a scroll fill is pending, or the
// viewport stays blank forever and Ctrl+D can never restore it. Before the fix
// `if w.HasLive() { continue }` swallowed the fill; now it is gated on
// !NeedsScrollFill so the one-shot fill lands.
func TestLivePaneScrollFillCapturesDespiteLiveSkip(t *testing.T) {
	h, _ := liveTestHome(t)
	_, _ = stubLiveTermFactory(t)
	h.syncLiveTermPane()

	p := h.focusedOpenPane()
	require.NotNil(t, p)
	w := h.paneWindows[p.ID()]
	require.True(t, w.HasLive(), "pane renders through the live attachment")

	// Ctrl+U enters scroll mode: the live render is replaced by the capture
	// viewport, empty with a pending fill.
	w.ScrollUp()
	require.True(t, w.NeedsScrollFill(), "scroll entry leaves a pending scrollback fill")

	// The pending fill MUST get a capture dispatched even though the pane is live
	// — otherwise the viewport renders blank until scroll mode is left (#1704).
	h.lastPaneCapture = map[int]time.Time{}
	_ = h.panesRefresh(false)
	assert.Contains(t, h.lastPaneCapture, p.ID(),
		"a live pane with a pending scroll fill must capture the scrollback (#1704)")
}

func TestSyncLiveTermPaneSurvivesFocusOnTree(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	// Focus moving to the tree must NOT churn the attachment: every VISIBLE pane
	// stays bound regardless of where the focus ring points.
	h.focusRegion("tree")
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1)
	assert.False(t, (*fakes)[0].closed)
}

func TestHidePaneClosesLiveAttachment(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	_, _ = h.handleHidePane()

	assert.True(t, (*fakes)[0].closed, "hiding the pane must close its attachment")
	assert.Empty(t, h.liveTerms)
}

func TestSyncLiveTermPaneSkipsIneligiblePanes(t *testing.T) {
	h := newTestHome(t)
	inst := addTreeInstance(t, h, "unstarted") // never started: not eligible
	h.sidebar.SetSelectedInstance(0)
	h.store.SetSelectedInstance(inst)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 0)
	fakes, _ := stubLiveTermFactory(t)

	h.syncLiveTermPane()

	assert.Empty(t, *fakes, "an unstarted instance must not bind")
	assert.Empty(t, h.liveTerms)
}

func TestSyncLiveTermPaneClosesWhileAttached(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	h.attached.Store(true)
	h.syncLiveTermPane()

	assert.True(t, (*fakes)[0].closed, "a full-screen attach must close every live attachment (#598 class)")
	assert.Empty(t, h.liveTerms)

	// And nothing rebinds while attached.
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1)
}

// TestSyncLiveTermPaneClosesWhileAttachTransitioning is the #1661 regression: the
// window between the attach dispatch (which closes the panes) and the attach
// actually starting. When the one-time attach help is already seen, showHelpScreen
// closes the panes and dispatches the attach through a 20ms tea.Tick, during which
// attached is still false and m.state is still stateDefault. A reconcile in that
// window must NOT rebuild an attachment — one rebuilt here would live through the
// full-screen attach, be reflowed to garbage by the attach client's resize, and be
// kept (stale/blank) after detach because its stream never dropped.
func TestSyncLiveTermPaneClosesWhileAttachTransitioning(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	// The attach is dispatched but not yet started: attached is false, state is
	// still default — only attachTransitioning marks the in-flight attach.
	h.attachTransitioning = true
	require.False(t, h.attached.Load())
	require.Equal(t, stateDefault, h.state)

	h.syncLiveTermPane()
	assert.True(t, (*fakes)[0].closed, "the pending attach must release every live attachment")
	assert.Empty(t, h.liveTerms)

	// And nothing rebinds during the transition — otherwise a pane created here
	// survives into the attach and renders stale after detach (#1661).
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1, "no attachment may be rebuilt while the attach is in flight")

	// Once the attach fully unwinds (detach clears both flags), a fresh attachment
	// is rebuilt — with a clean emulator, not the corrupted one.
	h.attachTransitioning = false
	h.syncLiveTermPane()
	require.Len(t, *fakes, 2, "detach must rebuild a fresh attachment")
	assert.False(t, (*fakes)[1].closed)
}

func TestAttachHelpScreenClosesLiveAttachment(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	// The full-screen attach dispatch path (all four call sites funnel through
	// showHelpScreen with helpTypeInstanceAttach) must release the attachments
	// before the attach starts — even when the help overlay defers it.
	_, _ = h.showHelpScreen(helpTypeInstanceAttach{}, nil)

	assert.True(t, (*fakes)[0].closed)
	assert.Empty(t, h.liveTerms)
}

func TestQuitClosesLiveAttachment(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	_, _ = h.handleQuit()

	assert.True(t, (*fakes)[0].closed, "quit must not orphan a stream goroutine")
	assert.Empty(t, h.liveTerms)
}
