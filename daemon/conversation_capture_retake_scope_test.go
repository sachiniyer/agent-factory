package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The #7160 loop suite: #7145's independent review, R1 and F3-a.

// retakeScopeRow is an unrecorded Codex row whose loop was armed for a store
// other than the one a retake now resolves — a project override's CODEX_HOME at
// the arm, the ambient store once the config degrades — with a failed baseline.
func retakeScopeRow(t *testing.T) (*Manager, *session.Instance, string, string, string, *heldOnly, session.ConversationCaptureSnapshot) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, _ := codexLoopTestInstance(t) // a retake binds the ambient CODEX_HOME
	armedHome := t.TempDir()
	blockSwapStoreDay(t, armedHome)
	loopSnap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(armedHome, inst.GetWorktreePath())
	require.Error(t, loopSnap.CodexBaselineErr(), "fixture: the armed store's baseline failed")
	held := &heldOnly{}
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	return manager, inst, repoID, key, armedHome, held, loopSnap
}

// TestARetakeNeverReScopesTheLoopToAnotherStore is R1. A retake that resolves
// a different Codex store — a project config that turned invalid and degraded
// to the global one, or failed to load — replaced the loop's snapshot, so the
// held query filtered the runtime's real rollout out as "another store", the
// diff watched the wrong store, and every attempt returned nil: nothing
// recorded, nothing logged. A retake may only replace a failed baseline with
// one for the same store and working directory.
func TestARetakeNeverReScopesTheLoopToAnotherStore(t *testing.T) {
	t.Run("the runtime's thread in the armed store still records", func(t *testing.T) {
		manager, inst, repoID, key, armedHome, held, loopSnap := retakeScopeRow(t)
		held.set(writeHeldTestRollout(t, armedHome, heldMissionThreadID, inst.GetWorktreePath()))

		require.NoError(t, manager.captureAgentConversationAttempt(repoID, key, inst, &loopSnap,
			inst.AgentRuntimeToken(), conversationCaptureTimeout, true))
		require.Equal(t, heldMissionThreadID, inst.AgentConversation().ID,
			"the loop still watches the store it was armed for, where the runtime's thread lives")
		require.Error(t, loopSnap.CodexBaselineErr(), "the loop keeps its failed baseline, not another store's good one")
	})

	t.Run("with nothing held yet, the refusal is reported", func(t *testing.T) {
		manager, inst, repoID, key, armedHome, _, loopSnap := retakeScopeRow(t)

		err := manager.captureAgentConversationAttempt(repoID, key, inst, &loopSnap,
			inst.AgentRuntimeToken(), conversationCaptureTimeout, true)
		require.ErrorContains(t, err, armedHome, "the loop's one warning names the store it keeps watching")
		require.ErrorContains(t, err, "keeps the failed baseline")
		require.False(t, inst.AgentConversation().HasID())
	})
}

// TestTheLoopNamesTheDiffsBlockerThroughItsOwnError is F3-a: #7145's F3 is
// pinned at diffUnanswered; this pins the attempt's wiring. A diff that failed
// mid-window on the store scan, with nothing held, reports the diff's own
// error, which names the channel.
func TestTheLoopNamesTheDiffsBlockerThroughItsOwnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	inst.SetCodexRuntimeHeldOverrideForTest((&heldOnly{}).snapshot)
	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.GetWorktreePath())
	require.NoError(t, snap.CodexBaselineErr())
	blockSwapStoreDay(t, codexHome)

	err := manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), conversationCaptureTimeout)
	require.ErrorContains(t, err, "the new-rollout diff could not answer: scan of the Codex rollout store failed")
}
