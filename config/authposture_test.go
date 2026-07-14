// Load-side tests for the daemon's auth-posture keys (require_token /
// require_loopback_token). They live apart from config_test.go because the posture
// they pin is a security contract in its own right — the daemon's gate derives
// entirely from these two booleans (daemon.webListenerPolicy), so their load
// behavior deserves a file a reader can find by name rather than 1500 lines of
// unrelated config coverage.

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRequireTokenLoadSemantics pins the load behavior for require_token: an
// omitted key keeps the tokenless default (auth is opt-in, so the bundled web UI
// needs no login), and an explicit true turns the token on. The explicit-true case
// is the load-side guarantee behind the daemon gate — an operator who asks for auth
// must actually get it, since nothing else in the stack re-derives that intent.
func TestRequireTokenLoadSemantics(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"absent ⇒ default false (auth is opt-in)", "default_program = 'claude'\n", false},
		{"explicit true", "require_token = true\n", true},
		{"explicit false", "require_token = false\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			fastShell(t)
			configDir, err := GetConfigDir()
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(configDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte(tc.content), 0o644))

			cfg, err := LoadConfig()
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.RequireToken)
		})
	}
}

// TestAuthPostureDefaults states the shipped posture in one place: tokens off,
// loopback exemption on, web listener bound to loopback. The three defaults are a
// single design decision — "the bundled web UI opens with no friction, and the
// loopback bind is what keeps that off the network" — so pin them together. A
// change to any one of them without the others is the dangerous case.
func TestAuthPostureDefaults(t *testing.T) {
	cfg := DefaultConfig()

	assert.False(t, cfg.RequireToken,
		"require_token must default false: auth is opt-in so the web UI needs no login")
	assert.False(t, cfg.RequireLoopbackToken,
		"require_loopback_token must default false: same-machine browsers stay exempt")
	assert.Equal(t, "127.0.0.1:8443", cfg.ListenAddr,
		"listen_addr must default to loopback — it is what bounds the tokenless default")
}
