package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestReconcileAgentRuntimeSnapshotMovesEveryIdentitySource(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{
		Title: "snapshot-agent", Path: t.TempDir(), Program: tmux.ProgramClaude,
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("snapshot-agent", tmux.ProgramClaude))
	require.True(t, inst.SetAgentConversation(AgentConversationData{
		Agent: tmux.ProgramClaude, ID: "outgoing-conversation",
	}))
	incoming := AgentConversationData{Agent: tmux.ProgramCodex, ID: "incoming-conversation"}
	data := inst.ToInstanceData()
	data.Program = tmux.ProgramCodex
	data.CurrentAgent = tmux.ProgramCodex
	data.AgentConversation = &incoming
	data.Tabs[0].Conversation = &incoming

	require.True(t, inst.ReconcileAgentRuntimeSnapshot(data))
	require.Equal(t, tmux.ProgramCodex, inst.AgentProgram())
	require.Equal(t, tmux.ProgramCodex, inst.ResolvedPaneProgram())
	require.Equal(t, tmux.ProgramCodex, inst.CurrentAgentName())
	require.Equal(t, incoming, inst.AgentConversation())
	require.False(t, inst.ReconcileAgentRuntimeSnapshot(data), "an identical snapshot is a no-op")
}

func TestReconcileAgentRuntimeSnapshotUsesResolvedAgentForOpaqueProgram(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{
		Title: "snapshot-wrapper", Path: t.TempDir(), Program: tmux.ProgramClaude,
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("snapshot-wrapper", tmux.ProgramClaude))
	require.True(t, inst.SetAgentConversation(AgentConversationData{
		Agent: tmux.ProgramClaude, ID: "outgoing-conversation",
	}))
	data := inst.ToInstanceData()
	data.Program = "/opt/agents/current"
	data.CurrentAgent = tmux.ProgramGemini
	data.AgentConversation = nil
	data.Tabs[0].Conversation = nil

	require.True(t, inst.ReconcileAgentRuntimeSnapshot(data))
	require.Equal(t, "/opt/agents/current", inst.AgentProgram(),
		"the configured command remains exact")
	require.Equal(t, tmux.ProgramGemini, inst.ResolvedPaneProgram(),
		"the client projection retains the daemon's resolved identity")
	require.Equal(t, tmux.ProgramGemini, inst.CurrentAgentName())
	require.True(t, inst.AgentConversation().Empty(),
		"the outgoing conversation must not identify the replacement")
}

func TestReconcileAgentRuntimeSnapshotPreservesUnchangedResolvedCommand(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{
		Title: "snapshot-command", Path: t.TempDir(), Program: tmux.ProgramClaude,
	})
	require.NoError(t, err)
	resolved := "claude --model opus"
	inst.SetTmuxSession(tmux.NewTmuxSession("snapshot-command", resolved))
	data := inst.ToInstanceData()

	require.False(t, inst.ReconcileAgentRuntimeSnapshot(data))
	require.Equal(t, resolved, inst.ResolvedPaneProgram(),
		"an ordinary snapshot must not replace the resolved runtime command with its configured base")
}
