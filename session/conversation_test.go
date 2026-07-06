package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestConversationDataRoundTrip(t *testing.T) {
	capturedAt := time.Date(2026, 7, 6, 10, 17, 35, 0, time.UTC)
	data := InstanceData{
		Title:   "worker",
		Path:    "/tmp/repo",
		Program: tmux.ProgramCodex,
		Tabs: []TabData{
			{
				Name: "agent",
				Kind: TabKindAgent,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          "019f386f-7206-7fc2-803b-f7045e07a242",
					CapturedAt:  capturedAt,
					CaptureKind: ConversationCaptureCodexRollout,
				},
			},
		},
		AgentConversation: &AgentConversationData{
			Agent:       tmux.ProgramCodex,
			ID:          "019f386f-7206-7fc2-803b-f7045e07a242",
			CapturedAt:  capturedAt,
			CaptureKind: ConversationCaptureCodexRollout,
		},
	}

	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"agent_conversation"`)
	require.Contains(t, string(raw), `"conversation"`)

	var restored InstanceData
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.NotNil(t, restored.AgentConversation)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", restored.AgentConversation.ID)
	require.Len(t, restored.Tabs, 1)
	require.NotNil(t, restored.Tabs[0].Conversation)
	require.Equal(t, ConversationCaptureCodexRollout, restored.Tabs[0].Conversation.CaptureKind)
}

func TestConversationDataOmittedWhenEmpty(t *testing.T) {
	raw, err := json.Marshal(InstanceData{
		Title:   "worker",
		Path:    "/tmp/repo",
		Program: tmux.ProgramCodex,
		Tabs:    []TabData{{Name: "agent", Kind: TabKindAgent}},
	})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "agent_conversation")
	require.NotContains(t, string(raw), "conversation")
}

func TestRestoreLocalTabsFallsBackToInstanceConversation(t *testing.T) {
	conv := &AgentConversationData{Agent: tmux.ProgramClaude, ID: "019f386f-7206-7fc2-803b-f7045e07a242"}
	inst := &Instance{}

	restoreLocalTabs(inst, InstanceData{
		Title:             "legacy",
		Program:           tmux.ProgramClaude,
		AgentConversation: conv,
		Tabs:              []TabData{{Name: "agent", Kind: TabKindAgent, TmuxName: "af_legacy_agent"}},
	})

	require.Equal(t, *conv, inst.AgentConversation())
}

func TestPrepareLaunchConversationSeedsClaudeSessionID(t *testing.T) {
	const id = "019f386f-7206-7fc2-803b-f7045e07a242"
	inst := &Instance{ID: id, Title: "worker"}
	inst.SetTmuxSession(tmux.NewTmuxSession("worker", tmux.ProgramClaude))

	got := prepareLaunchConversation(inst, "claude --model sonnet")

	require.Equal(t, "claude --model sonnet --session-id "+id, got)
	conv := inst.AgentConversation()
	require.Equal(t, tmux.ProgramClaude, conv.Agent)
	require.Equal(t, id, conv.ID)
	require.Equal(t, ConversationCaptureInjected, conv.CaptureKind)
	require.False(t, conv.CapturedAt.IsZero())
}
