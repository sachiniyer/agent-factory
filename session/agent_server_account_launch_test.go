package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// #5863: the agent-server of an account-bound session is launched through the
// account-scoping handoff on every off-box path, and a session without an account
// keeps the plain one. (The shim's behavior is covered in internal/sessionenv.)
func TestAccountBoundAgentServerLaunchScopesTheAmbientEnvironment(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	t.Run("sandbox on a listed host", func(t *testing.T) {
		w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude",
			spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
		cmd, err := w.agentServerCommand()
		require.NoError(t, err)
		assert.Contains(t, cmd, sessionenv.AgentServerAccountExecMarker)
		plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude", spec: ProvisionSpec{Title: "t"}}
		pcmd, err := plain.agentServerCommand()
		require.NoError(t, err)
		assert.NotContains(t, pcmd, sessionenv.AgentServerAccountExecMarker)
		assert.Contains(t, pcmd, sessionenv.AgentServerExecMarker)
	})
	t.Run("docker", func(t *testing.T) {
		p := &dockerProvisioner{spec: ProvisionSpec{Title: "t", Account: acct}, program: "claude"}
		cmd, err := p.agentServerCommand()
		require.NoError(t, err)
		assert.Contains(t, cmd, sessionenv.AgentServerAccountExecMarker)
		plain := &dockerProvisioner{spec: ProvisionSpec{Title: "t"}, program: "claude"}
		pcmd, err := plain.agentServerCommand()
		require.NoError(t, err)
		assert.NotContains(t, pcmd, sessionenv.AgentServerAccountExecMarker)
	})
	// The relaunch after a daemon restart goes through the same agentServerCommand
	// (startAgentServer), so it carries the same marker whenever the workspace holds
	// its account; restoring that metadata on reattach is #5948.
}
