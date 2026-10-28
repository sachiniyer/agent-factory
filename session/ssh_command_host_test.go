package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

func stubRelay(t *testing.T) {
	t.Helper()
	prev := sshRelayBinary
	sshRelayBinary = func() (string, error) { return "/usr/local/bin/af", nil }
	t.Cleanup(func() { sshRelayBinary = prev })
}

func TestSSHCommandForHostDefaultsToAfComposedStrict(t *testing.T) {
	cmd, err := sshCommandForHost(config.HostConfig{Host: "box1.example", User: "dev", Port: 2222}, "", "", 0)
	require.NoError(t, err)
	assert.Contains(t, cmd, "-F none")
	assert.Contains(t, cmd, "StrictHostKeyChecking=yes")
	assert.Contains(t, cmd, "-p 2222")
	assert.Contains(t, cmd, "User=")
	assert.NotContains(t, cmd, "ProxyCommand")
	assert.Contains(t, cmd, "'box1.example'")
}

func TestSSHCommandForHostPostureOverride(t *testing.T) {
	// The global posture applies when the host sets none; the host wins when it does.
	cmd, err := sshCommandForHost(config.HostConfig{Host: "b"}, config.SSHHostKeyAcceptNew, "", 0)
	require.NoError(t, err)
	assert.Contains(t, cmd, "StrictHostKeyChecking=accept-new")

	cmd, err = sshCommandForHost(config.HostConfig{Host: "b", HostKeyVerification: config.SSHHostKeyStrict}, config.SSHHostKeyInsecure, "", 0)
	require.NoError(t, err)
	assert.Contains(t, cmd, "StrictHostKeyChecking=yes")
	assert.NotContains(t, cmd, "StrictHostKeyChecking=no")
}

func TestSSHCommandForHostPinsOneMachine(t *testing.T) {
	stubRelay(t)
	for _, useConfig := range []bool{false, true} {
		cmd, err := sshCommandForHost(config.HostConfig{Host: "b", UseSSHConfig: useConfig}, "", "10.0.0.7", 2200)
		require.NoError(t, err)
		assert.Contains(t, cmd, "ProxyCommand=", "use_ssh_config=%v", useConfig)
		assert.Contains(t, cmd, "10.0.0.7", "use_ssh_config=%v", useConfig)
		assert.Contains(t, cmd, "2200", "use_ssh_config=%v", useConfig)
	}
}

func TestSSHCommandForHostUseSSHConfigIsTheExplicitOptIn(t *testing.T) {
	cmd, err := sshCommandForHost(config.HostConfig{Host: "bastion-fronted", User: "dev", UseSSHConfig: true, IdentityFile: "/k/id"}, "", "", 0)
	require.NoError(t, err)
	assert.NotContains(t, cmd, "-F none", "the opt-in must let the operator's ssh_config apply")
	assert.NotContains(t, cmd, "StrictHostKeyChecking", "host-key checking is ssh's own under the opt-in")
	assert.NotContains(t, cmd, "UserKnownHostsFile")
	assert.Contains(t, cmd, "BatchMode=yes")
	assert.Contains(t, cmd, "-i '/k/id'")
}

func TestHostConfigRefusesPostureWithUseSSHConfig(t *testing.T) {
	err := config.ValidateHostEntry("a", config.HostConfig{Host: "h", UseSSHConfig: true, HostKeyVerification: config.SSHHostKeyInsecure})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host_key_verification")
	require.Error(t, config.ValidateHostEntry("a", config.HostConfig{Host: "h", HostKeyVerification: "maybe"}))
}
