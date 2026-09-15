package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestHandoffCapturedBoundaryStillRollsBackOnLaunchFailure(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	repo := initTempGitRepo(t)
	gitOut(t, repo, "-c", "user.name=Handoff Test", "-c", "user.email=handoff@example.com", "commit", "--allow-empty", "-m", "outgoing work")
	gw, err := git.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	original := inst.ToInstanceData()
	require.NoError(t, inst.Transition(BeginHandoff()))
	entry, err := inst.RecordHandoffSwap(tmux.ProgramGemini, HandoffReasonManual, "", false)
	require.NoError(t, err)
	brief, err := inst.CaptureHandoffBrief(&entry, "finish the work")
	require.NoError(t, err)
	require.NotEmpty(t, brief.Work.HeadSHA)
	require.Equal(t, brief.Work.HeadSHA, entry.HeadSHA)
	require.Equal(t, tmux.ProgramClaude, brief.From)
	require.Equal(t, entry.HeadSHA, inst.handoffStorageCheckpoint().Tabs[0].Handoffs[0].HeadSHA)
	// A launch error after capture uses the updated token to restore the original
	// identity and remove the provisional ledger, without a stale-token failure.
	require.NoError(t, inst.RevertHandoff(entry))
	require.NoError(t, inst.Transition(AbortHandoff()))
	restored := inst.ToInstanceData()
	require.Equal(t, original.Program, restored.Program)
	require.Equal(t, original.Tabs[0].Conversation, restored.Tabs[0].Conversation)
	require.Empty(t, restored.Tabs[0].Handoffs)
}
