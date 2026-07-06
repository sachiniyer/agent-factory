package app

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// limitActionInstance builds a started, mock-backed instance blocked at a usage
// limit (#1146) for the manual-retry action tests.
func limitActionInstance(t *testing.T, title string, resetAt time.Time) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetLimitReached(resetAt)
	return inst
}

// TestHandleLimitRetry_NonLimitRow_NoDispatch: pressing c on a session that is
// not usage-limit-blocked surfaces a message and never fires the resume RPC.
func TestHandleLimitRetry_NonLimitRow_NoDispatch(t *testing.T) {
	h := newTestHome(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatus(session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetLimitResumerForTest(func(string, string) error { called = true; return nil })
	defer restore()

	_, _ = h.handleLimitRetry()
	require.False(t, called, "a non-limit row must not dispatch the resume RPC")
}

// TestHandleLimitRetry_LimitRow_Dispatches: pressing c on a limit-blocked row
// dispatches the resume command, which routes through the daemon seam.
func TestHandleLimitRetry_LimitRow_Dispatches(t *testing.T) {
	h := newTestHome(t)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var gotTitle string
	restore := SetLimitResumerForTest(func(title, _ string) error { gotTitle = title; return nil })
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.NotNil(t, cmd, "a limit row must dispatch a resume command")
	msg := cmd()
	done, ok := msg.(limitRetriedMsg)
	require.True(t, ok, "the command must emit limitRetriedMsg")
	require.NoError(t, done.err)
	require.Equal(t, "worker", gotTitle, "the resume command must call the daemon for the selected title")
}

// TestHandleLimitRetry_TearingDownRow_NoDispatch: pressing c on a limit-blocked
// row that is already being deleted must not race a resume RPC against teardown.
func TestHandleLimitRetry_TearingDownRow_NoDispatch(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	inst.SetInFlightOp(session.OpKilling)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetLimitResumerForTest(func(string, string) error { called = true; return nil })
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.False(t, called, "a deleting row must not dispatch the resume RPC")
	require.NotNil(t, cmd, "the user should get a transient error message")
	require.Contains(t, h.errBox.String(), "session 'worker' is being deleted")
}

// TestResumeFromLimitCmd_SurfacesError: a daemon rejection is carried back on the
// completion message (handled into the error box, limit state left intact).
func TestResumeFromLimitCmd_SurfacesError(t *testing.T) {
	h := newTestHome(t)
	restore := SetLimitResumerForTest(func(string, string) error {
		return errors.New("session is not blocked on a usage limit")
	})
	defer restore()

	msg := h.resumeFromLimitCmd("worker")()
	done, ok := msg.(limitRetriedMsg)
	require.True(t, ok)
	require.Error(t, done.err)
}

// TestHandleLimitRetried_ClearsLocally: on a successful retry the local row's
// limit state is cleared immediately, without waiting for the next snapshot.
func TestHandleLimitRetried_ClearsLocally(t *testing.T) {
	h := newTestHome(t)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(inst)
	require.True(t, inst.LimitReached())

	_, _ = h.handleLimitRetried(limitRetriedMsg{title: "worker"})
	require.False(t, inst.LimitReached(), "a successful retry must clear the local limit state")
}
