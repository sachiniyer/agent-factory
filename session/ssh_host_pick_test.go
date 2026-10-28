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

// A listed-host pick must survive the storage round trip, because restore and
// recovery re-provision from the row, not from the (cleared) cleanup handle.
func TestInstanceDataRoundTripsHostPick(t *testing.T) {
	inst := &Instance{Title: "h", Path: t.TempDir(), Host: "box"}
	data := inst.ToInstanceData()
	if data.Host != "box" {
		t.Fatalf("ToInstanceData dropped Host: %q", data.Host)
	}
}

// Create (chooseListedHost), the catalog (HostUnusableReason) and the
// default-host stand-in (BackendConfigError) answer one question, so they agree
// on every entry shape (#5745).
func TestListedHostPredicateAgreesAcrossSurfaces(t *testing.T) {
	restore := SetLookPathForTest(func(string) (string, error) { return "/usr/bin/ssh", nil })
	defer restore()
	cases := []struct {
		name  string
		entry config.HostConfig
		ok    bool
	}{
		{"plain", config.HostConfig{Host: "a.example"}, true},
		{"ipv6", config.HostConfig{Host: "::1"}, true},
		{"embedded port conflicts with port field", config.HostConfig{Host: "box.example:2222", Port: 22}, false},
		{"embedded port", config.HostConfig{Host: "box.example:2222"}, false},
		{"use_ssh_config", config.HostConfig{Host: "a.example", UseSSHConfig: true}, false},
		{"option shaped", config.HostConfig{Host: "-oProxyCommand=x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolvedWithHosts(map[string]config.HostConfig{"h": tc.entry}, "h", nil)
			_, _, createErr := chooseListedHost("h", cfg)
			defaultErr := BackendConfigError(BackendSSH, cfg)
			catalogErr := HostUnusableReason("h", cfg, t.TempDir())
			assert.Equal(t, tc.ok, createErr == nil, "create: %v", createErr)
			assert.Equal(t, tc.ok, defaultErr == nil, "default-host stand-in: %v", defaultErr)
			if !tc.ok {
				require.Error(t, catalogErr)
				assert.Contains(t, catalogErr.Error(), createErr.Error(), "the catalog reports create's own reason")
			}
		})
	}
}

// A default host whose key is unreadable is not usable: the catalog must check
// the host's identity file, not the repo's (absent) one.
func TestBackendUnusableReasonChecksDefaultHostIdentity(t *testing.T) {
	restore := SetLookPathForTest(func(string) (string, error) { return "/usr/bin/ssh", nil })
	defer restore()
	cfg := resolvedWithHosts(map[string]config.HostConfig{"h": {Host: "a.example", IdentityFile: "/nonexistent/af-key"}}, "h", nil)
	err := BackendUnusableReason(BackendSSH, cfg, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity_file", "the key is named, not the (also absent) origin")
}
