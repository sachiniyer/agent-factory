package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// stubPtyBinding records the frames a binding forwards.
type stubPtyBinding struct{ ptyTabBinding }

func (stubPtyBinding) input([]byte) error { return nil }

// queuedAttentionClears collects the keyed clears instead of running them, and
// returns the function that runs them. The surface that queues one can hold the
// session operation lock the clear takes (send-prompt does), so the test drains
// after the surface has returned, as the worker goroutine would.
func queuedAttentionClears(t *testing.T) (drain func()) {
	t.Helper()
	var pending []func()
	prev := runAttentionClear
	runAttentionClear = func(clear func()) { pending = append(pending, clear) }
	t.Cleanup(func() { runAttentionClear = prev })
	return func() {
		for len(pending) > 0 {
			next := pending[0]
			pending = pending[1:]
			next()
		}
	}
}

func promptTargetWithAttention(t *testing.T, status session.PromptDeliveryStatus) (*Manager, string, *session.Instance) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := observedPromptBackend{
		readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()},
		status:           status,
	}
	inst := registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Ready)
	_, err := inst.SetAttention("pick one", time.Now())
	require.NoError(t, err)
	return manager, repoID, inst
}

// Any prompt a user submits answers the request, whichever surface carried it
// (#6284): a terminal Enter, send-prompt over the control socket, and the HTTP
// SendPrompt route the web client and API callers use. The control RPC and the
// HTTP route share one handler, so one case stands for both; the terminal path
// is the PTY binding the web terminal and the TUI attach through. A prompt the
// system delivers is not the user, and neither is one that never arrived.
func TestAttentionIsAnsweredByAPromptTheUserSubmits(t *testing.T) {
	drain := queuedAttentionClears(t)

	t.Run("send-prompt and the web/API route", func(t *testing.T) {
		manager, repoID, inst := promptTargetWithAttention(t, session.PromptDelivered)
		cs := &controlServer{manager: manager}
		var resp SendPromptResponse
		require.NoError(t, cs.SendPrompt(SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "B"}, &resp))
		require.True(t, inst.HasAttention(), "the clear is queued behind the send, not run under its lock")
		drain()
		require.False(t, inst.HasAttention(), "a user's prompt answers the request")
	})

	t.Run("terminal Enter", func(t *testing.T) {
		manager, _, inst := promptTargetWithAttention(t, session.PromptDelivered)
		binding := attentionClearingBinding{
			ptyTabBinding: stubPtyBinding{}, instance: inst,
			clear: func(setAt time.Time) { manager.queueAttentionClear(inst, setAt) },
		}
		require.NoError(t, binding.input([]byte("B")))
		require.True(t, inst.HasAttention(), "typing alone keeps the request")
		require.NoError(t, binding.input([]byte("\r")))
		drain()
		require.False(t, inst.HasAttention(), "a submitted line answers it")
	})

	t.Run("task delivery does not answer", func(t *testing.T) {
		manager, repoID, inst := promptTargetWithAttention(t, session.PromptDelivered)
		_, err := manager.SendPromptWithStatus(SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "tick", TaskOrigin: true})
		require.NoError(t, err)
		drain()
		require.True(t, inst.HasAttention(), "automation is not the user")
	})

	t.Run("an undelivered prompt does not answer", func(t *testing.T) {
		manager, repoID, inst := promptTargetWithAttention(t, session.PromptNotDelivered)
		_, err := manager.SendPromptWithStatus(SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "B"})
		require.NoError(t, err)
		drain()
		require.True(t, inst.HasAttention(), "the answer never arrived")
	})

	t.Run("a newer request survives an answer to the older one", func(t *testing.T) {
		manager, _, inst := promptTargetWithAttention(t, session.PromptDelivered)
		seen := inst.Attention()
		_, err := inst.SetAttention("second question", seen.SetAt.Add(time.Second))
		require.NoError(t, err)
		manager.queueAttentionClear(inst, seen.SetAt) // the clear the first request's answer queued
		drain()
		require.True(t, inst.HasAttention(), "the keyed clear names the request the user saw")
	})
}
