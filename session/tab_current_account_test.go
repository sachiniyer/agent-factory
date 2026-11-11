package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7087: the resolved-handle pointer records which handle a refresh ran on, not
// which ACCOUNT it applied. A handoff to an agent without accounts clears the
// record's account, SwapAgent refreshes the agent handle with that empty
// account, its Start fails, and RevertHandoff restores the account in the
// RECORD only. The handle still carries "" and is still marked resolved, so a
// tab created before Recover ran under the daemon's own identity. A tab spawn
// applies the session's CURRENT account, whatever was last applied to the
// handle it is built from.
func TestTabAfterARevertedHandoffRunsAsTheSessionsAccount(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := sessionEnvHome(t)
	accountDir := registerAccount(t, tmux.ProgramCodex, "work")
	const agentName = "af_reverted_handoff"
	commands := mockRestoredTmux(t, agentName)
	row := loadTestRow(p, "reverted-handoff", agentName, Running, t.TempDir())
	row.Account = "work"
	instance, err := FromInstanceData(row)
	require.NoError(t, err)
	instance.mu.RLock()
	agentTmux := instance.tmuxLocked()
	instance.mu.RUnlock()
	require.Same(t, agentTmux, instance.envResolvedTmux.Load(), "precondition: the load resolved the agent handle")

	// The handoff to a target with no account namespace records the swap and
	// drops the scope; SwapAgent then refreshes the handle with no account
	// before the Start that fails.
	swap, err := instance.SwapAgentProgram(tmux.ProgramAider, HandoffReasonManual, "", false)
	require.NoError(t, err)
	require.Empty(t, instance.Account, "precondition: the handoff dropped the scope")
	require.NoError(t, refreshSessionEnvironment(instance, agentTmux))
	require.NoError(t, instance.RevertHandoff(swap))
	require.Equal(t, "work", instance.Account, "precondition: the revert restored the account in the record")

	tab, err := instance.AddShellTab()
	require.NoError(t, err)

	chain, spawned := spawnChain(commands(), tab.tmux.SanitizedName())
	require.True(t, spawned)
	require.Contains(t, chain, accountDir, "the tab runs as the session's current account, never the daemon's own identity")
}
