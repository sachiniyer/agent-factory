package session

import (
	"strconv"
	"strings"
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
			spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
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

// splitWrappedAgentServer returns the extras and the agent-server argv a
// WrapAgentServerAccountCommand-wrapped command carries, so a test can assert
// which admission channel each name rides.
func splitWrappedAgentServer(t *testing.T, wrapped string) (extras, args []string) {
	t.Helper()
	fields := strings.Fields(wrapped)
	require.GreaterOrEqual(t, len(fields), 4, "wrapped argv: %q", wrapped)
	require.Equal(t, sessionenv.AgentServerAccountExecMarker, fields[1], "wrapped argv: %q", wrapped)
	count, err := strconv.Atoi(fields[3])
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(fields), 4+count, "extras past end: %q", wrapped)
	return fields[4 : 4+count], fields[4+count:]
}

// sessionEnvNames collects the names an agent-server argv's --session-env
// pairs admit at the inner pane's filter.
func sessionEnvNames(args []string) map[string]int {
	names := map[string]int{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session-env" {
			names[args[i+1]]++
		}
	}
	return names
}

// #7555 R6/R1b: the keep-if-set names are NAMED to the inner pane's
// --session-env admission so the value decided at the account boundary — a
// forwarded one kept, or the defined-empty pin — reaches the agent. They ride
// --session-env, never --session-env-empty: a re-asserted empty pin at the last
// hop would blank the forwarded value they exist to keep. And they stay OUT of
// the wrap's extras: admitting an unforwarded name there would carry
// repo-controlled ambient state into ScopeAgentServerAccount reading exactly
// like the operator's claim.
func TestAccountBoundAgentServer_LoaderNamesRideSessionEnvOnly(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	loader := sessionenv.AccountLoaderFilledNames("gemini")
	require.NotEmpty(t, loader)
	assertShape := func(t *testing.T, cmd string) {
		t.Helper()
		extras, args := splitWrappedAgentServer(t, cmd)
		for _, name := range loader {
			require.NotContains(t, extras, name,
				"%s in extras would admit repo-controlled ambient state as the operator's claim", name)
			require.NotContains(t, args, "--session-env-empty",
				"--session-env-empty re-asserts an empty pin at the last hop, blanking a kept forwarded value")
		}
		sessionNames := sessionEnvNames(args)
		for _, name := range loader {
			require.Equal(t, 1, sessionNames[name],
				"%s must be named to the inner pane's --session-env admission exactly once: argv %v", name, args)
		}
	}
	t.Run("docker", func(t *testing.T) {
		p := &dockerProvisioner{spec: ProvisionSpec{Title: "t", Account: acct}, program: "gemini"}
		cmd, err := p.agentServerCommand()
		require.NoError(t, err)
		assertShape(t, cmd)
	})
	t.Run("sandbox", func(t *testing.T) {
		w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "gemini",
			spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
		cmd, err := w.agentServerCommand()
		require.NoError(t, err)
		assertShape(t, cmd)
	})
}
