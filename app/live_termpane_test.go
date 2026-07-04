package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// fakeLiveTerm drives the live-termpane state machine (#1089 PR 1) without
// spawning tmux attach clients.
type fakeLiveTerm struct {
	closed bool
	done   chan struct{}
}

func newFakeLiveTerm() *fakeLiveTerm {
	return &fakeLiveTerm{done: make(chan struct{})}
}

func (f *fakeLiveTerm) Render(width, height int) string { return "FAKE-LIVE-GRID" }
func (f *fakeLiveTerm) Resize(width, height int)        {}
func (f *fakeLiveTerm) Close() error                    { f.closed = true; return nil }
func (f *fakeLiveTerm) Done() <-chan struct{}           { return f.done }

// stubLiveTermFactory points the bind seam at fake attachments and returns
// the created fakes + attempted session names.
func stubLiveTermFactory(t *testing.T) (created *[]*fakeLiveTerm, sessions *[]string) {
	t.Helper()
	var fakes []*fakeLiveTerm
	var names []string
	orig := newLiveTermPaneFn
	newLiveTermPaneFn = func(sessionName string, width, height int) (liveTermAttachment, error) {
		f := newFakeLiveTerm()
		fakes = append(fakes, f)
		names = append(names, sessionName)
		return f, nil
	}
	t.Cleanup(func() { newLiveTermPaneFn = orig })
	return &fakes, &names
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
	fakes, sessions := stubLiveTermFactory(t)

	h.syncLiveTermPane()

	require.Len(t, *fakes, 1, "focused eligible pane must bind a live attachment")
	assert.Equal(t, inst.TabTmuxName(0), (*sessions)[0], "attachment targets the pane tab's session")
	require.NotNil(t, h.liveTerm)
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	assert.Equal(t, p, h.livePane)
	assert.True(t, h.paneWindows[p.ID()].HasLive(), "the window renders through the attachment")

	// Steady state: same binding, no rebind churn.
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1)

	// A live pane's capture polling is skipped (#1089): panesRefresh must
	// not dispatch a capture for it.
	h.lastPaneCapture = map[int]time.Time{}
	_ = h.panesRefresh(false)
	assert.NotContains(t, h.lastPaneCapture, p.ID(), "live pane must not capture-poll")
}

func TestSyncLiveTermPaneSurvivesFocusOnTree(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	// Focus moving to the tree must NOT churn the attachment while the pane
	// stays visible.
	h.focusRegion("tree")
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1)
	assert.False(t, (*fakes)[0].closed)
	require.NotNil(t, h.liveTerm)
}

func TestHidePaneClosesLiveAttachment(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	_, _ = h.handleHidePane()

	assert.True(t, (*fakes)[0].closed, "hiding the pane must close its attachment")
	assert.Nil(t, h.liveTerm)
	assert.Nil(t, h.livePane)
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
	assert.Nil(t, h.liveTerm)
}

func TestSyncLiveTermPaneClosesWhileAttached(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	h.attached.Store(true)
	h.syncLiveTermPane()

	assert.True(t, (*fakes)[0].closed, "a full-screen attach must close the render client (#598 class)")
	assert.Nil(t, h.liveTerm)

	// And nothing rebinds while attached.
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1)
}

func TestAttachHelpScreenClosesLiveAttachment(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	// The full-screen attach dispatch path (all four call sites funnel
	// through showHelpScreen with helpTypeInstanceAttach) must release the
	// attachment before the attach starts — even when the help overlay
	// defers it.
	_, _ = h.showHelpScreen(helpTypeInstanceAttach{}, nil)

	assert.True(t, (*fakes)[0].closed)
	assert.Nil(t, h.liveTerm)
}

func TestClientDeathFallsBackWithBackoff(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	// The attach client dies out from under us (session killed externally).
	close((*fakes)[0].done)
	h.syncLiveTermPane()
	assert.True(t, (*fakes)[0].closed)
	assert.Nil(t, h.liveTerm, "death must drop the binding so capture rendering resumes")

	// No immediate respawn-die loop: the retry backs off...
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 1)

	// ...and rebinds once the backoff elapses.
	h.liveBindFailedAt = time.Now().Add(-2 * liveBindRetryInterval)
	h.syncLiveTermPane()
	assert.Len(t, *fakes, 2)
}

func TestQuitClosesLiveAttachment(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)

	_, _ = h.handleQuit()

	assert.True(t, (*fakes)[0].closed, "quit must not orphan the attach client")
	assert.Nil(t, h.liveTerm)
}
