package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentModelChangeProjectionIsTypedAndStorageSafe(t *testing.T) {
	inst := &Instance{ID: "session-id", liveness: LiveReady}
	change := NewAgentModelChange("gpt-5.6-sol max", "gpt-5.6-luna low")
	require.NotNil(t, change)
	require.True(t, inst.SetAgentModelChange(change))

	// The live API/CLI projection carries the exact explanation.
	projected := inst.ToInstanceData()
	require.Equal(t, change, projected.ModelChange)
	rawProjection, err := json.Marshal(projected)
	require.NoError(t, err)
	require.Contains(t, string(rawProjection), `"model_change":{"before":"gpt-5.6-sol max","after":"gpt-5.6-luna low"}`)

	// instances.json cannot resurrect a warning derived from a replaced runtime.
	stored := projected.ForStorage()
	require.Nil(t, stored.ModelChange)
	rawStorage, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NotContains(t, string(rawStorage), "model_change")

	// Constructors/setters reject values that do not describe a transition.
	require.Nil(t, NewAgentModelChange("same", "same"))
	require.True(t, inst.SetAgentModelChange(&AgentModelChange{Before: "", After: "unknown"}))
	require.Nil(t, inst.AgentModelChange())
}

func TestAgentModelChangeDoesNotCrossArchiveRestoreRuntimeBoundary(t *testing.T) {
	change := NewAgentModelChange("gpt-5.6-sol max", "gpt-5.6-luna low")
	inst := &Instance{liveness: LiveRunning, started: true}
	require.True(t, inst.SetAgentModelChange(change))

	require.NoError(t, inst.Transition(BeginArchive()))
	require.NoError(t, inst.Transition(CommitArchive()))
	require.Nil(t, inst.AgentModelChange(), "an archived row has no runtime whose diagnostic is current")
	require.Nil(t, inst.ToInstanceData().ModelChange)

	// Also reject a stale projection injected while the row is archived. Restore
	// creates a new runtime under the same Instance identity, so its first snapshot
	// must not inherit a warning observed from the retired process.
	require.False(t, inst.SetAgentModelChange(change), "an archived row must reject runtime diagnostics")
	require.Nil(t, inst.AgentModelChange())

	// Simulate stale in-memory state from an older process/version so BeginRestore
	// itself remains the final boundary guard, independent of setter behavior.
	inst.agentModelChange = cloneAgentModelChange(change)
	require.NoError(t, inst.Transition(BeginRestore()))
	require.Nil(t, inst.AgentModelChange(), "a newly restored runtime must start without its predecessor's diagnostic")
	require.Nil(t, inst.ToInstanceData().ModelChange)
}
