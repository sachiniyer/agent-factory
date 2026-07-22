package app

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
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
	inst.SetStatusForTest(session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetLimitResumerForTest(func(daemon.ResumeFromLimitRequest) error { called = true; return nil })
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

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.NotNil(t, cmd, "a limit row must dispatch a resume command")
	msg := cmd()
	done, ok := msg.(limitRetriedMsg)
	require.True(t, ok, "the command must emit limitRetriedMsg")
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest,
		"the resume command must preserve the selected session's stable identity")
}

// A manual retry retains its target while the tea.Cmd waits to run. If a
// snapshot replaces the selected row with a different session that reused the
// title in that window, the pending retry must not deliver the old prompt into
// the replacement.
func TestHandleLimitRetry_DoesNotTargetSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.NotNil(t, cmd)

	replacement := limitActionInstance(t, original.Title, time.Now().Add(2*time.Hour))
	require.NotEqual(t, original.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	_ = cmd()
	require.Equal(t, original.ID, gotRequest.ID,
		"the pending retry must retain the original session's stable ID")
	require.NotEqual(t, replacement.ID, gotRequest.ID,
		"a same-title replacement must not inherit the pending retry")
}

// TestHandleLimitRetry_TearingDownRow_NoDispatch: pressing c on a limit-blocked
// row that is already being deleted must not race a resume RPC against teardown.
func TestHandleLimitRetry_TearingDownRow_NoDispatch(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	inst.SetInFlightOpForTest(session.OpKilling)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetLimitResumerForTest(func(daemon.ResumeFromLimitRequest) error { called = true; return nil })
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
	restore := SetLimitResumerForTest(func(daemon.ResumeFromLimitRequest) error {
		return errors.New("session is not blocked on a usage limit")
	})
	defer restore()

	target := sessionActionTarget{id: "worker-id", title: "worker", repoID: h.repoID}
	msg := h.resumeFromLimitCmd(target)()
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

	target := captureSessionActionTarget(inst, h.repoID)
	_, _ = h.handleLimitRetried(limitRetriedMsg{target: target})
	require.False(t, inst.LimitReached(), "a successful retry must clear the local limit state")
}

func TestHandleLimitRetried_DoesNotClearSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(original)
	target := captureSessionActionTarget(original, h.repoID)

	replacement := limitActionInstance(t, original.Title, time.Now().Add(2*time.Hour))
	require.NotEqual(t, original.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	_, _ = h.handleLimitRetried(limitRetriedMsg{target: target})
	require.True(t, replacement.LimitReached(),
		"the old retry completion must not clear a same-title replacement's limit state")
}

func TestHandleLimitRetried_NoOpKeepsLimitLocally(t *testing.T) {
	h := newTestHome(t)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(inst)
	target := captureSessionActionTarget(inst, h.repoID)

	_, _ = h.handleLimitRetried(limitRetriedMsg{target: target, err: errors.New("resume was not performed: another operation owns the retry")})
	require.True(t, inst.LimitReached(), "a daemon no-op must not clear the local limit state")
}
