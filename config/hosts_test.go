package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateHostEntry(t *testing.T) {
	ok := HostConfig{Host: "box1.tailnet.ts.net", User: "dev", Port: 2222, IdentityFile: "~/.ssh/k", WorkspaceRoot: "~/w"}
	require.NoError(t, ValidateHostEntry("box1", ok))
	require.NoError(t, ValidateHostEntry("b_2-x", HostConfig{Host: "10.0.0.1"}))

	bad := map[string]struct {
		name  string
		entry HostConfig
		want  string
	}{
		"empty name":    {"", HostConfig{Host: "h"}, "must not be empty"},
		"dotted name":   {"a.b", HostConfig{Host: "h"}, "host name"},
		"dash name":     {"-x", HostConfig{Host: "h"}, "host name"},
		"missing host":  {"a", HostConfig{}, "host is required"},
		"option host":   {"a", HostConfig{Host: "-oProxyCommand=x"}, "hostname or IP"},
		"user@ host":    {"a", HostConfig{Host: "me@h"}, "hostname or IP"},
		"space host":    {"a", HostConfig{Host: "h h"}, "hostname or IP"},
		"option user":   {"a", HostConfig{Host: "h", User: "-l"}, "login name"},
		"port range":    {"a", HostConfig{Host: "h", Port: 70000}, "port"},
		"negative port": {"a", HostConfig{Host: "h", Port: -1}, "port"},
		"dash identity": {"a", HostConfig{Host: "h", IdentityFile: "-i"}, "'-'"},
		"newline root":  {"a", HostConfig{Host: "h", WorkspaceRoot: "x\ny"}, "control"},
		"too long name": {strings.Repeat("a", 64), HostConfig{Host: "h"}, "host name"},
	}
	for label, tc := range bad {
		t.Run(label, func(t *testing.T) {
			err := ValidateHostEntry(tc.name, tc.entry)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateHostsDefaultMustBeListed(t *testing.T) {
	hosts := map[string]HostConfig{"a": {Host: "h"}}
	require.NoError(t, ValidateHosts(hosts, "a"))
	require.NoError(t, ValidateHosts(hosts, ""))
	err := ValidateHosts(hosts, "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `default_host "b"`)
}

func TestSanitizeHostsDropsInvalidAndStaleDefault(t *testing.T) {
	cfg := &Config{
		Hosts:       map[string]HostConfig{"good": {Host: "h"}, "bad": {Host: "-x"}},
		DefaultHost: "bad",
	}
	sanitizeHosts(cfg)
	assert.Contains(t, cfg.Hosts, "good")
	assert.NotContains(t, cfg.Hosts, "bad")
	assert.Empty(t, cfg.DefaultHost)
}

func TestParseConfigTOMLReadsHostList(t *testing.T) {
	cfg, err := parseConfigTOML([]byte(`
default_host = "box1"

[hosts.box1]
host = "box1.tailnet.ts.net"
user = "dev"
port = 2222

[hosts.bad]
host = "-oProxyCommand=x"
`), "config.toml")
	require.NoError(t, err)
	assert.Equal(t, HostConfig{Host: "box1.tailnet.ts.net", User: "dev", Port: 2222}, cfg.Hosts["box1"])
	assert.NotContains(t, cfg.Hosts, "bad")
	assert.Equal(t, "box1", cfg.DefaultHost)
}

func TestHostsWholeTableWriteValidates(t *testing.T) {
	_, _, err := canonicalizeStructuredValue("hosts", `{"a":{"host":"-x"}}`)
	require.Error(t, err)
	canonical, encoded, err := canonicalizeStructuredValue("hosts", `{"a":{"host":"h","port":22}}`)
	require.NoError(t, err)
	assert.Contains(t, canonical, `"host":"h"`)
	assert.Contains(t, encoded, "[hosts]")
}
