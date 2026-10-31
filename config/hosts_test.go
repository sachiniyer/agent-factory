package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateHostEntry(t *testing.T) {
	ok := HostConfig{Host: "box1.tailnet.ts.net", User: "dev", Port: 2222, IdentityFile: "/home/dev/.ssh/k", WorkspaceRoot: "/srv/w"}
	require.NoError(t, ValidateHostEntry("box1", ok))
	require.NoError(t, ValidateHostEntry("b-2x", HostConfig{Host: "10.0.0.1"}))

	bad := map[string]struct {
		name  string
		entry HostConfig
		want  string
	}{
		"upper name":      {"Box", HostConfig{Host: "h"}, "host name"},
		"underscore name": {"a_b", HostConfig{Host: "h"}, "host name"},
		"tab host":        {"a", HostConfig{Host: "h\tx"}, "hostname or IP"},
		"space user":      {"a", HostConfig{Host: "h", User: "a b"}, "login name"},
		"ctl user":        {"a", HostConfig{Host: "h", User: "a\x01"}, "login name"},
		"rel identity":    {"a", HostConfig{Host: "h", IdentityFile: "k"}, "absolute"},
		"tilde identity":  {"a", HostConfig{Host: "h", IdentityFile: "~/k"}, "absolute"},
		"rel root":        {"a", HostConfig{Host: "h", WorkspaceRoot: "w"}, "absolute"},
		"ctl identity":    {"a", HostConfig{Host: "h", IdentityFile: "/k\x07"}, "control"},
		"proxy host":      {"a", HostConfig{Host: "-oProxyCommand=touch /tmp/x"}, "hostname or IP"},
		"empty name":      {"", HostConfig{Host: "h"}, "must not be empty"},
		"dotted name":     {"a.b", HostConfig{Host: "h"}, "host name"},
		"dash name":       {"-x", HostConfig{Host: "h"}, "host name"},
		"missing host":    {"a", HostConfig{}, "host is required"},
		"option host":     {"a", HostConfig{Host: "-oProxyCommand=x"}, "hostname or IP"},
		"user@ host":      {"a", HostConfig{Host: "me@h"}, "hostname or IP"},
		"space host":      {"a", HostConfig{Host: "h h"}, "hostname or IP"},
		"option user":     {"a", HostConfig{Host: "h", User: "-l"}, "login name"},
		"port range":      {"a", HostConfig{Host: "h", Port: 70000}, "port"},
		"negative port":   {"a", HostConfig{Host: "h", Port: -1}, "port"},
		"dash identity":   {"a", HostConfig{Host: "h", IdentityFile: "-i"}, "absolute"},
		"newline root":    {"a", HostConfig{Host: "h", WorkspaceRoot: "/x\ny"}, "control"},
		"too long name":   {strings.Repeat("a", 64), HostConfig{Host: "h"}, "host name"},
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

// The host address rules, as one table: every address the validator accepts is
// one the ssh runtime's resolver accepts unchanged, and the shapes it refuses are
// the ones that would conflict with the port field or smuggle an ssh option.
func TestValidateHostEntryAddressForms(t *testing.T) {
	for _, addr := range []string{"box1", "box1.tailnet.ts.net", "10.0.0.1", "::1", "::", "::ffff:192.0.2.1", "2001:db8::1", "fe80::1"} {
		require.NoErrorf(t, ValidateHostEntry("a", HostConfig{Host: addr}), "%q must be accepted", addr)
	}
	for _, addr := range []string{"box.example:2222", "[::1]", "[::1]:22", "::1%eth0", "::zz", ":22", "1:2:3", "-::1", "box_1", "a b"} {
		require.Errorf(t, ValidateHostEntry("a", HostConfig{Host: addr}), "%q must be refused", addr)
	}
}

// Set, load and the whole-table writer agree on every host-list shape: a value
// the writer accepts is one the loader keeps, and one the writer refuses is one
// the loader would have dropped.
func TestHostListSetAndLoadAgree(t *testing.T) {
	cases := []struct {
		name  string
		hosts map[string]HostConfig
		def   string
		valid bool
	}{
		{"listed default", map[string]HostConfig{"a": {Host: "h"}}, "a", true},
		{"no default", map[string]HostConfig{"a": {Host: "h"}}, "", true},
		{"dangling default", map[string]HostConfig{"a": {Host: "h"}}, "missing", false},
		{"ipv6 leading colons", map[string]HostConfig{"a": {Host: "::1"}}, "a", true},
		{"embedded port", map[string]HostConfig{"a": {Host: "h:2222", Port: 22}}, "a", false},
		{"option host", map[string]HostConfig{"a": {Host: "-oProxyCommand=x"}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setErr := ValidateHosts(tc.hosts, tc.def)
			require.Equal(t, tc.valid, setErr == nil, "writer verdict: %v", setErr)

			c := &Config{Hosts: map[string]HostConfig{}, DefaultHost: tc.def}
			for k, v := range tc.hosts {
				c.Hosts[k] = v
			}
			sanitizeHosts(c)
			loadKept := len(c.Hosts) == len(tc.hosts) && c.DefaultHost == tc.def
			require.Equal(t, tc.valid, loadKept, "loader kept the list as written")
		})
	}
}

// `af config set default_host <unlisted>` must fail instead of reporting a
// success the loader then discards (#5740).
func TestSetDefaultHostRefusesUnlisted(t *testing.T) {
	writeTempConfig(t, "schema_version = 1\n")
	_, err := SetGlobalConfigValue("hosts", `{"a":{"host":"h"}}`)
	require.NoError(t, err)

	_, err = SetGlobalConfigValue("default_host", "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `default_host "missing"`)

	_, err = SetGlobalConfigValue("default_host", "a")
	require.NoError(t, err)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "a", cfg.DefaultHost)

	// And the list cannot be rewritten out from under its default.
	_, err = SetGlobalConfigValue("hosts", `{"b":{"host":"h"}}`)
	require.Error(t, err)
}

// ValidateHostPick is the one refusal every pick surface shares.
func TestValidateHostPick(t *testing.T) {
	hosts := map[string]HostConfig{"b": {Host: "b.example"}, "a": {Host: "a.example"}}
	require.NoError(t, ValidateHostPick(hosts, ""))
	require.NoError(t, ValidateHostPick(hosts, "a"))
	err := ValidateHostPick(hosts, "c")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `host "c" is not in the host list [a b]`)
	assert.Contains(t, err.Error(), "add it under [hosts.c] in the global config")
	require.Error(t, ValidateHostPick(nil, "a"))
}
