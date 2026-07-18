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

// TestValidateListenerAuthPosture is the #2090 refusal contract: the daemon
// starts in every posture that authenticates network peers (or keeps them out
// entirely) and refuses in the one that does not.
//
// The require_loopback_token rows are the subtle ones and the reason this table
// exists. That key reads like a second lock, but daemon.webListenerPolicy sets
// tokenDisabled = !RequireToken and tokenDisabled short-circuits the gate, so
// while require_token is false NOTHING is authenticated — require_loopback_token
// only withdraws an exemption that a disabled token already made moot. A guard
// that let require_loopback_token = true excuse a network bind would hand back
// the exact hole #2090 reported, under a config that reads secure.
func TestValidateListenerAuthPosture(t *testing.T) {
	cases := []struct {
		name                 string
		listenAddr           string
		requireToken         bool
		requireLoopbackToken bool
		wantRefusal          bool
	}{
		// Safe: nothing off-box can reach a loopback listener, so the tokenless
		// default (the shipped one) stays allowed.
		{"loopback default, tokenless", "127.0.0.1:8443", false, false, false},
		{"loopback ipv6, tokenless", "[::1]:8443", false, false, false},
		{"localhost by name, tokenless", "localhost:8443", false, false, false},
		{"loopback with token", "127.0.0.1:8443", true, false, false},

		// Safe: the web server is off entirely, so there is nothing to expose.
		{"web server disabled", "", false, false, false},

		// Safe: a network bind that actually authenticates its peers.
		{"network bind with token", "0.0.0.0:8443", true, false, false},
		{"routable ip with token", "192.168.1.10:8443", true, false, false},
		{"all interfaces ipv6 with token", "[::]:8443", true, false, false},

		// The #2090 exposure, in each shape it reaches users.
		{"all interfaces, tokenless", "0.0.0.0:8443", false, false, true},
		{"unspecified ipv6, tokenless", "[::]:8443", false, false, true},
		{"empty host binds every interface", ":8443", false, false, true},
		{"routable ip, tokenless", "192.168.1.10:8443", false, false, true},

		// require_loopback_token cannot rescue a tokenless network bind: it is
		// inert while require_token is false.
		{"network, tokenless, loopback token on", "0.0.0.0:8443", false, true, true},
		// ...and it is not needed to permit an authenticated one.
		{"network, token on, loopback token on", "0.0.0.0:8443", true, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ListenAddr = tc.listenAddr
			cfg.RequireToken = tc.requireToken
			cfg.RequireLoopbackToken = tc.requireLoopbackToken

			err := ValidateListenerAuthPosture(cfg)
			if !tc.wantRefusal {
				assert.NoError(t, err, "this posture authenticates network peers (or admits none) and must start")
				return
			}
			require.Error(t, err, "an unauthenticated network listener must refuse to start")
			// The refusal is only useful if it says what to change, so pin the
			// remediation rather than just the failure.
			assert.Contains(t, err.Error(), tc.listenAddr, "name the offending address")
			assert.Contains(t, err.Error(), "af config set require_token true", "offer the token fix")
			assert.Contains(t, err.Error(), "af config set listen_addr 127.0.0.1:8443", "offer the loopback fix")
		})
	}
}

// TestValidateListenerAuthPostureNilConfig pins the nil case: callers pre-flight
// this against a config they may have failed to load, and a nil-deref there would
// turn a security guard into a crash on an unrelated path.
func TestValidateListenerAuthPostureNilConfig(t *testing.T) {
	assert.NoError(t, ValidateListenerAuthPosture(nil))
}

// TestDefaultConfigStartsClean pins the upgrade promise for the common case: a
// user who never touched these keys is not stopped by the new guard.
func TestDefaultConfigStartsClean(t *testing.T) {
	assert.NoError(t, ValidateListenerAuthPosture(DefaultConfig()),
		"the shipped default must never trip the guard it ships with")
}

// TestExposureWarningAgreesWithTheRefusal pins the two halves of the #2090
// defense to the same predicate: `af config set` warns exactly when the daemon
// would refuse to start.
//
// Drift here is silently awful in both directions. A warning without a refusal is
// the pre-#2090 world (told about it, exposed anyway). A refusal without a warning
// is worse UX than it looks: `af config set` exits 0 and says nothing, and the
// daemon only stops at the NEXT restart — possibly days later, with no memory of
// which config change caused it.
func TestExposureWarningAgreesWithTheRefusal(t *testing.T) {
	addrs := []string{
		"127.0.0.1:8443", "[::1]:8443", "localhost:8443", "127.0.0.2:8443",
		"0.0.0.0:8443", "[::]:8443", ":8443", "10.0.0.5:8443", "192.168.1.10:8443", "",
	}
	for _, addr := range addrs {
		for _, requireToken := range []bool{false, true} {
			name := addr + "/require_token=" + map[bool]string{true: "true", false: "false"}[requireToken]
			t.Run(name, func(t *testing.T) {
				cfg := DefaultConfig()
				cfg.RequireToken = requireToken

				warned := exposureWarning(cfg, "listen_addr", addr) != ""

				refusedCfg := DefaultConfig()
				refusedCfg.ListenAddr = addr
				refusedCfg.RequireToken = requireToken
				refused := ValidateListenerAuthPosture(refusedCfg) != nil

				assert.Equal(t, refused, warned,
					"set-time warning and daemon refusal must fire on exactly the same configs")
			})
		}
	}
}
