package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The #7131 suite, daemon half: #7127's independent review, R3 and R4.

// TestAFailedBaselineIsRetakenOnlyByAnAttemptThatCanAttribute is R3. The F3
// retake walks the whole account store; an attempt with no verified agent
// process returns before any diff, so a retake there buys nothing — on master
// such a row kept its failed snapshot and walked nothing. The store heals
// between the arm and the attempt, so a retake leaves a sound baseline behind
// and its absence a failed one: what the attempt did to the loop's snapshot is
// the observable.
func TestAFailedBaselineIsRetakenOnlyByAnAttemptThatCanAttribute(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	for _, tc := range []struct {
		name     string
		evidence bool
	}{
		{"no agent process to judge from", false},
		{"an agent process to judge from", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkConversationCaptureForTest(t)
			manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
			if !tc.evidence {
				// No held-set stand-in and a name-only pane: no launch record.
				inst.SetCodexRuntimeHeldOverrideForTest(nil)
			}
			blocked := configAgentCodexDayDir(codexHome, time.Now().AddDate(0, 0, -1))
			require.NoError(t, os.MkdirAll(blocked, 0o755))
			require.NoError(t, os.Chmod(blocked, 0))
			t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
			snap := inst.SessionCodexCaptureBaseline()
			require.Error(t, snap.CodexBaselineErr(), "fixture: the arm-time baseline fails")
			require.NoError(t, os.Chmod(blocked, 0o755), "the store heals before the next re-ask")

			_ = manager.captureAgentConversationAttempt(repoID, key, inst, &snap, inst.AgentRuntimeToken(), conversationCaptureTimeout, true)
			if tc.evidence {
				require.NoError(t, snap.CodexBaselineErr(), "an attempt that can attribute takes the baseline again (F3)")
			} else {
				require.Error(t, snap.CodexBaselineErr(), "an attempt that cannot attribute walks no store")
			}
		})
	}
}

// TestArmedCaptureOfARetiredRowRecordsThroughTheKeptIdentity is R4: the
// population property 2 is for is a retired row — no runtime command, so the
// deferred arm re-resolves its baseline from config rather than from
// RuntimeProgram — armed by the poll, not handed a snapshot. The loop must
// re-verify the kept identity and record the agent's thread.
func TestArmedCaptureOfARetiredRowRecordsThroughTheKeptIdentity(t *testing.T) {
	manager, inst, repoID, _, _, agent, asked := keptEvidenceCodexRow(t)
	require.Empty(t, inst.RuntimeProgram(), "fixture: the retire leaves no runtime command")

	manager.ArmConversationCaptures()
	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == heldUserThreadID
	}, 5*time.Second, 20*time.Millisecond, "the armed loop records the re-verified agent's thread")
	require.Eventually(t, func() bool {
		return persistedConversationID(t, repoID) == heldUserThreadID
	}, 3*time.Second, 20*time.Millisecond)
	row := persistedRow(t, repoID)
	require.Empty(t, row.RuntimeProgram, "capture-only: the retire stays a retire on disk")
	require.Zero(t, row.RuntimePID)
	require.Equal(t, agent.PID, row.CaptureRuntimePID)
	require.Zero(t, asked.Load(), "no attribution asks which pane is active")
}
