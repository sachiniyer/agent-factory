package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

type modelChangeBackend struct {
	*session.FakeBackend
	change *session.AgentModelChange
}

func (b *modelChangeBackend) AgentModelChange(*session.Instance) *session.AgentModelChange {
	return b.change
}

func TestRefreshStatusPublishesProjectionOnlyModelChange(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &modelChangeBackend{
		FakeBackend: session.NewFakeBackend(),
		change:      session.NewAgentModelChange("gpt-5.6-sol max", "gpt-5.6-luna low"),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "model-change", backend, true, session.Ready)
	_, events := manager.events.subscribe()

	manager.refreshInstanceStatus(repoID, inst)

	updated := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	require.Equal(t, backend.change, updated.ModelChange,
		"a quiet Ready row needs an event even though no durable status changed")
	require.Equal(t, backend.change, manager.Snapshot(repoID)[0].ModelChange)
	require.Equal(t, session.LiveReady, inst.GetLiveness())

	backend.change = nil
	manager.refreshInstanceStatus(repoID, inst)
	cleared := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	require.Nil(t, cleared.ModelChange, "returning to the original model must clear open clients")
	require.Nil(t, inst.AgentModelChange())
}

func TestRuntimeReplacementClearsPriorModelChange(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(
		t, manager, repoID, repoPath, "model-change-replaced", session.NewFakeBackend(), true, session.Ready,
	)
	require.True(t, inst.SetAgentModelChange(
		session.NewAgentModelChange("gpt-5.6-sol max", "gpt-5.6-luna low"),
	))

	manager.noteRuntimeReplaced(repoID, inst)

	require.Nil(t, inst.AgentModelChange(), "a replacement runtime must not inherit its predecessor's warning")
}
