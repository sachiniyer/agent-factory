package daemon

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// The #7155 suite: #7144's independent review, F-A arm 1, F-C and F-D.

// postReadinessPaintBackend is a replacement Codex pane that reads as ready
// (its composer) and whose directory-trust dialog paints paintDelay after the
// readiness contract's own dismissal first looked, then stays up until a
// guarded dismissal answers it. A zero paintDelay never paints.
type postReadinessPaintBackend struct {
	*session.FakeBackend
	mu         sync.Mutex
	paintDelay time.Duration
	paintAt    time.Time
	answered   bool
}

func (b *postReadinessPaintBackend) Preview(*session.Instance) (string, error) {
	return "› \n", nil
}

func (b *postReadinessPaintBackend) CheckAndHandleTrustPrompt(*session.Instance) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.paintDelay == 0 || b.answered {
		return false
	}
	if b.paintAt.IsZero() {
		b.paintAt = time.Now().Add(b.paintDelay)
		return false
	}
	if time.Now().Before(b.paintAt) {
		return false
	}
	b.answered = true
	return true
}

func (b *postReadinessPaintBackend) state() (painted, answered bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.paintAt.IsZero() && !time.Now().Before(b.paintAt), b.answered
}

// settleFixture is a tracked Codex row whose pane answers readiness at once.
func settleFixture(t *testing.T, paintDelay time.Duration) (*Manager, *session.Instance, string, string, string, *postReadinessPaintBackend) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	t.Cleanup(task.SetWaitForReadyTimingForTest(5*time.Second, 5*time.Millisecond))
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	backend := &postReadinessPaintBackend{FakeBackend: session.NewFakeBackend(), paintDelay: paintDelay}
	inst.SetBackend(backend)
	return manager, inst, repoID, key, codexHome, backend
}

// TestNonRespawnRecoveryAnswersALateTrustDialogBeforeTheNotice is F-A arm 1.
// A daemon that died after respawning a Codex replacement leaves a live,
// complete pane set; the next pass takes the non-respawn arm, whose readiness
// contract looks once — possibly before the replacement's late dialog paints —
// and then goes straight to the notice. The last trust check before that
// notice must come after the late-paint window, on this arm too.
func TestNonRespawnRecoveryAnswersALateTrustDialogBeforeTheNotice(t *testing.T) {
	const window = 600 * time.Millisecond
	manager, inst, repoID, key, _, backend := settleFixture(t, window/4)
	oldTrust, oldInterval := accountSwapTrustWindow, accountSwapTrustDismissInterval
	accountSwapTrustWindow, accountSwapTrustDismissInterval = window, window/12
	t.Cleanup(func() { accountSwapTrustWindow, accountSwapTrustDismissInterval = oldTrust, oldInterval })

	captureAfterDelivery, err := manager.settleReplacementRuntime(repoID, key, inst.Title, inst,
		&autoAccountSwap{from: "personal", to: "work"}, false, session.ConversationCaptureSnapshot{})
	require.NoError(t, err)
	require.False(t, captureAfterDelivery)
	painted, answered := backend.state()
	require.True(t, answered, "the notice goes out next, so a dialog painting after the readiness check must be answered before this returns")
	require.True(t, painted, "fixture: the dialog painted after the readiness check, inside the window")
}

// TestTheTrustWindowChecksOnceMoreWhenItEnds is F-D's second nit. With no tick
// inside the window, a dialog already up was answered only if the ticker won
// a race against the window's end; the window's end must itself check.
func TestTheTrustWindowChecksOnceMoreWhenItEnds(t *testing.T) {
	inst, codexHome := swapCaptureChannelRow(t)
	blockSwapStoreDay(t, codexHome)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.Error(t, snap.CodexBaselineErr(), "fixture: a failed baseline runs no capture, only the window")
	backend := &latePaintTrustBackend{FakeBackend: session.NewFakeBackend(), paintAt: time.Now()}
	inst.SetBackend(backend)
	oldTrust, oldInterval := accountSwapTrustWindow, accountSwapTrustDismissInterval
	accountSwapTrustWindow, accountSwapTrustDismissInterval = 200*time.Millisecond, time.Hour
	t.Cleanup(func() { accountSwapTrustWindow, accountSwapTrustDismissInterval = oldTrust, oldInterval })

	_, err := captureAccountSwapConversation(inst, snap)
	require.NoError(t, err)
	require.True(t, backend.wasAnswered(), "the window's end is the last trust check before the notice")
}

// TestCarriedRefusalSaysTheCarriedThreadIsRunning is F-C. A carried swap whose
// confirmation could not read its store refuses this attempt, but the copy
// landed and the replacement runs the carried thread; the next attempt takes
// the non-respawn arm and delivers the notice without re-confirming. The error
// must say that, not that the conversation was not preserved.
func TestCarriedRefusalSaysTheCarriedThreadIsRunning(t *testing.T) {
	manager, inst, repoID, key, codexHome, _ := settleFixture(t, 0)
	carriedPath := writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())
	blockSwapStoreDay(t, codexHome)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.Error(t, snap.CodexBaselineErr(), "fixture: the before-image failed")
	snap.ExpectCarriedCodexRolloutForTest(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID, CaptureKind: session.ConversationCaptureCodexRollout,
	}, carriedPath)

	_, err := manager.settleReplacementRuntime(repoID, key, inst.Title, inst,
		&autoAccountSwap{from: "personal", to: "work"}, true, snap)
	require.Error(t, err, "F4: a carried swap still refuses an unreadable store on this attempt")
	require.NotContains(t, err.Error(), "failed to preserve the replacement conversation",
		"the carried thread is what the replacement runs")
	require.Contains(t, err.Error(), "running the carried conversation")
	require.Contains(t, err.Error(), "without re-confirming", "it says what the next attempt does")
}
