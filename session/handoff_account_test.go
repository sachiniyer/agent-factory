package session

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestHandoffAccountPreservesCustomProgram(t *testing.T) {
	inst := handoffTestInstance(t, "claude")
	inst.Program = "claude --model opus"
	inst.Account = "work"
	require.NoError(t, inst.BeginManualAccountSwap())
	entry, err := inst.SelectAccountForHandoff("work", "personal", "claude", HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	require.Equal(t, "claude --model opus", inst.AgentProgram())
	require.Equal(t, "claude --model opus", inst.ToInstanceData().Program)
	require.Equal(t, "claude", entry.From.Agent)
	require.Equal(t, "claude", entry.To)
	require.Equal(t, "work", entry.FromAccount)
	require.Equal(t, "personal", entry.ToAccount)
	require.Len(t, inst.Handoffs(), 1)
	require.NoError(t, inst.RevertHandoff(entry))
	require.Equal(t, "claude --model opus", inst.AgentProgram())
}
