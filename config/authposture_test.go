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

// TestListenerExposureNotice is the #2168 Phase 0 contract: NO posture is
// refused any more, and the one posture that serves the control API to
// unauthenticated network peers says so.
//
// The owner's decision this pins: "just allow binding to 0.0.0.0 without a
// token. Assume users are safe and will do the right thing." #2090 had made this
// combination a fatal startup refusal; the notice is what replaced it. The
// property that must hold is therefore about the RETURN TYPE as much as the
// table — a string cannot be returned up a call stack as a failure, which is how
// the refusal reached os.Exit(1) and crash-looped the autostart unit (#2168 §1.2).
//
// The require_loopback_token rows are the subtle ones and the reason this table
// exists. That key reads like a second lock, but daemon.webListenerPolicy sets
// tokenDisabled = !RequireToken and tokenDisabled short-circuits the gate, so
// while require_token is false NOTHING is authenticated — require_loopback_token
// only withdraws an exemption that a disabled token already made moot. A notice
// that let require_loopback_token = true excuse a network bind would leave the
// user unwarned about the exact hole #2090 reported, under a config that reads
// secure.
func TestListenerExposureNotice(t *testing.T) {
	cases := []struct {
		name                 string
		listenAddr           string
		requireToken         bool
		requireLoopbackToken bool
		wantNotice           bool
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

			notice := ListenerExposureNotice(cfg)
			if !tc.wantNotice {
				assert.Empty(t, notice, "this posture authenticates network peers (or admits none) — nothing to warn about")
				return
			}
			require.NotEmpty(t, notice, "an unauthenticated network listener must be reported")
			// The warning is only useful if it says what is exposed and how to add
			// auth, so pin both rather than just its existence.
			assert.Contains(t, notice, tc.listenAddr, "name the exposed address")
			assert.Contains(t, notice, "DeliverPrompt", "say what an unauthenticated peer can actually do")
			assert.Contains(t, notice, "require_token = true", "offer the token fix")
			// It reports; it does not forecast a failure that no longer happens.
			assert.NotContains(t, notice, "refus", "the posture is allowed since #2168 Phase 0 — nothing refuses it")
			assert.NotContains(t, notice, "\n", "one line: this goes in a log and a status row")
		})
	}
}

// TestListenerExposureNoticeNilConfig pins the nil case: callers reach this with
// a config they may have failed to load, and a nil-deref there would turn a
// diagnostic into a crash on an unrelated path.
func TestListenerExposureNoticeNilConfig(t *testing.T) {
	assert.Empty(t, ListenerExposureNotice(nil))
}

// TestPreviewListenerExposureNotice pins the preview origin's OWN posture (#1856),
// and what makes it a separate notice from the control-plane one is the point:
// the preview listener never serves the daemon API, so it must NEVER borrow the
// control-plane warning (that would be false, and a false alarm trains an
// operator to ignore the real one). It also does not gate on require_token — that
// key is the control listener's, not the preview origin's.
func TestPreviewListenerExposureNotice(t *testing.T) {
	cases := []struct {
		name              string
		previewListenAddr string
		wantNotice        bool
	}{
		// Off / loopback: nothing off-box, nothing to warn about.
		{"disabled (default)", "", false},
		{"loopback", "127.0.0.1:8444", false},
		{"loopback ipv6", "[::1]:8444", false},
		{"localhost by name", "localhost:8444", false},
		// Network-reachable: warned, regardless of require_token (not the preview
		// origin's key).
		{"all interfaces", "0.0.0.0:8444", true},
		{"unspecified ipv6", "[::]:8444", true},
		{"empty host binds every interface", ":8444", true},
		{"routable ip", "192.168.1.10:8444", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// require_token is deliberately toggled to prove it does NOT govern the
			// preview notice.
			for _, requireToken := range []bool{false, true} {
				cfg := DefaultConfig()
				cfg.PreviewListenAddr = tc.previewListenAddr
				cfg.RequireToken = requireToken

				notice := PreviewListenerExposureNotice(cfg)
				if !tc.wantNotice {
					assert.Empty(t, notice, "a disabled or loopback preview origin has nothing to warn about")
					continue
				}
				require.NotEmpty(t, notice, "a network-reachable preview origin must be reported")
				assert.Contains(t, notice, tc.previewListenAddr, "name the exposed address")
				// It must NOT masquerade as the control-plane warning: DeliverPrompt is
				// the distinctive claim of ListenerExposureNotice, and it must not appear
				// here because this listener never serves it.
				assert.NotContains(t, notice, "DeliverPrompt",
					"the preview origin does not serve the control API — it must not borrow that warning")
				// Instead it states the reassuring invariant positively.
				assert.Contains(t, notice, "never serves the daemon control API",
					"the notice must say the preview origin never carries the control plane, not warn that it does")
				assert.Contains(t, notice, "preview", "name what this listener is")
				assert.NotContains(t, notice, "\n", "one line: this goes in a log and a status row")
			}
		})
	}
}

// TestPreviewListenerExposureNoticeNilConfig mirrors the control-plane nil guard:
// a caller with a config it failed to load must get "", not a crash.
func TestPreviewListenerExposureNoticeNilConfig(t *testing.T) {
	assert.Empty(t, PreviewListenerExposureNotice(nil))
}

// TestDefaultConfigHasNoPreviewExposureNotice pins the common case: the shipped
// default leaves preview_listen_addr empty, so an untouched config is never
// warned about a preview origin it does not have.
func TestDefaultConfigHasNoPreviewExposureNotice(t *testing.T) {
	assert.Empty(t, PreviewListenerExposureNotice(DefaultConfig()),
		"the shipped default disables the preview listener — it must never trip this warning")
}

// TestDefaultConfigHasNoExposureNotice pins the common case: a user who never
// touched these keys is never warned about an exposure they do not have.
func TestDefaultConfigHasNoExposureNotice(t *testing.T) {
	assert.Empty(t, ListenerExposureNotice(DefaultConfig()),
		"the shipped default is loopback-bound — it must never trip the warning it ships with")
}

// TestExposureWarningAgreesWithTheDaemonNotice pins the two user-facing halves of
// the #2090 report to the same predicate: `af config set` warns at write time
// exactly when the daemon will warn at bind time.
//
// Drift here is silently awful in both directions. A set-time warning with no
// daemon notice means the only record of an exposure is a line the user saw once,
// days before it started serving. A daemon notice with no set-time warning means
// `af config set` exits 0, says nothing, and the exposure surfaces only in a log
// file at the next restart — with no memory of which config change caused it.
func TestExposureWarningAgreesWithTheDaemonNotice(t *testing.T) {
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
				cfg.ListenAddr = addr

				warned := exposureWarning(cfg, "listen_addr") != ""

				noticedCfg := DefaultConfig()
				noticedCfg.ListenAddr = addr
				noticedCfg.RequireToken = requireToken
				noticed := ListenerExposureNotice(noticedCfg) != ""

				assert.Equal(t, noticed, warned,
					"set-time warning and daemon notice must fire on exactly the same configs")
			})
		}
	}
}
