package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalAllowedHost(t *testing.T) {
	ok := []struct{ in, want string }{
		{"siyer-dev-machine.lonk-forel.ts.net", "siyer-dev-machine.lonk-forel.ts.net"},
		{"Box.Example.COM", "box.example.com"},
		{"box.example.com.", "box.example.com"}, // one root dot is the same name
		{"my_container", "my_container"},
		{"192.168.1.5", "192.168.1.5"},
		{"2001:db8::5", "2001:db8::5"},
		{"[2001:DB8::5]", "2001:db8::5"},
		{"::ffff:10.0.0.1", "10.0.0.1"},
		{"localhost", "localhost"},
	}
	for _, tc := range ok {
		got, err := CanonicalAllowedHost(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}

	bad := []struct{ in, mentions string }{
		{"", "empty"},
		{"https://box.example.com", `"box.example.com"`},
		{"https://box.example.com:8443/", `"box.example.com"`},
		{"box.example.com:8443", `"box.example.com"`},
		{"[::1]:8443", "without a port"},
		{"*.ts.net", "wildcard"},
		{"box example", "whitespace"},
		{"box..example", "not a valid host name"},
		{"box.example.com..", "not a valid host name"},
		{"fe80::1%eth0", "zone"},
		{"bad$name", "not a valid host name"},
	}
	for _, tc := range bad {
		_, err := CanonicalAllowedHost(tc.in)
		require.Error(t, err, tc.in)
		assert.Contains(t, err.Error(), AllowedHostsKey, tc.in)
		assert.Contains(t, err.Error(), tc.mentions, tc.in)
	}
}

func TestRequestHostName(t *testing.T) {
	ok := []struct{ in, want string }{
		{"localhost", "localhost"},
		{"localhost:8443", "localhost"},
		{"LOCALHOST:8443", "localhost"},
		{"localhost.:8443", "localhost"},
		{"localhost:", "localhost"}, // empty port: shape-valid, port is dropped anyway
		{"127.0.0.1:8443", "127.0.0.1"},
		{"127.0.0.1.", "127.0.0.1"},
		{"[::1]", "::1"},
		{"[::1]:8443", "::1"},
		{"[::ffff:127.0.0.1]:8443", "127.0.0.1"},
		{"af234567abcdefghijklmnopqrstuvwxyz.localhost:8444", "af234567abcdefghijklmnopqrstuvwxyz.localhost"},
		{"Box.Tailnet.TS.NET", "box.tailnet.ts.net"},
		{"box.tailnet.ts.net.:9100", "box.tailnet.ts.net"},
	}
	for _, tc := range ok {
		got, valid := RequestHostName(tc.in)
		require.True(t, valid, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	// Malformed: none of these is something a browser sends, and each one is
	// refused rather than guessed at.
	for _, in := range []string{
		"",
		"::1",              // IPv6 must be bracketed
		"::1:8443",         // ambiguous
		"[::1",             // unterminated
		"[::1]8443",        // junk after the bracket
		"[127.0.0.1]",      // brackets hold IPv6 only
		"[fe80::1%25eth0]", // a zone
		"localhost:abc",    // non-numeric port
		"local host",
		"evil.example..",
		"a/b",
	} {
		_, valid := RequestHostName(in)
		assert.False(t, valid, "%q must be malformed", in)
	}
}

func TestIsLoopbackHostName(t *testing.T) {
	for _, h := range []string{"localhost", "x.localhost", "af2345.localhost", "127.0.0.1", "127.8.9.10", "::1"} {
		assert.True(t, IsLoopbackHostName(h), h)
	}
	for _, h := range []string{"localhost.evil.example", "evillocalhost", "0.0.0.0", "::", "10.0.0.1", "example.com"} {
		assert.False(t, IsLoopbackHostName(h), h)
	}
}

func TestListenAddrHostName(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8443":  "127.0.0.1",
		"[::1]:8443":      "::1",
		"Box.Lan:8443":    "box.lan",
		"100.64.1.2:8443": "100.64.1.2",
		"0.0.0.0:8443":    "", // a wildcard names no host
		"[::]:8443":       "",
		":8443":           "",
		"":                "",
		"not-an-address":  "",
	}
	for in, want := range cases {
		assert.Equal(t, want, ListenAddrHostName(in), in)
	}
}

// The loader canonicalizes the list and drops — with a warning, not a refused
// start — an entry that is not a host. Fail-closed: a dropped entry admits
// nothing, which is all a malformed one could ever have matched.
func TestAllowedHostsLoadCanonicalizesAndDropsBadEntries(t *testing.T) {
	writeTempConfig(t, `schema_version = 1
[network]
allowed_hosts = ["Box.Tailnet.TS.NET.", "https://typo.example.com", "box.tailnet.ts.net", "10.0.0.7"]
`)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, []string{"box.tailnet.ts.net", "10.0.0.7"}, cfg.AllowedHosts)
}

func TestAllowedHostsSetValidatesAndReplacesTheList(t *testing.T) {
	writeTempConfig(t, "schema_version = 1\n")

	_, err := SetGlobalConfigValue(AllowedHostsKey, "box.tailnet.ts.net:443")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"box.tailnet.ts.net"`, "the refusal names the spelling that works")

	_, err = SetGlobalConfigValue(AllowedHostsKey, "box.tailnet.ts.net, other.example.com")
	require.NoError(t, err)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, []string{"box.tailnet.ts.net", "other.example.com"}, cfg.AllowedHosts)

	// The legacy flat spelling is the same setting.
	_, err = SetGlobalConfigValue("allowed_hosts", "only.example.com")
	require.NoError(t, err)
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, []string{"only.example.com"}, cfg.AllowedHosts)

	_, err = UnsetGlobalConfigValue(AllowedHostsKey)
	require.NoError(t, err)
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.AllowedHosts)
}

func TestAllowedHostsIsAppliedLiveAndGlobalOnly(t *testing.T) {
	assert.Equal(t, EffectAppliedLive, KeyEffectClass(AllowedHostsKey))
	for _, entry := range Manifest() {
		if entry.Key == AllowedHostsKey {
			assert.Equal(t, sourceGlobalOnly, entry.Sources)
			return
		}
	}
	t.Fatalf("%s is not in the manifest", AllowedHostsKey)
}
