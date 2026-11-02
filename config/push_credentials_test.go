package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Push credentials (#5571 W7.8) are opt-in in two places: a listed host's
// push_credentials and the free-form target's sandbox.push_credentials. Both are
// off by default, both load from the global config, and neither can come from a
// repository.

func TestPushCredentialsAreOffByDefault(t *testing.T) {
	cfg, err := parseConfigTOML([]byte(`
[hosts.box1]
host = "box1.tailnet.ts.net"
`), "config.toml")
	require.NoError(t, err)
	assert.False(t, cfg.Hosts["box1"].PushCredentials, "listing a host must not opt it into push credentials")
	assert.False(t, cfg.SandboxPushCredentials, "the free-form target is off unless the operator says so")
}

func TestPushCredentialsLoadFromTheGlobalConfig(t *testing.T) {
	cfg, err := parseConfigTOML([]byte(`
[hosts.box1]
host = "box1.tailnet.ts.net"
push_credentials = true

[hosts.box2]
host = "box2.tailnet.ts.net"

[sandbox]
ssh = "ssh box3"
push_credentials = true
`), "config.toml")
	require.NoError(t, err)
	assert.True(t, cfg.Hosts["box1"].PushCredentials)
	assert.False(t, cfg.Hosts["box2"].PushCredentials, "one host's opt-in is not another's")
	assert.True(t, cfg.SandboxPushCredentials, "the grouped spelling sets the runtime field")
}

func TestPushCredentialsSetThroughTheConfigWriter(t *testing.T) {
	writeTempConfig(t, "schema_version = 1\n")
	_, err := SetGlobalConfigValue("hosts", `{"a":{"host":"h","push_credentials":true}}`)
	require.NoError(t, err)
	_, err = SetGlobalConfigValue("sandbox.push_credentials", "true")
	require.NoError(t, err)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Hosts["a"].PushCredentials)
	assert.True(t, cfg.SandboxPushCredentials)

	_, err = SetGlobalConfigValue("sandbox.push_credentials", "false")
	require.NoError(t, err)
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.False(t, cfg.SandboxPushCredentials)
}

// A repository must never grant itself the right to push as the person who
// cloned it: both spellings of the sandbox key, and a host list, are load errors
// in-repo, not silently ignored.
func TestPushCredentialsAreRejectedInRepo(t *testing.T) {
	for name, body := range map[string]string{
		"grouped sandbox key": "[sandbox]\npush_credentials = true\n",
		"flat sandbox key":    "sandbox_push_credentials = true\n",
		"a host list":         "[hosts.box1]\nhost = \"h\"\npush_credentials = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repo := t.TempDir()
			writeInRepoTomlConfig(t, repo, body)
			_, _, err := LoadInRepoConfig(repo)
			require.Error(t, err, "an in-repo push credential grant must not load")
		})
	}
}
