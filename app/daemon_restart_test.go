package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
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
	// #4824: a wedged remote daemon may have torn the session down with only the
	// reply lost, so the outcome is unknown — say "could not be confirmed", never
	// the "session is retained" recovery that would invite a second kill of a
	// session that may already be gone.
	assert.Nil(t, hm.recovery, "a remote wedged daemon must not show the 'session is retained' recovery")
	assert.Contains(t, hm.errBox.FullError(), "could not be confirmed", "the unknown-outcome message must surface instead")
	assert.NotContains(t, hm.errBox.FullError(), "failed to kill", "the kill is not known to have failed")
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

// TestKillTimeout_RemoteWedgedDaemonRoutesToUnknownOutcome is the regression lock
// for #4824 on the REMOTE arm of handleInstanceKilled. A kill that times out
// against a wedged remote daemon (errDaemonUnresponsive) is an outcome the client
// cannot confirm — the daemon may have torn the session down with only the reply
// lost — so it must surface "could not be confirmed", never the "session is
// retained" recovery that invites a second kill of a session that may already be
// gone.
//
// killSessionThroughDaemon rewraps the timeout into errDaemonUnresponsive with a
// single %w, dropping context.DeadlineExceeded from the chain, so the handler
// cannot rely on mutationOutcomeUnknown (which keys on the deadline) and must
// route on the error identity instead. The first three assertions pin that root
// cause; the rest pin the routing it forces, end-to-end through Update.
func TestKillTimeout_RemoteWedgedDaemonRoutesToUnknownOutcome(t *testing.T) {
	setRemoteTargetForTest(t, true)
	setRestartActionForTest(t, func() error {
		t.Fatal("a remote target must never run the local daemon restart")
		return nil
	})

	// Root cause: the rewrap keeps only errDaemonUnresponsive and drops the
	// deadline, so mutationOutcomeUnknown cannot recognize the timeout as
	// uncertain — the handler must not depend on that signal for this error.
	wrapped := wedgedKillResult()
	require.True(t, errors.Is(wrapped, errDaemonUnresponsive), "precondition: the timeout wraps errDaemonUnresponsive")
	require.False(t, errors.Is(wrapped, context.DeadlineExceeded), "the rewrap drops the deadline from the chain")
	require.False(t, mutationOutcomeUnknown(wrapped), "mutationOutcomeUnknown cannot see this as uncertain — the handler routes by identity instead")

	h := newTestHome(t)
	inst := newKillableInstance(t, "remote-wedged")
	require.NoError(t, inst.Transition(session.BeginKill()))
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	target := captureSessionActionTarget(inst, h.repoID)

	model, _ := h.Update(instanceKilledMsg{target: target, err: wrapped})
	hm := model.(*home)

	require.NotEqual(t, stateConfirm, hm.state, "a remote wedged daemon must not open a local-restart confirm")
	require.Nil(t, hm.recovery, "a remote timeout is an unknown outcome, not a 'retained' session")
	assert.Contains(t, hm.errBox.FullError(), "could not be confirmed", "the #4824 unknown-outcome wording must surface")
	assert.Contains(t, hm.errBox.FullError(), "the sidebar", "the message must point the user at authoritative state before retrying")
	assert.NotContains(t, hm.errBox.FullError(), "session is retained", "the retained wording wrongly implies the kill did not run")
	assert.NotContains(t, hm.errBox.FullError(), "failed to kill", "the kill is not known to have failed")
	assert.Contains(t, hm.errBox.FullError(), "remote-wedged", "the notice must name the session whose outcome is unknown")

	// The fence still reverts: holding it would strand the row if the kill never
	// ran, and the next snapshot removes the row if the kill landed.
	assert.NotEqual(t, session.OpKilling, inst.GetInFlightOp(), "the optimistic kill fence reverts so the row cannot strand")
	assert.Contains(t, collectTitles(h.store.GetInstances()), "remote-wedged",
		"the row is not removed on a guess; the snapshot decides")
}

// TestKillTimeout_LocalWedgedDaemonStillOffersRestart guards the other arm of the
// errDaemonUnresponsive branch: the restructure must not change the LOCAL
// behavior, which keeps the more specific in-interface restart offer (#2479)
// rather than the generic "could not be confirmed" message.
func TestKillTimeout_LocalWedgedDaemonStillOffersRestart(t *testing.T) {
	setRemoteTargetForTest(t, false)
	setRestartActionForTest(t, func() error { return nil })

	h := newTestHome(t)
	resizeHome(h, 120, 45)
	target := armKilledInstance(t, h, "local-wedged")

	model, _ := h.Update(instanceKilledMsg{target: target, err: wedgedKillResult()})
	hm := model.(*home)
	require.Equal(t, stateConfirm, hm.state, "a wedged local daemon must still open the restart confirm")
	require.NotNil(t, hm.confirmationOverlay)
	assert.Contains(t, hm.confirmationOverlay.Render(), "Restart it?", "the confirm must offer the restart")
	assert.Nil(t, hm.recovery, "the local restart offer preempts the 'retained' recovery")
	assert.Empty(t, hm.errBox.FullError(), "the restart offer preempts the unknown-outcome message")
}
