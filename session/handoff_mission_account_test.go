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
