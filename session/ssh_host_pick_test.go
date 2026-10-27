package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

func resolvedWithHosts(hosts map[string]config.HostConfig, def string, ssh *config.SSHConfig) *config.ResolvedConfig {
	cfg := &config.ResolvedConfig{}
	cfg.Hosts = hosts
	cfg.DefaultHost = def
	cfg.SSH = ssh
	return cfg
}

func TestChooseListedHost(t *testing.T) {
	hosts := map[string]config.HostConfig{"a": {Host: "a.example"}, "b": {Host: "b.example"}, "cfg": {Host: "c", UseSSHConfig: true}}

	name, h, err := chooseListedHost("b", resolvedWithHosts(hosts, "a", nil))
	require.NoError(t, err)
	assert.Equal(t, "b", name)
	assert.Equal(t, "b.example", h.Host)

	// No pick: the default applies when the repo brings no [ssh] host…
	name, h, err = chooseListedHost("", resolvedWithHosts(hosts, "a", nil))
	require.NoError(t, err)
	assert.Equal(t, "a", name)
	require.NotNil(t, h)

	// …and never overrides a repo that names its own.
	name, h, err = chooseListedHost("", resolvedWithHosts(hosts, "a", &config.SSHConfig{Host: "repo.example"}))
	require.NoError(t, err)
	assert.Empty(t, name)
	assert.Nil(t, h)

	// A pick wins over the repo's own [ssh].
	name, _, err = chooseListedHost("b", resolvedWithHosts(hosts, "", &config.SSHConfig{Host: "repo.example"}))
	require.NoError(t, err)
	assert.Equal(t, "b", name)

	// No pick, no default: the repo path (nil host).
	_, h, err = chooseListedHost("", resolvedWithHosts(hosts, "", nil))
	require.NoError(t, err)
	assert.Nil(t, h)

	// Unknown name lists the configured ones.
	_, _, err = chooseListedHost("nope", resolvedWithHosts(hosts, "", nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"nope"`)
	assert.Contains(t, err.Error(), "[a b cfg]")

	// use_ssh_config is refused, not silently ignored.
	_, _, err = chooseListedHost("cfg", resolvedWithHosts(hosts, "", nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use_ssh_config")
}

func TestResolveBackendKindHostPick(t *testing.T) {
	dir := t.TempDir()
	kind, err := resolveBackendKind(InstanceOptions{Host: "a"}, dir)
	require.NoError(t, err)
	assert.Equal(t, BackendSSH, kind)

	kind, err = resolveBackendKind(InstanceOptions{Host: "a", Backend: BackendSSH}, dir)
	require.NoError(t, err)
	assert.Equal(t, BackendSSH, kind)

	for _, other := range []BackendKind{BackendLocal, BackendDocker, BackendSandbox} {
		_, err = resolveBackendKind(InstanceOptions{Host: "a", Backend: other}, dir)
		require.Error(t, err, "backend %s", other)
		assert.Contains(t, err.Error(), "--host")
	}
}

func TestBackendConfigErrorSSHAcceptsAListedDefaultHost(t *testing.T) {
	hosts := map[string]config.HostConfig{"a": {Host: "a.example"}}
	require.NoError(t, BackendConfigError(BackendSSH, resolvedWithHosts(hosts, "a", nil)))
	require.Error(t, BackendConfigError(BackendSSH, resolvedWithHosts(hosts, "", nil)))
	// A default that is not in the list is not a stand-in for ssh.host.
	require.Error(t, BackendConfigError(BackendSSH, resolvedWithHosts(hosts, "gone", nil)))
}
