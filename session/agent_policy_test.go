package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The policy reaches the agent-server by file, on both runtimes, and the file —
// not the host's config — is what the launch names (#5571 W2.5).
func TestAgentServerLaunchNamesThePolicyFile(t *testing.T) {
	spec := ProvisionSpec{Title: "t", AgentPolicy: []byte(`{"version":1}`)}
	docker, err := (&dockerProvisioner{spec: spec}).agentServerCommand()
	require.NoError(t, err)
	ssh, err := (&sandboxWorkspace{spec: spec, SessionDir: "/srv/af-session"}).agentServerCommand()
	require.NoError(t, err)
	assert.Contains(t, docker, "--policy-file "+dockerPolicyPath)
	assert.Contains(t, ssh, "--policy-file /srv/af-session/af-policy.json")

	spec.AgentPolicy = nil
	docker, _ = (&dockerProvisioner{spec: spec}).agentServerCommand()
	assert.NotContains(t, docker, "--policy-file", "no policy, no flag")
}

func TestSandboxWritesThePolicyOverStdinWithUmask077(t *testing.T) {
	remote := &recordedRemote{}
	w := &sandboxWorkspace{
		spec:       ProvisionSpec{AgentPolicy: []byte(`{"version":1,"hooks":{"post_worktree_commands":["make"]}}`)},
		SessionDir: "/srv/af-session",
		shell:      remote,
	}
	require.NoError(t, w.writePolicy(time.Second))
	require.Len(t, remote.scripts, 1)
	assert.True(t, strings.HasPrefix(remote.scripts[0], "umask 077 && cat > "), remote.scripts[0])
	assert.Contains(t, remote.scripts[0], "/srv/af-session/af-policy.json")
	assert.NotContains(t, remote.scripts[0], "make", "hook text travels on stdin, never in the script")
	assert.Contains(t, remote.stdins[0], "post_worktree_commands")

	w.spec.AgentPolicy = nil
	require.NoError(t, w.writePolicy(time.Second))
	assert.Len(t, remote.scripts, 1, "no policy, no write")
}
