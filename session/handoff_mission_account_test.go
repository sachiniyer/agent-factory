package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

func TestMissionBriefSameAgentAccount(t *testing.T) {
	brief := MissionBrief{From: "claude", To: "claude", Goal: "finish migration"}
	require.Equal(t, brief.Goal, brief.Render(), "no work needs only the goal")
	brief.Work = git.WorkSummary{Branch: "migration", HeadSHA: "outgoing-tip", Commits: 2, DirtyFiles: 1}
	rendered := brief.Render()
	require.Contains(t, rendered, brief.Goal)
	require.Contains(t, rendered, "branch migration")
	require.Contains(t, rendered, "outgoing-tip")
	require.Contains(t, rendered, "2 commits, 1 uncommitted file")
	require.Contains(t, rendered, "conversation")
	require.NotContains(t, rendered, "It was being done by claude")
}

func TestMissionBriefSameAgentCarriedConversation(t *testing.T) {
	brief := MissionBrief{
		From: "codex", To: "codex", Goal: "finish migration",
		Work:         git.WorkSummary{Branch: "migration", HeadSHA: "outgoing-tip", Commits: 2},
		Conversation: HandoffConversation{Carried: true},
	}
	rendered := brief.Render()
	require.Contains(t, rendered, "This conversation continues after an account handoff")
	require.Contains(t, rendered, "finish migration", "a --brief override is the one thing the transcript lacks")
	require.NotContains(t, rendered, "not available to you",
		"a carried conversation IS available; the old sentence would now be false")
	require.NotContains(t, rendered, "fresh conversation")

	brief.Goal = ""
	require.Equal(t, "This conversation continues after an account handoff: everything above is still yours to use, "+
		"and only the account changed. Continue from where you left off.", brief.Render())
}

func TestMissionBriefSameAgentCarryFailure(t *testing.T) {
	brief := MissionBrief{
		From: "claude", To: "claude", Goal: "finish migration",
		Conversation: HandoffConversation{CarryFailure: "its transcript is missing from the previous account's home"},
	}
	rendered := brief.Render()
	require.Contains(t, rendered, "This is a fresh conversation after an account handoff. "+
		"af tried to carry the previous conversation over to the new account, but its transcript is missing "+
		"from the previous account's home, so it is not available to you",
		"a failed carry must say it was attempted and why, even when there is no work to summarize")
	require.Contains(t, rendered, "finish migration")
	require.Contains(t, rendered, "Continue from that state")
}

func TestMissionBriefCrossAgentIgnoresCarryOutcome(t *testing.T) {
	plain := MissionBrief{From: "claude", To: "codex", Goal: "finish migration", Reason: HandoffReasonManual}
	for _, conversation := range []HandoffConversation{{Carried: true}, {CarryFailure: "some reason"}} {
		brief := plain
		brief.Conversation = conversation
		require.Equal(t, plain.Render(), brief.Render(),
			"providers cannot read each other's transcripts, so a cross-agent brief never mentions a carry")
	}
}
