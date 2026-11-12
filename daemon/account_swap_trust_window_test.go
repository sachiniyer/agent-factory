package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7140 swap suite (F2, F4). A replacement Codex launched into an account
// home that has not trusted the worktree can paint its directory-trust dialog
// after the readiness check (#4393), and an automatic swap's notice is sent
// by a path that dismisses nothing (deliverAutomaticAccountNotice). The
// capture window's pump is the only post-readiness dismissal that swap gets,
// so every capture that lets the swap go on to its notice must have pumped
// for the whole window, however soon the capture itself answered.

// latePaintTrustBackend is a replacement pane whose trust dialog paints at
// paintAt, after readiness, and stays up until a guarded dismissal answers it.
type latePaintTrustBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	paintAt  time.Time
	answered bool
}

func (b *latePaintTrustBackend) CheckAndHandleTrustPrompt(*session.Instance) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.answered || time.Now().Before(b.paintAt) {
		return false
	}
	b.answered = true
	return true
}

func (b *latePaintTrustBackend) wasAnswered() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answered
}

// latePaintWindow sets the capture and trust windows, paces the pump inside
// them, and installs a dialog that paints a quarter of the way in.
func latePaintWindow(t *testing.T, inst *session.Instance) (*latePaintTrustBackend, time.Duration) {
	t.Helper()
	const window = 800 * time.Millisecond
	oldTimeout, oldTrust, oldInterval := conversationCaptureTimeout, accountSwapTrustWindow, accountSwapTrustDismissInterval
	conversationCaptureTimeout, accountSwapTrustWindow, accountSwapTrustDismissInterval = window, window, window/16
	t.Cleanup(func() {
		conversationCaptureTimeout, accountSwapTrustWindow, accountSwapTrustDismissInterval = oldTimeout, oldTrust, oldInterval
	})
	backend := &latePaintTrustBackend{FakeBackend: session.NewFakeBackend(), paintAt: time.Now().Add(window / 4)}
	inst.SetBackend(backend)
	return backend, window
}

// shortSwapTrustWindow gives the trust pump a window well inside a test's
// capture window, so the two obligations can be told apart: a swap that
// returns before a third of the capture window did no capture work for it,
// and one that returns no sooner than the trust window still pumped (#7140).
func shortSwapTrustWindow(t *testing.T) time.Duration {
	t.Helper()
	const trust = 300 * time.Millisecond
	oldTrust, oldInterval := accountSwapTrustWindow, accountSwapTrustDismissInterval
	accountSwapTrustWindow, accountSwapTrustDismissInterval = trust, trust/6
	t.Cleanup(func() { accountSwapTrustWindow, accountSwapTrustDismissInterval = oldTrust, oldInterval })
	return trust
}

func TestAccountSwapCaptureAnswersALatePaintedTrustDialogBeforeTheNotice(t *testing.T) {
	t.Run("a fresh swap whose baseline failed", func(t *testing.T) {
		inst, codexHome := swapCaptureChannelRow(t)
		blockSwapStoreDay(t, codexHome)
		snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
		require.Error(t, snap.CodexBaselineErr(), "fixture: the pre-launch baseline must fail")
		backend, _ := latePaintWindow(t, inst)

		captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
		require.NoError(t, err)
		require.True(t, captureAfterDelivery, "the swap still defers its capture")
		require.True(t, backend.wasAnswered(),
			"the notice goes out next and dismisses nothing, so the window must have answered the late dialog")
	})

	t.Run("a carried confirmation, which answers at once", func(t *testing.T) {
		inst, codexHome := swapCaptureChannelRow(t)
		carriedPath := writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())
		snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
		require.NoError(t, snap.CodexBaselineErr())
		snap.ExpectCarriedCodexRolloutForTest(session.AgentConversationData{
			Agent: tmux.ProgramCodex, ID: heldMissionThreadID, CaptureKind: session.ConversationCaptureCodexRollout,
		}, carriedPath)
		backend, _ := latePaintWindow(t, inst)

		captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
		require.NoError(t, err)
		require.False(t, captureAfterDelivery)
		require.Equal(t, heldMissionThreadID, inst.AgentConversation().ID, "the carried thread is confirmed")
		require.True(t, backend.wasAnswered(),
			"a confirmation that answers at once must not cut the window short")
	})

	// The guard: the ordinary fresh swap finds no rollout before its first
	// prompt, so its capture already spends the window pumping.
	t.Run("a fresh swap with no rollout yet", func(t *testing.T) {
		inst, codexHome := swapCaptureChannelRow(t)
		snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
		require.NoError(t, snap.CodexBaselineErr())
		backend, _ := latePaintWindow(t, inst)

		captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
		require.NoError(t, err)
		require.True(t, captureAfterDelivery)
		require.True(t, backend.wasAnswered())
	})
}

// TestCarriedAccountSwapRefusesWhenItsBaselineFailed is F4: the up-front
// deferral for a failed baseline is for fresh swaps only. A carried swap
// whose before-image failed must still refuse — deferring it would skip the
// carried confirmation and let the post-delivery held query record whatever
// the runtime holds, hiding the breakage the carried refusal exists to show.
func TestCarriedAccountSwapRefusesWhenItsBaselineFailed(t *testing.T) {
	inst, codexHome := swapCaptureChannelRow(t)
	carriedPath := writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())
	blockSwapStoreDay(t, codexHome)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.Error(t, snap.CodexBaselineErr(), "fixture: the pre-launch baseline must fail")
	snap.ExpectCarriedCodexRolloutForTest(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID, CaptureKind: session.ConversationCaptureCodexRollout,
	}, carriedPath)

	captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
	require.Error(t, err, "a carried swap must not defer past a before-image it could not take")
	require.False(t, captureAfterDelivery)
	require.False(t, inst.AgentConversation().HasID())
}
