package ui

import (
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// These are the regression tests for issue #940. TerminalPane established the
// invariant `fallback==true ⇒ isScrolling==false` in #672 because String()
// short-circuits on isScrolling BEFORE checking fallback — so any fallback
// entered while scrolling must also clear scroll mode and the stale viewport,
// or the prior viewport renders instead of the fallback message. PreviewPane's
// setFallbackState was missing that enforcement; these tests drive each
// fallback entry point while in scroll mode and assert the fallback message
// renders, scroll mode is exited, and the viewport content is cleared.
//
// Mirrors the TabPane twin of this invariant in terminal_test.go:
// TestTabPaneShellFallbackResetsScrollMode and the TestTabPaneShellScrollMode*
// cases.

const staleScrollMarker = "STALE-SCROLL-VIEWPORT-CONTENT"

// enterScrollWithStaleViewport puts the pane into scroll mode holding stale
// viewport content for the given instance, without going through ScrollUp (so
// the test does not depend on tmux capture behavior). currentInstance is set to
// `inst` so UpdateContent's dropStaleScrollState guard does not pre-emptively
// reset scroll mode on its own — we want to prove setFallbackState is what
// clears it.
func enterScrollWithStaleViewport(p *TabPane, inst *session.Instance) {
	enableHostHistory(p, inst, 0)
	p.currentInstance = inst
	// Adopt the agent slot too, so dropStaleView sees no (instance, tab) change
	// when UpdateContent(inst, 0) runs — we want to prove setFallbackState is
	// what clears scroll mode, not the view-change guard.
	p.currentTab = 0
	p.scroll.Scroll(&p.viewport, scrollOneLineUp)
	p.viewport.SetContent(staleScrollMarker)
}

// TestPreviewScrollModeThenDeletingFallback: a same-instance Running → Deleting
// transition while scrolling must show "Tearing down session…", not the
// stale viewport (#940 / #920).
func TestPreviewScrollModeThenDeletingFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "deleting", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Deleting)

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	enterScrollWithStaleViewport(p, inst)

	require.NoError(t, p.UpdateContent(inst, 0))

	require.False(t, p.scroll.Active(),
		"entering the Deleting fallback must exit scroll mode")
	require.True(t, p.content.fallback,
		"Deleting instance must render a fallback")
	require.NotContains(t, p.viewport.View(), staleScrollMarker,
		"stale viewport content must be cleared on fallback")

	rendered := p.String()
	require.Contains(t, rendered, "Tearing down session…",
		"rendered frame must show the teardown fallback, not stale scroll content")
	require.NotContains(t, rendered, staleScrollMarker,
		"rendered frame must not be the stale viewport")
}

// TestPreviewScrollModeThenNilInstanceFallback: deselecting to a nil instance
// while scrolling must show the welcome fallback, not the stale viewport.
func TestPreviewScrollModeThenNilInstanceFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	// currentInstance stays nil and we adopt the agent slot so dropStaleView
	// (nil==nil, tab unchanged) does not reset scroll itself — setFallbackState
	// must.
	p.currentTab = 0
	p.scroll.Scroll(&p.viewport, scrollOneLineUp)
	p.viewport.SetContent(staleScrollMarker)

	require.NoError(t, p.UpdateContent(nil, 0))

	require.False(t, p.scroll.Active(),
		"the nil-instance fallback must exit scroll mode")
	require.True(t, p.content.fallback)
	require.NotContains(t, p.viewport.View(), staleScrollMarker,
		"stale viewport content must be cleared on fallback")

	rendered := p.String()
	require.Contains(t, rendered, "No agents running yet",
		"rendered frame must show the welcome fallback, not stale scroll content")
	require.NotContains(t, rendered, staleScrollMarker)
}

// TestPreviewScrollModeThenLoadingFallback: a Loading instance while scrolling
// must show "Setting up workspace…", not the stale viewport.
func TestPreviewScrollModeThenLoadingFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "loading", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Loading)

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	enterScrollWithStaleViewport(p, inst)

	require.NoError(t, p.UpdateContent(inst, 0))

	require.False(t, p.scroll.Active(),
		"entering the Loading fallback must exit scroll mode")
	require.True(t, p.content.fallback)
	require.NotContains(t, p.viewport.View(), staleScrollMarker)

	rendered := p.String()
	require.Contains(t, rendered, "Setting up workspace…",
		"rendered frame must show the Loading fallback, not stale scroll content")
	require.NotContains(t, rendered, staleScrollMarker)
}

func TestPreviewScrollEntryDeletingShowsTeardownFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "deleting", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Deleting)

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	enableHostHistory(p, inst, 0)

	require.NoError(t, p.ScrollUp(inst, 0))

	require.False(t, p.scroll.Active(),
		"deleting agent tab must not enter empty scroll mode")
	require.True(t, p.content.fallback,
		"deleting agent tab must enter fallback state when scrolling starts")
	require.Contains(t, p.content.text, "Tearing down session…")
	require.Empty(t, strings.TrimSpace(p.viewport.View()),
		"a rejected scroll entry must not leave viewport content")
	require.Contains(t, p.String(), "Tearing down session…",
		"rendered frame must show the teardown fallback")
}

// TestPreviewScrollModeViewportContentCleared focuses on the viewport-clearing
// half of the invariant: after entering any fallback, viewport.View() must no
// longer contain the captured scroll content. Without the fix the viewport is
// left untouched and String() (which checks isScrolling first) renders it.
func TestPreviewScrollModeViewportContentCleared(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "deleting", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Deleting)

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	enterScrollWithStaleViewport(p, inst)
	require.Contains(t, p.viewport.View(), staleScrollMarker,
		"precondition: viewport holds stale scroll content")

	require.NoError(t, p.UpdateContent(inst, 0))

	require.False(t, p.scroll.Active(), "scroll mode must be exited")
	require.NotContains(t, p.viewport.View(), staleScrollMarker,
		"viewport content must be cleared when entering a fallback (#940)")
}

// TestPreviewScrollModeThenSessionGoneFallback: when the user enters scroll
// mode (ScrollUp) but the tmux session has vanished, the PreviewFullHistory
// capture fails with ErrSessionGone and the pane enters the session-gone
// fallback. setFallbackState must leave the pane with isScrolling==false and a
// cleared viewport so the fallback renders.
func TestPreviewScrollModeThenSessionGoneFallback(t *testing.T) {
	var sessionCreated, sessionGone atomic.Bool

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			cmdStr := cmd.String()
			if strings.Contains(cmdStr, "has-session") {
				if sessionGone.Load() {
					return fmt.Errorf("session gone")
				}
				if sessionCreated.Load() {
					return nil
				}
				return fmt.Errorf("session does not exist")
			}
			if strings.Contains(cmdStr, "new-session") {
				sessionCreated.Store(true)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			cmdStr := cmd.String()
			if strings.Contains(cmdStr, "capture-pane") {
				if sessionGone.Load() {
					return nil, fmt.Errorf("exit status 1")
				}
				return []byte("hello world"), nil
			}
			return []byte(""), nil
		},
	}

	setup := setupTestEnvironment(t, cmdExec)
	defer setup.cleanupFn()

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)

	// Register currentInstance via a live render so ScrollUp's dropStaleScrollState
	// guard does not reset scroll on the instance-switch path.
	require.NoError(t, p.UpdateContent(setup.instance, 0))

	// Session vanishes. Entering scroll mode is now I/O-free (#1637): ScrollUp
	// just marks a pending fill, and the off-loop refresh (UpdateContent) is where
	// the full-history capture fails with ErrSessionGone and routes through
	// setFallbackState — so drive that refresh to reach the fallback.
	sessionGone.Store(true)

	require.NoError(t, p.ScrollUp(setup.instance, 0))
	require.NoError(t, p.UpdateContent(setup.instance, 0))

	require.False(t, p.scroll.Active(),
		"session-gone while entering scroll mode must not leave the pane scrolling")
	require.True(t, p.content.fallback,
		"preview must enter fallback state when the session is gone")
	require.Contains(t, p.content.text, "Session no longer running")
	require.NotContains(t, p.viewport.View(), "hello world",
		"viewport must not retain stale captured content under the fallback")
	require.Contains(t, p.String(), "Session no longer running",
		"rendered frame must show the session-gone fallback")
}
