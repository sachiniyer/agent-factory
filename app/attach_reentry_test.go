package app

import (
	"bytes"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestBeginAttachTransition_SecondCallIgnoredWhileInFlight pins the #1530
// re-entry guard at the funnel every full-screen attach entry point passes
// through: once a transition is armed (or an attach is live), a second
// beginAttachTransition must be a no-op — no new tick scheduled, so no second
// attach flow spins up duplicate heartbeat/resume goroutines or races
// m.attached.Store.
func TestBeginAttachTransition_SecondCallIgnoredWhileInFlight(t *testing.T) {
	h := newTestHome(t)

	// First call arms the transition and returns a real tick cmd.
	cmd1 := h.beginAttachTransition(func() tea.Cmd { return nil })
	require.NotNil(t, cmd1, "first attach transition must schedule the pre-attach tick")
	require.True(t, h.attachTransitioning, "first attach transition must arm the flag")

	// Second call while the transition is armed must be ignored.
	cmd2 := h.beginAttachTransition(func() tea.Cmd { return nil })
	require.Nil(t, cmd2, "a second attach transition while one is in flight must be a no-op")

	// Same when an attach is already live (transitioning flag cleared, attached set).
	h.attachTransitioning = false
	h.attached.Store(true)
	cmd3 := h.beginAttachTransition(func() tea.Cmd { return nil })
	require.Nil(t, cmd3, "an attach transition while already attached must be a no-op")
	h.attached.Store(false)
}

// TestHandleEnter_SecondEnterDuringAttachDoesNotScheduleSecondAttach is the
// #1530 regression: pressing Enter again during the attach transition window
// must be ignored, not kick off a second concurrent attach flow. The first
// Enter arms the transition (attachTransitioning=true) but does not run the
// attach callback yet — that happens when the returned tick resolves to a
// beginAttachMsg — so a second Enter lands squarely in the window the bug
// exploited. With the guard, exactly one attach flow runs despite two presses.
func TestHandleEnter_SecondEnterDuringAttachDoesNotScheduleSecondAttach(t *testing.T) {
	resetDetachWatchdog(t)

	h := newTestHome(t)
	// Skip the first-time attach help overlay so the deferred attach callback
	// is scheduled synchronously inside handleEnter.
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	// Remote instance so handleEnter takes the full-screen attach path
	// (liveSessionName == "") that funnels through beginAttachTransition.
	inst := instanceWithFakeBackend(t, "inst")
	inst.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	inst.SetStatusForTest(session.Running)
	require.True(t, inst.IsRemote(), "precondition: instance must be remote for the full-screen attach path")
	require.True(t, inst.TmuxAlive(), "precondition: instance must be attachable")

	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	attaches := 0
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attaches++
		return m.attachOverlayCallback(title, label, traceSuffix, rem, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch) // detach immediately, synchronously, no real PTY
			return ch, nil
		})
	})

	// First Enter arms the transition; the attach callback has NOT run yet.
	model, cmd1 := h.handleEnter()
	require.True(t, model.(*home).attachTransitioning, "first Enter must arm the attach transition")
	require.NotNil(t, cmd1, "first Enter must schedule the attach transition tick")

	// Second Enter arrives during the transition window — it must be ignored.
	_, cmd2 := h.handleEnter()
	require.Nil(t, cmd2, "a second Enter during the attach transition must be ignored")

	// Let the single scheduled transition run to completion (attach + detach).
	runAttachTransitionCmd(t, h, cmd1)
	endDetachWatchdog()

	require.Equal(t, 1, attaches, "exactly one attach flow must be scheduled despite two Enter presses")
}
