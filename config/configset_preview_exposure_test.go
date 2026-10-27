package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// exposureWarning (config/configset.go) is the per-write channel that warns at
// `af config set` time — the field SetResult.Warnings carries, which the TUI and web
// render as the fallback when the daemon's apply-time notice is empty. It was gated to
// the two control-plane keys (network.listen_addr / network.require_token), so a write
// to network.preview_listen_addr produced nothing from this channel even when the
// resulting preview listener was network-bound. These tests pin the parallel
// network.preview_listen_addr case added to the switch — it routes to
// PreviewListenerExposureNotice, which is itself non-transition-gated (fires whenever
// preview_listen_addr is non-empty and non-loopback), so the preview key reaches the
// same "warn on every exposed write of the changed key" parity the control plane has.
//
//
// The assertion isolates the preview notice from the control-plane one via the
// preview-specific phrase "web-tab preview origin" — exclusive to
// PreviewListenerExposureNotice — never the shared "reachable from the network" the two
// notices carry in common. A matcher on the shared phrase would be silently satisfied by
// a control-plane notice, masking the gap (per the report's note on the existing
// exposureNotice helper that the control-plane tests use).

// previewWarning returns the entry of warnings carrying the preview-listener notice
// (matched on the preview-specific phrase "web-tab preview origin"), or "" if none. It
// cannot be satisfied by a control-plane ListenerExposureNotice, which omits that phrase.
func previewWarning(warnings []string) string {
	for _, w := range warnings {
		if strings.Contains(w, "web-tab preview origin") {
			return w
		}
	}
	return ""
}

// TestExposureWarningPreviewFiresForNetworkPreviewWrite is the core unit test for the
// per-write channel: a network.preview_listen_addr write whose resulting preview is
// network-bound warns with the preview notice; a loopback or empty preview stays silent.
func TestExposureWarningPreviewFiresForNetworkPreviewWrite(t *testing.T) {
	cases := []struct {
		name              string
		previewListenAddr string
		wantNotice        bool
	}{
		{"loopback", "127.0.0.1:8444", false},
		{"loopback ipv6", "[::1]:8444", false},
		{"localhost by name", "localhost:8444", false},
		{"disabled (empty)", "", false},
		{"all interfaces", "0.0.0.0:8444", true},
		{"unspecified ipv6", "[::]:8444", true},
		{"empty host binds every interface", ":8444", true},
		{"routable ip", "192.168.1.10:8444", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// require_token does NOT gate the preview notice; toggle it to prove the
			// preview case ignores it (parity with PreviewListenerExposureNotice).
			for _, requireToken := range []bool{false, true} {
				cfg := DefaultConfig()
				cfg.PreviewListenAddr = tc.previewListenAddr
				cfg.RequireToken = requireToken
				w := exposureWarning(cfg, "network.preview_listen_addr")
				if !tc.wantNotice {
					require.Empty(t, w,
						"loopback/empty preview must not warn (require_token=%v): %s", requireToken, w)
					continue
				}
				require.NotEmpty(t, w,
					"network-bound preview must warn (require_token=%v)", requireToken)
				require.Containsf(t, w, "web-tab preview origin",
					"the warning must be the preview notice, not the control-plane one: %s", w)
				require.NotContains(t, w, "DeliverPrompt",
					"the preview notice must not borrow the control-plane warning")
				require.NotContains(t, w, "\n",
					"one line: this goes in a save response and a log")
			}
		})
	}
}

// TestExposureWarningPreviewAgreesWithPreviewListenerExposureNotice pins the two
// expressions of the preview exposure to the same predicate, mirroring
// TestExposureWarningAgreesWithTheDaemonNotice for the control plane: the per-write
// warning fires exactly when PreviewListenerExposureNotice would. Drift here means
// either a set-time warning with no daemon notice (false alarm) or a daemon notice with
// no set-time warning (silent exposure) — the exact asymmetry this fix closes.
func TestExposureWarningPreviewAgreesWithPreviewListenerExposureNotice(t *testing.T) {
	addrs := []string{
		"", "127.0.0.1:8444", "[::1]:8444", "localhost:8444",
		"0.0.0.0:8444", "[::]:8444", ":8444", "10.0.0.5:8444", "192.168.1.10:8444",
	}
	for _, addr := range addrs {
		for _, requireToken := range []bool{false, true} {
			name := addr + "/require_token=" + map[bool]string{true: "true", false: "false"}[requireToken]
			t.Run(name, func(t *testing.T) {
				cfg := DefaultConfig()
				cfg.PreviewListenAddr = addr
				cfg.RequireToken = requireToken

				warned := exposureWarning(cfg, "network.preview_listen_addr") != ""
				noticed := PreviewListenerExposureNotice(cfg) != ""
				require.Equal(t, noticed, warned,
					"per-write warning and PreviewListenerExposureNotice must fire on exactly the same configs (require_token=%v)", requireToken)
			})
		}
	}
}

// TestExposureWarningPreviewAliasSpellingCanonicalizes pins that both CLI spellings of
// the preview key warn. The flat spelling "preview_listen_addr" is the legacy wire
// alias; canonicalConfigKey maps it to "network.preview_listen_addr" before the switch,
// so a `af config set preview_listen_addr 0.0.0.0:8444` warns just like the dotted
// spelling. (Mirrors the control-plane alias guarantee.)
func TestExposureWarningPreviewAliasSpellingCanonicalizes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PreviewListenAddr = "0.0.0.0:8444"
	for _, key := range []string{"network.preview_listen_addr", "preview_listen_addr"} {
		require.NotEmpty(t, exposureWarning(cfg, key),
			"both spellings of the preview key must warn, got empty for %q", key)
	}
}

// TestExposureWarningPreviewStaysSilentOnUnrelatedKeys pins the "speaks to the change
// the user just made" discipline: a write to an unrelated key stays silent even when the
// preview IS network-bound, so the warning is not trained away by noise. (Parity with the
// control plane, where an unrelated key never warns.)
func TestExposureWarningPreviewStaysSilentOnUnrelatedKeys(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PreviewListenAddr = "0.0.0.0:8444" // exposed preview
	for _, key := range []string{"default_program", "auto_update", "daemon_poll_interval", "debug_pprof"} {
		require.Empty(t, exposureWarning(cfg, key),
			"an unrelated key must not trigger the preview notice: %s", key)
	}
}

// TestExposureWarningPreviewNotCrossWiredWithControlPlane is the anti-cross-wiring guard
// for the switch: a network.listen_addr write on a config that is ALSO control-plane
// exposed returns the CONTROL-plane notice (DeliverPrompt), never the preview notice,
// even when the preview is network-bound too. The two notices describe different
// listeners and must not be swapped by the key routing.
func TestExposureWarningPreviewNotCrossWiredWithControlPlane(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:8443"
	cfg.RequireToken = false               // control plane exposed
	cfg.PreviewListenAddr = "0.0.0.0:8444" // preview also exposed

	// A control-plane key write returns the control-plane notice.
	w := exposureWarning(cfg, "network.listen_addr")
	require.NotEmpty(t, w, "an exposed control-plane key write must warn")
	require.Contains(t, w, "DeliverPrompt",
		"a control-plane key write must return the control-plane notice, got: %s", w)
	require.Empty(t, previewWarning([]string{w}),
		"a control-plane key write must NOT return the preview notice")

	// A preview key write returns the preview notice, not the control-plane one.
	pw := exposureWarning(cfg, "network.preview_listen_addr")
	require.NotEmpty(t, pw, "an exposed preview key write must warn")
	require.NotContains(t, pw, "DeliverPrompt",
		"a preview key write must NOT return the control-plane notice, got: %s", pw)
	require.NotEmpty(t, previewWarning([]string{pw}),
		"a preview key write must return the preview-specific notice")

	// A require_token write still returns the control-plane notice (the control plane has
	// a pairing; the preview does not), never the preview notice.
	rw := exposureWarning(cfg, "network.require_token")
	require.NotEmpty(t, rw)
	require.Contains(t, rw, "DeliverPrompt")
	require.Empty(t, previewWarning([]string{rw}),
		"network.require_token is a control-plane key and must never route to the preview notice")
}

// TestExposureWarningPreviewLoopbackStaysSilentOnAnyKey guards the over-warning
// direction for the preview: when the preview is loopback/empty, no key write surfaces
// the preview notice. In particular a network.listen_addr write that exposes the CONTROL
// plane must not leak the preview notice alongside it.
func TestExposureWarningPreviewLoopbackStaysSilentOnAnyKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:8443"
	cfg.RequireToken = false                 // control plane exposed
	cfg.PreviewListenAddr = "127.0.0.1:8444" // preview loopback (safe)

	w := exposureWarning(cfg, "network.listen_addr")
	require.NotEmpty(t, w, "the exposed control-plane write must still warn")
	require.Empty(t, previewWarning([]string{w}),
		"a loopback preview must not contribute a preview notice to a control-plane write")
	require.Empty(t, exposureWarning(cfg, "network.preview_listen_addr"),
		"a loopback preview write must not warn")
}

// TestSetGlobalConfigValueWarnsForNetworkPreviewWrite is the end-to-end per-write test:
// `af config set preview_listen_addr 0.0.0.0:8444` (the daemonless write path that also
// feeds channel 1) must populate SetResult.Warnings with the preview notice. Mirrors
// TestSetGlobalConfigValueWarnsFromTheWrittenFile for the control plane. Pre-fix
// exposureWarning's key gate dropped this key and SetResult.Warnings was empty.
func TestSetGlobalConfigValueWarnsForNetworkPreviewWrite(t *testing.T) {
	// Seed: a loopback preview (safe), token on, control plane loopback (not exposed).
	writeTempConfig(t, "default_program = 'claude'\npreview_listen_addr = '127.0.0.1:8444'\nrequire_token = true\n")

	res, err := SetGlobalConfigValue("preview_listen_addr", "0.0.0.0:8444")
	require.NoError(t, err, "setting a network preview address must succeed (warn, never refuse)")
	require.NotNil(t, res)

	w := previewWarning(res.Warnings)
	require.NotEmpty(t, w,
		"moving the preview to a network bind must warn at write time. Warnings:\n%s",
		strings.Join(res.Warnings, "\n"))
	require.Contains(t, w, "0.0.0.0:8444",
		"the warning must name the exposed address, got: %s", w)
	require.NotContains(t, w, "DeliverPrompt",
		"the write-time warning must be the preview notice, not the control-plane one")
}

// TestSetGlobalConfigValuePreviewWarningStaysSilentForLoopback is the over-warning guard
// for the end-to-end path: writing a loopback preview must not warn, even though the
// preview key is now wired into exposureWarning.
func TestSetGlobalConfigValuePreviewWarningStaysSilentForLoopback(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\npreview_listen_addr = '0.0.0.0:8444'\nrequire_token = true\n")

	res, err := SetGlobalConfigValue("preview_listen_addr", "127.0.0.1:8444")
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, previewWarning(res.Warnings),
		"moving the preview back to loopback must not warn. Warnings:\n%s",
		strings.Join(res.Warnings, "\n"))
}

// TestSetGlobalConfigValuePreviewWarnsOnAlreadyExposedMove pins the non-transition-gated
// behavior that distinguishes channel 1 from the apply-time channel 2: an ALREADY
// network-bound preview moved to a DIFFERENT non-loopback address still warns at write
// time, because exposureWarning fires on every exposed network.preview_listen_addr
// write. This is the residual the apply-time transition gate does not cover, and the
// symmetry the control plane already has via its own exposureWarning.
func TestSetGlobalConfigValuePreviewWarnsOnAlreadyExposedMove(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\npreview_listen_addr = '0.0.0.0:8444'\nrequire_token = true\n")

	res, err := SetGlobalConfigValue("preview_listen_addr", "0.0.0.0:8445")
	require.NoError(t, err)
	require.NotNil(t, res)

	w := previewWarning(res.Warnings)
	require.NotEmpty(t, w,
		"moving an already-exposed preview between two non-loopback addresses must still "+
			"warn (per-write channel is not transition-gated). Warnings:\n%s",
		strings.Join(res.Warnings, "\n"))
	require.Contains(t, w, "0.0.0.0:8445",
		"the warning must name the new exposed address, got: %s", w)
}
