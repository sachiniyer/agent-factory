package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
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

// TestPrepareLaunchConversationResumesCarriedConversation is the #2616 unit:
// a create handed a prior conversation must come up ON that conversation —
// the recorded id AFTER the launch equal to the id BEFORE — instead of
// injecting a fresh --session-id. The instance id deliberately differs from
// the conversation id here: the root's reap-and-recreate mints a new record,
// so a test that let the two coincide could not tell a carry-over from a
// fresh injection.
func TestPrepareLaunchConversationResumesCarriedConversation(t *testing.T) {
	const priorID = "64ea06ed-7206-7fc2-803b-f7045e07a242"
	inst := &Instance{
		ID:    "5299e00d-1111-4222-8333-f7045e07a242",
		Title: "root",
		carriedConversation: AgentConversationData{
			Agent:       tmux.ProgramClaude,
			ID:          priorID,
			CapturedAt:  time.Date(2026, 7, 26, 21, 35, 39, 0, time.UTC),
			CaptureKind: ConversationCaptureInjected,
		},
	}
	inst.SetTmuxSession(tmux.NewTmuxSession("root", tmux.ProgramClaude))

	got := prepareLaunchConversation(inst, "claude --dangerously-skip-permissions")

	require.Equal(t, "claude --dangerously-skip-permissions --resume "+priorID, got)
	require.Equal(t, priorID, inst.AgentConversation().ID,
		"the re-created session must record the conversation it resumed, not a new one")
	require.Equal(t, tmux.ProgramClaude, inst.AgentConversation().Agent)
	require.NotEqual(t, inst.ID, inst.AgentConversation().ID,
		"a carried conversation must not be overwritten by the fresh --session-id injection")
}

// TestPrepareLaunchConversationIgnoresCarriedConversationForAnotherAgent: the
// carry is only honored when the program will actually run the agent that owns
// the conversation. A root handed off to another agent, or a root_agents
// program pointed at one, must launch fresh rather than have a codex id
// handed to claude.
func TestPrepareLaunchConversationIgnoresCarriedConversationForAnotherAgent(t *testing.T) {
	const instanceID = "5299e00d-1111-4222-8333-f7045e07a242"
	inst := &Instance{
		ID:    instanceID,
		Title: "root",
		carriedConversation: AgentConversationData{
			Agent:       tmux.ProgramCodex,
			ID:          "64ea06ed-7206-7fc2-803b-f7045e07a242",
			CaptureKind: ConversationCaptureCodexRollout,
		},
	}
	inst.SetTmuxSession(tmux.NewTmuxSession("root", tmux.ProgramClaude))

	got := prepareLaunchConversation(inst, "claude --dangerously-skip-permissions")

	require.Equal(t, "claude --dangerously-skip-permissions --session-id "+instanceID, got)
	require.Equal(t, instanceID, inst.AgentConversation().ID)
	require.Equal(t, tmux.ProgramClaude, inst.AgentConversation().Agent)
}

// TestPrepareLaunchConversationLeavesAUserPinnedResumeAlone: a root_agents
// program that already pins its own --resume/--session-id owns the decision.
// ResumeProgramWithConversationID refuses to add a second one, so the launch
// falls through with the command untouched and no conversation recorded.
func TestPrepareLaunchConversationLeavesAUserPinnedResumeAlone(t *testing.T) {
	inst := &Instance{
		ID:    "5299e00d-1111-4222-8333-f7045e07a242",
		Title: "root",
		carriedConversation: AgentConversationData{
			Agent: tmux.ProgramClaude,
			ID:    "64ea06ed-7206-7fc2-803b-f7045e07a242",
		},
	}
	inst.SetTmuxSession(tmux.NewTmuxSession("root", tmux.ProgramClaude))

	got := prepareLaunchConversation(inst, "claude --resume pinned-by-the-user")

	require.Equal(t, "claude --resume pinned-by-the-user", got)
	require.False(t, inst.AgentConversation().HasID())
}
