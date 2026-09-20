package session

import (
	"testing"
	"time"

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

// The checkpoint projects LiveRunning for a swap still fenced in memory, so it
// must scrub ALL of the outgoing wall's metadata — not only the reset. A
// surviving LimitObservedAt would reload into the running session and suppress
// the first-sighting stamp on the NEXT genuinely-seen wall (#4361 review).
func TestHandoffStorageCheckpointScrubsLimitEvidence(t *testing.T) {
	inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	inst.SetLimitReached(time.Now().Add(72 * time.Hour))
	require.False(t, inst.ToInstanceData().LimitObservedAt.IsZero(),
		"fixture sanity: the parked episode carries a sighting time")

	data := inst.handoffStorageCheckpoint()
	require.Equal(t, LiveRunning, data.Liveness)
	require.True(t, data.LimitResetAt.IsZero())
	require.True(t, data.LimitObservedAt.IsZero(),
		"the checkpoint must not hand the running session a stale sighting")
	require.Empty(t, data.LimitAgent)
	require.Empty(t, data.LimitAccount)
}
