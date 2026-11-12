package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7126 suite. The swap's synchronous pre-delivery capture runs after the
// replacement runtime launched, and its error fails the whole settlement —
// "failed to preserve the replacement conversation", resume not performed —
// after the runtime boundary was already crossed. A fresh (non-carried) swap
// must defer to its post-delivery capture when a capture channel is merely
// unavailable, exactly as it does when no rollout exists yet (#4712) or the
// runtime has no launch evidence (#7124); a carried swap keeps refusing a
// store it cannot read.

// swapCaptureChannelRow is a tracked codex row whose replacement runtime has
// an attribution source, and the store its swap captures from.
func swapCaptureChannelRow(t *testing.T) (*session.Instance, string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	_, inst, _, _, codexHome := codexLoopTestInstance(t)
	return inst, codexHome
}

// blockSwapStoreDay makes one day directory inside the scan window of that
// store unreadable.
func blockSwapStoreDay(t *testing.T, codexHome string) {
	t.Helper()
	blocked := configAgentCodexDayDir(codexHome, time.Now().AddDate(0, 0, -1))
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.Chmod(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
}

// TestFreshAccountSwapDefersWhenItsBaselineFailed is #7126's sequence: the
// pre-launch snapshot of the incoming store failed its baseline, the
// replacement launched, and the synchronous capture must neither fail the
// handoff nor re-read the same failure. The window itself is the trust pump's
// (#7140 F2), so the swap returns when it ends — not after a capture's
// timeout plus settle.
func TestFreshAccountSwapDefersWhenItsBaselineFailed(t *testing.T) {
	inst, codexHome := swapCaptureChannelRow(t)
	blockSwapStoreDay(t, codexHome)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.Error(t, snap.CodexBaselineErr(), "fixture: the pre-launch baseline must fail")

	const window = time.Second
	old := conversationCaptureTimeout
	conversationCaptureTimeout = window
	t.Cleanup(func() { conversationCaptureTimeout = old })
	start := time.Now()
	captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
	require.NoError(t, err, "an unavailable capture channel must not fail a committed handoff")
	require.True(t, captureAfterDelivery, "the swap defers to its post-delivery capture")
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, window, "the trust pump owns the whole window")
	require.Less(t, elapsed, window+window/2, "a failed baseline is known before the capture starts, so nothing runs past the window")
	require.False(t, inst.AgentConversation().HasID())
}

// TestFreshAccountSwapDefersWhenTheStoreFailsMidCapture is the same channel
// failing after a good baseline: the store scan errors inside the window.
func TestFreshAccountSwapDefersWhenTheStoreFailsMidCapture(t *testing.T) {
	inst, codexHome := swapCaptureChannelRow(t)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.NoError(t, snap.CodexBaselineErr())
	blockSwapStoreDay(t, codexHome)

	captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
	require.NoError(t, err, "an unavailable capture channel must not fail a committed handoff")
	require.True(t, captureAfterDelivery)
}

// TestCarriedAccountSwapStillRefusesAnUnreadableStore is the guard: a carried
// swap's capture confirms the carried thread, and confirming it past a store
// it cannot enumerate would hide the breakage. That refusal stays.
func TestCarriedAccountSwapStillRefusesAnUnreadableStore(t *testing.T) {
	inst, codexHome := swapCaptureChannelRow(t)
	carriedPath := writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	snap.ExpectCarriedCodexRolloutForTest(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID, CaptureKind: session.ConversationCaptureCodexRollout,
	}, carriedPath)
	blockSwapStoreDay(t, codexHome)

	_, err := captureAccountSwapConversation(inst, snap)
	require.Error(t, err, "a carried confirmation must not hide a broken store")
}

// TestDeferredCaptureReachesTheHeldQueryWhenTheStoreScanFails is where a
// deferral lands (#7128 review). The post-delivery capture reuses the swap's
// snapshot, whose baseline held; when the store scan then fails, the diff's
// error must not end the attempt before the held query — which reads the
// runtime's open files, not the store — gets to answer. Otherwise a deferral
// on an unreadable store records nothing for as long as the store stays
// unreadable, even though the runtime's own rollout is readable.
func TestDeferredCaptureReachesTheHeldQueryWhenTheStoreScanFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	held := &heldOnly{}
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.NoError(t, snap.CodexBaselineErr())
	// The delivered mission minted the replacement's thread, which it holds.
	held.set(writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath()))
	blockSwapStoreDay(t, codexHome)

	require.NoError(t, manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), conversationCaptureTimeout))
	require.Equal(t, heldMissionThreadID, inst.AgentConversation().ID,
		"an unreadable store must not keep the held query from recording the runtime's thread")
	require.Equal(t, heldMissionThreadID, persistedConversationID(t, repoID))
}
