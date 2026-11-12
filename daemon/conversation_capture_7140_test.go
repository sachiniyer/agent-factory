package daemon

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The #7140 loop suite: F3 from #7128's independent review, N2 from #7133's.

// TestTheDiffsBlockerNamesTheChannelThatFailed is F3. A diff that could not
// answer because the runtime's open files could not be enumerated (or its
// process generation changed mid-walk) scanned the store fine, so the warning
// must not tell the operator the store cannot be scanned: the wrapped error
// names the channel.
func TestTheDiffsBlockerNamesTheChannelThatFailed(t *testing.T) {
	openFiles := fmt.Errorf("%w: codex conversation capture undecided: could not enumerate the runtime's open files to attribute 1 new rollout files",
		session.ErrCodexCaptureChannelUnavailable)
	why := diffUnanswered(nil, nil, openFiles)
	require.ErrorIs(t, why, openFiles)
	require.NotContains(t, why.Error(), "rollout store", "the store scanned fine; only the open-file walk failed")

	store := errors.New("scan of the Codex rollout store failed: open sessions/2026/11/11: permission denied")
	require.Contains(t, diffUnanswered(nil, nil, store).Error(), "scan of the Codex rollout store failed",
		"a store that failed still says so, through its own error")
	require.Contains(t, diffUnanswered(errors.New("EMFILE"), nil, nil).Error(), "could not be scanned for a baseline")
	require.NoError(t, diffUnanswered(nil, nil, nil))
}

// TestAHeldQueryFailureAlsoSaysWhyTheDiffCouldNotAnswer is N2. When the
// runtime holds two top-level threads, only the diff can tell them apart; if
// the diff could not run — a failed baseline, a failed retake — the held
// query's error was returned alone, so the loop's one warning named the
// ambiguity and never why the channel that resolves it stayed dead.
func TestAHeldQueryFailureAlsoSaysWhyTheDiffCouldNotAnswer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	blockSwapStoreDay(t, codexHome)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.Error(t, snap.CodexBaselineErr(), "fixture: the baseline failed")
	held := &heldOnly{}
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	held.set(
		writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath()),
		writeHeldTestRollout(t, codexHome, heldThirdThreadID, inst.GetWorktreePath()),
	)

	err := manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), conversationCaptureTimeout)
	require.ErrorContains(t, err, "holds 2 top-level rollouts", "fixture: the held query is ambiguous")
	require.ErrorContains(t, err, "could not be scanned for a baseline",
		"the warning must also say why the diff that could resolve it cannot")
	require.False(t, inst.AgentConversation().HasID())
}
