package app

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// ----------------------------------------------------------------------------
// Regression tests for issue #844: "make deletion async".
//
// Before the fix, the kill confirmation's OnConfirm ran the whole teardown —
// including the daemon RPC, whose remote delete_cmd often runs over ssh and
// takes tens of seconds — synchronously on the bubbletea Update goroutine,
// freezing the entire TUI. The fix marks the row Deleting synchronously and
// hands the teardown to a background tea.Cmd (killInstanceCmd).
// ----------------------------------------------------------------------------

// setKillerForTest swaps killSessionThroughDaemon and restores it on cleanup.
func setKillerForTest(t *testing.T, f func(title, repoID string) error) {
	t.Helper()
	prev := killSessionThroughDaemon
	killSessionThroughDaemon = func(request daemon.KillSessionRequest) error {
		return f(request.Title, request.RepoID)
	}
	t.Cleanup(func() { killSessionThroughDaemon = prev })
}

// newKillableInstance returns a Ready instance backed by a FakeBackend so no
// real tmux/git resources exist to tear down.
func newKillableInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		return session.NewFakeBackend(), nil
	})
	defer restore()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Ready)
	return inst
}

// confirmKill drives handleKill through the confirmation overlay's 'y' key and
// returns the tea.Cmd produced by the confirm (the pendingConfirmMsg forward).
func confirmKill(t *testing.T, h *home) tea.Cmd {
	t.Helper()
	model, _ := h.handleKill()
	hm := model.(*home)
	require.Equal(t, stateConfirm, hm.state, "kill must open the confirmation dialog")
	require.NotNil(t, hm.confirmationOverlay)
	model, cmd := hm.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.Equal(t, stateDefault, model.(*home).state, "confirm must close the dialog")
	return cmd
}

// TestHandleKill_MarksDeletingWithoutBlockingEventLoop is the core #844 fix:
// confirming a kill must return control to the event loop immediately — with
// the row flipped to Deleting — while the slow daemon teardown is still in
// flight on a background goroutine.
func TestHandleKill_MarksDeletingWithoutBlockingEventLoop(t *testing.T) {
	killStarted := make(chan struct{})
	killBlock := make(chan struct{})
	setKillerForTest(t, func(title, repoID string) error {
		close(killStarted)
		<-killBlock
		return nil
	})

	h := newTestHome(t)
	inst := newKillableInstance(t, "slow-remote")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// The synchronous part of the flow: confirm + the startKillMsg hop. Bound
	// its wall-clock so a regression back to a blocking RPC fails loudly even
	// though the fake kill would deadlock the test anyway (#817-style guard).
	syncStart := time.Now()
	cmd := confirmKill(t, h)
	require.NotNil(t, cmd, "confirm must forward the start-kill message")
	assert.Equal(t, session.Deleting, inst.GetStatus(),
		"row must be marked Deleting synchronously at confirm time")

	msg := cmd()
	startMsg, ok := msg.(startKillMsg)
	require.True(t, ok, "confirm action must emit startKillMsg, got %T", msg)
	_, killCmd := h.Update(startMsg)
	require.NotNil(t, killCmd, "startKillMsg must dispatch the background kill cmd")
	require.Less(t, time.Since(syncStart), 2*time.Second,
		"the event-loop side of kill must not wait for the teardown")

	// Run the teardown cmd the way bubbletea would: on its own goroutine.
	done := make(chan tea.Msg, 1)
	go func() { done <- killCmd() }()

	select {
	case <-killStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("background kill cmd never invoked the daemon kill")
	}

	// Teardown is in flight: the event loop is free, the row is still visible
	// and still Deleting.
	select {
	case <-done:
		t.Fatal("kill cmd completed before the daemon RPC was released")
	default:
	}
	assert.Equal(t, []string{"slow-remote"}, collectTitles(h.store.GetInstances()),
		"row must stay in the sidebar while deletion is in flight")
	assert.Equal(t, session.Deleting, inst.GetStatus())

	// Release the teardown and complete the flow.
	close(killBlock)
	var killedMsg tea.Msg
	select {
	case killedMsg = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("kill cmd did not complete after the daemon RPC returned")
	}
	killed, ok := killedMsg.(instanceKilledMsg)
	require.True(t, ok, "kill cmd must emit instanceKilledMsg, got %T", killedMsg)
	require.NoError(t, killed.err)

	_, _ = h.Update(killed)
	assert.Empty(t, h.store.GetInstances(), "row must be removed once teardown completes")
}

// TestHandleKill_FailureRevertsToReadyAndAllowsRetry: a failed teardown must
// keep the row (so nothing is silently lost), make it retryable again, and
// surface the underlying error to the user.
func TestHandleKill_FailureRevertsToReadyAndAllowsRetry(t *testing.T) {
	killErr := errors.New("delete_cmd failed: ssh: connect to host refused")
	setKillerForTest(t, func(title, repoID string) error { return killErr })

	h := newTestHome(t)
	h.errBox.SetSize(500, 1)
	inst := newKillableInstance(t, "doomed")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	cmd := confirmKill(t, h)
	require.NotNil(t, cmd)
	require.Equal(t, session.Deleting, inst.GetStatus())

	_, killCmd := h.Update(cmd())
	require.NotNil(t, killCmd)
	killed, ok := killCmd().(instanceKilledMsg)
	require.True(t, ok)
	require.ErrorIs(t, killed.err, killErr)

	_, _ = h.Update(killed)

	assert.Equal(t, []string{"doomed"}, collectTitles(h.store.GetInstances()),
		"failed kill must keep the row so the user can retry")
	assert.Equal(t, session.Ready, inst.GetStatus(),
		"failed kill must revert Deleting so kill is enabled again")
	rendered := h.errBox.String()
	assert.Contains(t, rendered, "failed to kill session 'doomed'")
	assert.Contains(t, rendered, "connect to host refused",
		"the underlying cause must be surfaced, not swallowed (#797)")

	// Retry must reach the confirmation dialog again.
	model, _ := h.handleKill()
	assert.Equal(t, stateConfirm, model.(*home).state, "retry kill must work after a failure")
}

// TestHandleKill_DeletingInstanceIsNoOp: pressing D on a row that is already
// mid-deletion must not open a second confirmation or dispatch a second
// teardown — it surfaces a brief message instead.
func TestHandleKill_DeletingInstanceIsNoOp(t *testing.T) {
	setKillerForTest(t, func(title, repoID string) error {
		t.Error("kill must not be dispatched for an already-deleting instance")
		return nil
	})

	h := newTestHome(t)
	h.errBox.SetSize(500, 1)
	inst := newKillableInstance(t, "going-away")
	inst.SetStatusForTest(session.Deleting)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleKill()
	hm := model.(*home)
	assert.Equal(t, stateDefault, hm.state, "no confirmation dialog for a deleting row")
	assert.Nil(t, hm.confirmationOverlay)
	assert.Contains(t, h.errBox.String(), "already being deleted")
}

// TestHandleEnter_DeletingInstanceIsNoOp: attaching to a mid-deletion session
// would race the teardown; Enter must refuse with a brief message.
func TestHandleEnter_DeletingInstanceIsNoOp(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(500, 1)
	inst := newKillableInstance(t, "going-away")
	inst.SetStatusForTest(session.Deleting)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleEnter()
	hm := model.(*home)
	assert.Equal(t, stateDefault, hm.state, "no attach flow for a deleting row")
	assert.Nil(t, hm.textOverlay)
	assert.Contains(t, h.errBox.String(), "is being deleted")
}

// TestInstanceKilled_RowAlreadyRemoved: the 3s external refresh can notice the
// deleted disk record and remove the row before instanceKilledMsg arrives.
// The handler must tolerate the missing row.
func TestInstanceKilled_RowAlreadyRemoved(t *testing.T) {
	h := newTestHome(t)
	_, _ = h.Update(instanceKilledMsg{target: sessionActionTarget{title: "already-gone", repoID: h.repoID}})
	assert.Empty(t, h.store.GetInstances())
}
