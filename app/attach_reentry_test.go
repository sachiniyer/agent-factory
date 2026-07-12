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
	require.True(t, inst.Capabilities().Workspace == session.WorkspaceRemote, "precondition: instance must be remote for the full-screen attach path")
	require.True(t, inst.TmuxAlive(), "precondition: instance must be attachable")

	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	attaches := 0
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		attaches++
		return m.attachOverlayCallback(title, label, traceSuffix, func() (chan struct{}, error) {
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

// TestHandleEnter_CanceledAttachHelpDoesNotWedgeGuard is the #1530 follow-up
// regression: opening the first-time attach help overlay and dismissing it with
// Esc (a cancel — the attach callback never runs) must NOT leave the re-entry
// guard armed. A subsequent Enter must still attach. Before the defensive clear,
// any attach-abort path that armed attachTransitioning without reaching the
// callback's cleanup would make every later Enter a no-op until restart.
func TestHandleEnter_CanceledAttachHelpDoesNotWedgeGuard(t *testing.T) {
	resetDetachWatchdog(t)

	h := newTestHome(t)
	// Do NOT mark the help seen — the first-time attach overlay must appear so
	// the Esc-cancel path is exercised.

	inst := instanceWithFakeBackend(t, "inst")
	inst.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	inst.SetStatusForTest(session.Running)
	require.True(t, inst.Capabilities().Workspace == session.WorkspaceRemote, "precondition: instance must be remote for the full-screen attach path")
	require.True(t, inst.TmuxAlive(), "precondition: instance must be attachable")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)
	attaches := 0
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		attaches++
		return m.attachOverlayCallback(title, label, traceSuffix, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch)
			return ch, nil
		})
	})

	// Enter → first-time attach help overlay shown; attach not started yet.
	model, _ := h.handleEnter()
	h = model.(*home)
	require.NotNil(t, h.textOverlay, "first-time attach must show the help overlay")
	require.Equal(t, stateHelp, h.state, "attach help overlay must put the model in stateHelp")
	require.False(t, h.attachTransitioning, "the help overlay alone must not arm the attach guard")
	require.Equal(t, 0, attaches, "the overlay must not start an attach")

	// Esc dismisses the overlay WITHOUT running the attach callback (cancel).
	model, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	h = model.(*home)
	require.Equal(t, stateDefault, h.state, "Esc must close the attach help overlay (back to stateDefault)")
	require.False(t, h.attachTransitioning, "a canceled attach must not leave the re-entry guard armed")
	require.Equal(t, 0, attaches, "Esc must not start an attach")

	// A subsequent Enter must STILL attach — the guard was cleared. The help was
	// marked seen on the first show, so this Enter runs the attach synchronously.
	_, cmd := h.handleEnter()
	runAttachTransitionCmd(t, h, cmd)
	endDetachWatchdog()
	require.Equal(t, 1, attaches, "a canceled attach must not block a subsequent attach")
}

// TestHandleHelpState_AttachCancelClearsTransitionGuard directly exercises the
// #1530 defensive clear: if the re-entrant-attach guard is armed while the
// attach help overlay is up (simulating any current or future path that arms
// attachTransitioning before the overlay resolves), canceling the overlay with
// Esc must clear it — otherwise every later Enter would be a permanent no-op.
// This fails without the clear in handleHelpState's cancel branch.
func TestHandleHelpState_AttachCancelClearsTransitionGuard(t *testing.T) {
	h := newTestHome(t)

	inst := instanceWithFakeBackend(t, "inst")
	inst.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	inst.SetStatusForTest(session.Running)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// Enter → first-time attach help overlay shown.
	model, _ := h.handleEnter()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state, "precondition: attach help overlay must be up")

	// Simulate the guard having been armed before the overlay resolves.
	h.attachTransitioning = true

	// Esc cancels the overlay (attachHelpDismissPolicy → runOnDismiss=false).
	model, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	h = model.(*home)
	require.Equal(t, stateDefault, h.state, "Esc must close the attach help overlay")
	require.False(t, h.attachTransitioning, "canceling the attach help must clear the re-entry guard")
}
