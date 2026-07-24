package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2479: a kill that times out against a WEDGED LOCAL daemon must OFFER an
// in-interface restart — the interface runs the recovery — instead of printing a
// shell command. These drive the real handler path: an instanceKilledMsg wrapping
// errDaemonUnresponsive through Update, then the confirm and its async restart.

func setRestartActionForTest(t *testing.T, f func() error) {
	t.Helper()
	prev := restartDaemonAction
	restartDaemonAction = f
	t.Cleanup(func() { restartDaemonAction = prev })
}

func setRemoteTargetForTest(t *testing.T, remote bool) {
	t.Helper()
	prev := isRemoteTarget
	isRemoteTarget = func() bool { return remote }
	t.Cleanup(func() { isRemoteTarget = prev })
}

// wedgedKillResult is the error killSessionThroughDaemon returns on a timeout —
// built the same way, so a change to the wrapping is caught here too.
func wedgedKillResult() error {
	return fmt.Errorf("%w within 60s — the teardown may still finish in the background", errDaemonUnresponsive)
}

func armKilledInstance(t *testing.T, h *home, title string) sessionActionTarget {
	t.Helper()
	inst := newKillableInstance(t, title)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	return captureSessionActionTarget(inst, h.repoID)
}

// A wedged LOCAL daemon: the kill handler opens the restart confirm; accepting it
// runs the in-interface restart rather than surfacing any shell command.
func TestKillTimeout_OffersInInterfaceRestart(t *testing.T) {
	setRemoteTargetForTest(t, false)
	restartCalled := false
	setRestartActionForTest(t, func() error {
		restartCalled = true
		return nil
	})

	h := newTestHome(t)
	resizeHome(h, 120, 45) // roomy, so the confirm renders its full copy
	target := armKilledInstance(t, h, "wedged")

	// The kill came back with the wedged-daemon error: the handler must open a
	// confirm, not drop a shell command into the error box.
	model, _ := h.Update(instanceKilledMsg{target: target, err: wedgedKillResult()})
	hm := model.(*home)
	require.Equal(t, stateConfirm, hm.state, "a wedged local daemon must open the restart confirm")
	require.NotNil(t, hm.confirmationOverlay)
	dialog := hm.confirmationOverlay.Render()
	assert.Contains(t, dialog, "Restart it?", "the confirm must offer the restart")
	assert.NotContains(t, dialog, "af daemon restart", "the offer must not print the shell command")
	assert.Contains(t, dialog, "may be dropped", "the confirm must warn that sessions can be dropped (#2176)")

	// Accept the confirm: it forwards daemonRestartRequestedMsg, which dispatches
	// the async restart cmd.
	model, cmd := hm.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.Equal(t, stateDefault, model.(*home).state)
	require.NotNil(t, cmd, "accepting the confirm must forward the restart request")

	reqMsg := cmd()
	require.IsType(t, daemonRestartRequestedMsg{}, reqMsg)
	_, restartCmd := h.Update(reqMsg)
	require.NotNil(t, restartCmd, "the request must dispatch the restart cmd off the event loop")

	// Run the restart cmd the way bubbletea would.
	doneMsg := restartCmd()
	restarted, ok := doneMsg.(daemonRestartedMsg)
	require.True(t, ok, "restart cmd must emit daemonRestartedMsg, got %T", doneMsg)
	require.NoError(t, restarted.err)
	assert.True(t, restartCalled, "the in-interface restart must actually run the restart action")

	// Success feedback, and no lingering shell instruction.
	_, _ = h.Update(restarted)
	assert.NotContains(t, h.errBox.FullError(), "af daemon restart")
}

// A REMOTE target's daemon is on another machine, so a local restart cannot help:
// the handler must NOT offer it, and must fall through to the plain error rather
// than pop a confirm that would do nothing.
func TestKillTimeout_RemoteTargetDoesNotOfferRestart(t *testing.T) {
	setRemoteTargetForTest(t, true)
	setRestartActionForTest(t, func() error {
		t.Fatal("a remote target must never run the local daemon restart")
		return nil
	})

	h := newTestHome(t)
	target := armKilledInstance(t, h, "remote-wedged")

	model, _ := h.Update(instanceKilledMsg{target: target, err: wedgedKillResult()})
	hm := model.(*home)
	assert.NotEqual(t, stateConfirm, hm.state, "a remote wedged daemon must not open a local-restart confirm")
	assert.Contains(t, hm.errBox.FullError(), "failed to kill", "the plain error must surface instead")
}

// When the restart itself cannot run, the last-resort fallback is a clear message
// — the one place naming the manual command is honest, because the in-interface
// recovery was attempted and failed.
func TestDaemonRestartFailure_FallsBackToAClearMessage(t *testing.T) {
	setRestartActionForTest(t, func() error {
		return fmt.Errorf("spawn refused")
	})

	h := newTestHome(t)
	_, _ = h.handleDaemonRestarted(daemonRestartedMsg{err: fmt.Errorf("spawn refused")})

	full := h.errBox.FullError()
	assert.Contains(t, full, "could not restart the daemon", "the fallback must say the restart failed")
	assert.Contains(t, full, "spawn refused", "the fallback must carry the underlying cause")
	assert.Contains(t, full, "af daemon restart", "the last-resort fallback may name the manual command")
}
