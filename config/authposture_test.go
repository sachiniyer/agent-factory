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

// TestListenerExposureNotice pins the exposure notice for the ONE posture it
// still describes since #5137: a serving unauthenticated network bind, which is
// reachable only through the explicit allow_unauthenticated_network opt-in
// (every tokenless non-loopback row below therefore sets it). Without the
// opt-in the posture is refused outright (TestListenerBindRefusal) and the
// notice must stay silent — "af serves" is a lie about a listener that is
// never bound.
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
	}
	// Every row above is wantNotice=false: the only posture that still earns a
	// notice — an OPTED-IN tokenless network bind — needs
	// AllowUnauthenticatedNetwork set, and is exercised by
	// TestListenerExposureNoticeOnlyFiresOnTheOptIn below.
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
			assert.Contains(t, notice, "af config set network.require_token true", "offer the canonical token fix")
			// It reports the opted-in exposure; the refusal is a different message
			// (ListenerBindRefusal) carried by different surfaces.
			assert.NotContains(t, notice, "refus", "the opted-in posture is serving, not refused — the notice must not conflate them")
			assert.NotContains(t, notice, "\n", "one line: this goes in a log and a status row")
		})
	}
}

// TestListenerExposureNoticeOnlyFiresOnTheOptIn pairs the notice with its
// gate: the same tokenless network postures the table above expects warned
// produce NOTHING without allow_unauthenticated_network — the bind is refused
// (TestListenerBindRefusal), so a serving-exposure notice would be a lie.
// require_loopback_token is toggled on one axis too: it is inert while
// require_token is false, so it must neither rescue the refusal nor silence
// the opted-in notice.
func TestListenerExposureNoticeOnlyFiresOnTheOptIn(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8443", "[::]:8443", ":8443", "192.168.1.10:8443"} {
		for _, loopbackToken := range []bool{false, true} {
			cfg := DefaultConfig()
			cfg.ListenAddr = addr
			cfg.RequireToken = false
			cfg.RequireLoopbackToken = loopbackToken
			assert.Empty(t, ListenerExposureNotice(cfg),
				"%q (loopback_token=%v) without the opt-in is refused, not served — no exposure notice", addr, loopbackToken)
			cfg.AllowUnauthenticatedNetwork = true
			assert.NotEmpty(t, ListenerExposureNotice(cfg),
				"%q (loopback_token=%v) with the opt-in IS served unauthenticated — the notice must fire", addr, loopbackToken)
		}
	}
}

// TestListenerBindRefusal is the #5137 contract: a non-loopback listen_addr
// with the token off and no opt-in is refused, in EVERY shape the exposure
// reaches users — wildcard, IPv4, IPv6, empty host, hostname — while loopback,
// token-required, and opted-in postures bind normally.
func TestListenerBindRefusal(t *testing.T) {
	cases := []struct {
		name                    string
		listenAddr              string
		requireToken            bool
		allowUnauthenticatedNet bool
		wantRefused             bool
	}{
		// The refused shapes — every non-loopback form, including the wildcards
		// and a Tailscale CGNAT address (a tailnet is a network like any other).
		{"wildcard ipv4", "0.0.0.0:8443", false, false, true},
		{"wildcard ipv6", "[::]:8443", false, false, true},
		{"empty host binds every interface", ":8443", false, false, true},
		{"routable ipv4", "192.168.1.10:8443", false, false, true},
		{"private ipv4", "10.0.0.5:8443", false, false, true},
		{"tailscale address", "100.83.69.90:8443", false, false, true},
		{"hostname", "myhost.example.com:8443", false, false, true},

		// Allowed: the three named fixes, each on its own.
		{"wildcard with token required", "0.0.0.0:8443", true, false, false},
		{"wildcard with explicit opt-in", "0.0.0.0:8443", false, true, false},
		{"loopback, tokenless", "127.0.0.1:8443", false, false, false},
		{"loopback ipv6, tokenless", "[::1]:8443", false, false, false},
		{"localhost, tokenless", "localhost:8443", false, false, false},
		{"loopback /8 edge, tokenless", "127.42.0.1:8443", false, false, false},
		{"web server disabled", "", false, false, false},
		{"wildcard with token AND opt-in", "0.0.0.0:8443", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ListenAddr = tc.listenAddr
			cfg.RequireToken = tc.requireToken
			cfg.AllowUnauthenticatedNetwork = tc.allowUnauthenticatedNet

			refusal := ListenerBindRefusal(cfg)
			if !tc.wantRefused {
				assert.Empty(t, refusal)
				return
			}
			require.NotEmpty(t, refusal)
			// The SAME message goes everywhere — startup log, config-set error,
			// doctor FAIL, daemon status — so pin the three named fixes and the
			// consequence rather than today's phrasing.
			assert.Contains(t, refusal, tc.listenAddr, "name the refused address")
			assert.Contains(t, refusal, "DeliverPrompt", "say what an unauthenticated peer could reach")
			assert.Contains(t, refusal, "network.require_token true", "fix one: require the token")
			assert.Contains(t, refusal, "af token show", "say where the token comes from")
			assert.Contains(t, refusal, "network.listen_addr 127.0.0.1:8443", "fix two: bind loopback")
			assert.Contains(t, refusal, "network.allow_unauthenticated_network true", "fix three: the explicit opt-in")
			assert.Contains(t, refusal, "unix control socket", "say what still works — the refusal is listener-scoped")
			assert.NotContains(t, refusal, "\n", "one line: this goes in a log and a status row")
		})
	}
	assert.Empty(t, ListenerBindRefusal(nil), "a nil config refuses nothing — it configures nothing")
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

				// And it must state the EXPOSURE, not only the reassurance. This block is
				// the whole reason #3045 existed: the doc comment specified both halves,
				// the string carried only the comforting one ("a network bind gains
				// nothing"), and every assertion above passed anyway — they checked the
				// code that was written rather than the claim the comment made.
				//
				// A notice that understates the risk is worse than no notice, because it
				// converts an unexamined default into an examined and approved one. So
				// these assert the mechanism an operator needs to weigh, each phrased as
				// the fact rather than as today's wording where possible.
				assert.Contains(t, notice, "only credential",
					"the operator must be told the tab hostname is the ONLY thing guarding a preview here")
				assert.Contains(t, notice, "Host: <tab>.localhost",
					"say HOW a non-browser client reaches a per-tab origin, or *.localhost reads as a protection")
				for _, leak := range []string{"log", "screenshot", "browser history"} {
					assert.Containsf(t, notice, leak,
						"name where a hostname realistically leaks (%s) — that is what makes the exposure concrete", leak)
				}
				// The reassuring half must survive too: both are required, and dropping
				// either one is a different bug.
				assert.Contains(t, notice, "gains nothing from the bind",
					"a remote browser genuinely cannot use this bind, and omitting that would overstate the risk")
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
// Both fixtures opt in: since #5137 the warning exists ONLY under
// allow_unauthenticated_network — everywhere else the posture is refused
// outright and exposureWarning defers to listenerWriteRefusal, which runs first
// and errors the write. Setting the opt-in here is what keeps the warn branch
// exercised rather than vacuous.
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
				cfg.AllowUnauthenticatedNetwork = true

				warned := exposureWarning(cfg, "listen_addr") != ""

				noticedCfg := DefaultConfig()
				noticedCfg.ListenAddr = addr
				noticedCfg.RequireToken = requireToken
				noticedCfg.AllowUnauthenticatedNetwork = true
				noticed := ListenerExposureNotice(noticedCfg) != ""

				assert.Equal(t, noticed, warned,
					"set-time warning and daemon notice must fire on exactly the same configs")
			})
		}
	}
}

// The stale-daemon route guard. Every write an old daemon accepts ends in a
// whole-file ApplyConfig, so ANY write can bind a tokenless network listener
// already sitting in the file — the only writes that may take the plain route
// are the ones that force the listener posture safe by themselves.
func TestListenerPostureWriteExposureFailsClosed(t *testing.T) {
	t.Run("only safe-forcing sets stay unguarded", func(t *testing.T) {
		safe := [][2]string{
			{"network.require_token", "true"},
			{"network.require_token", "True"},
			{"network.require_token", "1"},
			{"require_token", "true"}, // the legacy flat alias
			{"network.listen_addr", ""},
			{"network.listen_addr", "127.0.0.1:8443"},
			{"network.listen_addr", "127.53.0.9:8443"},
			{"network.listen_addr", "[::1]:8443"},
			{"network.listen_addr", "localhost:8443"},
			{"listen_addr", "127.0.0.1:8443"}, // the legacy flat alias
		}
		for _, kv := range safe {
			assert.False(t, ListenerPostureWriteExposure(kv[0], kv[1]),
				"%s=%s forces the listener safe — it must keep the plain route", kv[0], kv[1])
		}
	})
	t.Run("every other set is guarded", func(t *testing.T) {
		guarded := [][2]string{
			{"network.listen_addr", "0.0.0.0:8443"},
			{"network.listen_addr", "10.0.0.5:8443"},
			{"network.listen_addr", "100.64.1.2:8443"},
			{"network.listen_addr", "example.com:8443"},
			{"network.listen_addr", ":8443"},
			{"listen_addr", "0.0.0.0:8443"},
			{"network.require_token", "false"},
			{"network.require_token", " false "},
			{"require_token", "0"},
			{"network.require_token", "not-a-bool"}, // unparseable fails closed
			// The whole-file-apply reason: unrelated keys are dangerous the
			// moment the file itself holds the refused posture.
			{"default_program", "claude"},
			{"update_channel", "preview"},
			{"network.allow_unauthenticated_network", "true"},
			{"network.allow_unauthenticated_network", "false"},
		}
		for _, kv := range guarded {
			assert.True(t, ListenerPostureWriteExposure(kv[0], kv[1]),
				"%s=%s does not force the listener safe — it must take the guarded route", kv[0], kv[1])
		}
	})
	t.Run("unset is guarded unless it restores the loopback default", func(t *testing.T) {
		assert.False(t, ListenerPostureUnsetExposure("network.listen_addr"),
			"unsetting listen_addr restores the default loopback bind")
		assert.False(t, ListenerPostureUnsetExposure("listen_addr"),
			"the flat alias unset restores the same default")
		assert.True(t, ListenerPostureUnsetExposure("network.require_token"),
			"unsetting require_token restores the tokenless default")
		assert.True(t, ListenerPostureUnsetExposure("network.allow_unauthenticated_network"),
			"unsetting the opt-in does not itself make a non-loopback tokenless file safe")
		assert.True(t, ListenerPostureUnsetExposure("default_program"),
			"an unrelated unset rides the same whole-file apply")
	})
}
