package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// queuedAttentionClearsCounted is queuedAttentionClears that also reports how
// many clears were queued and how many were drained, so a test can assert that
// an unproven delivery queued zero clears at all.
func queuedAttentionClearsCounted(t *testing.T) (drain func() (queued, drained int)) {
	t.Helper()
	var pending []func()
	prev := runAttentionClear
	runAttentionClear = func(clear func()) { pending = append(pending, clear) }
	t.Cleanup(func() { runAttentionClear = prev })
	return func() (queued, drained int) {
		queued = len(pending)
		for len(pending) > 0 {
			next := pending[0]
			pending = pending[1:]
			next()
			drained++
		}
		return queued, drained
	}
}

// An unproven prompt delivery must not clear the attention request.
// SendPromptWithEvidence can return PromptSentUnverified or PromptCouldNotConfirm
// with a nil error — the RPC completed, but the pane receiving the text was not
// proven. Those verdicts are not the user's answer, so the keyed attention clear
// must not fire for them; only a proven delivery (PromptDelivered) settles the
// obligation. The clear is permanent (nothing re-raises a cleared request), so
// firing it on an unproven verdict silently loses the user's answer path and
// leaves the agent blocked with no indicator.
func TestUnprovenPromptDeliveryMustNotClearAttention(t *testing.T) {
	cases := []struct {
		name   string
		status session.PromptDeliveryStatus
	}{
		{"sent-unverified", session.PromptSentUnverified},
		{"could-not-confirm", session.PromptCouldNotConfirm},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drain := queuedAttentionClearsCounted(t)
			manager, repoID, inst := promptTargetWithAttention(t, tc.status)

			var resp SendPromptResponse
			cs := &controlServer{manager: manager}
			require.NoError(t, cs.SendPrompt(SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "answer"}, &resp))
			require.Equal(t, tc.status, resp.Status, "the ambiguous verdict must pass through unchanged")

			queued, drained := drain()
			require.Zero(t, queued, "an unproven delivery must not queue an attention clear at all")
			require.True(t, inst.HasAttention(), "the attention request must remain set")
			_ = drained
		})
	}
}

// A proven delivery (PromptDelivered) still clears the attention request after
// the fix narrows the guard: the happy path is unchanged.
func TestProvenPromptDeliveryStillClearsAttention(t *testing.T) {
	drain := queuedAttentionClearsCounted(t)
	manager, repoID, inst := promptTargetWithAttention(t, session.PromptDelivered)

	var resp SendPromptResponse
	cs := &controlServer{manager: manager}
	require.NoError(t, cs.SendPrompt(SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "answer"}, &resp))
	require.True(t, inst.HasAttention(), "the clear is queued behind the send, not run under its lock")

	queued, _ := drain()
	require.Equal(t, 1, queued, "a proven delivery queues exactly one attention clear")
	require.False(t, inst.HasAttention(), "a proven delivery answers the request")
}
