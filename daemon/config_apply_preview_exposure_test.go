package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// ApplyConfig routes the web-tab preview listener's network-exposure transition onto
// ApplyConfigResult.Warnings — the channel every config-save surface (CLI `af config
// set`, TUI editor, web toast) renders — closing the asymmetry the live-rebind path in
// #2480 PR2 introduced: the control-plane block in config_apply.go surfaced the MAIN
// listener's transition on the save surface, while the parallel
// config.PreviewListenerExposureNotice for network.preview_listen_addr reached only the
// daemon log (bindPreviewLocked's log.WarningLog.Printf), which no save-surface client
// reads. So an operator moving network.preview_listen_addr from unset/loopback onto the
// network saw NO warning in the save response — silently running a preview listener
// whose per-tab *.localhost hostnames (its only credential) became network-reachable.
//
// These tests pin the apply-time channel (channel 2) and the per-key SaveConfigValue
// save surface that routes both channels (channel 2 in resp.Warnings, channel 1 in
// resp.Result.Warnings). They mirror the control-plane regression suite in
// config_apply_exposure_rebind_bug_test.go, and the assertion isolates the PREVIEW
// notice from the control-plane one: it matches the preview-specific phrase
// "web-tab preview origin" (which never appears in a control-plane notice), NOT the
// shared "reachable from the network" phrase that both ListenerExposureNotice and
// PreviewListenerExposureNotice carry — so a preview-specific assertion is never
// silently satisfied by a control-plane notice, which is exactly the gap a matcher on
// the shared phrase would mask.

// previewNotice returns the save-response warning carrying the preview-listener
// exposure-advisory text, or "" if none is present. It matches the preview-specific
// phrase "web-tab preview origin" (exclusive to config.PreviewListenerExposureNotice),
// so it cannot be satisfied by a control-plane ListenerExposureNotice — the two notices
// share "reachable from the network", but only the preview one names the web-tab origin.
func previewNotice(warnings []string) string {
	for _, w := range warnings {
		if strings.Contains(w, "web-tab preview origin") {
			return w
		}
	}
	return ""
}

// controlPlaneNotice returns the control-plane exposure warning, matching the
// control-plane-exclusive phrase "DeliverPrompt" that the preview notice deliberately
// omits (authposture.go: the preview origin never serves the daemon control API). Used
// as an anti-cross-wiring guard: a preview-key test asserts it picks up the preview
// notice, not the control-plane one masquerading as it.
func controlPlaneNotice(warnings []string) string {
	for _, w := range warnings {
		if strings.Contains(w, "DeliverPrompt") {
			return w
		}
	}
	return ""
}

// previewExposureFixture builds a controlServer whose manager owns REAL bound web
// listeners — the control-plane listener on loopback (so it never produces a
// control-plane notice beside the preview one under test) and the preview listener on
// previewAddr. failNextRebind forces the next rebind to fail (both listeners). Returns
// the server and the kernel-resolved bound preview address ("" when the preview is
// unset/disabled and does not bind). The control-plane listener uses an ephemeral
// loopback port (127.0.0.1:0) so it binds without a fixed-port race; previewAddr
// "127.0.0.1:0"/"0.0.0.0:0" likewise bind ephemeral loopback/without port contention.
func previewExposureFixture(t *testing.T, previewAddr string, failNextRebind bool) (*controlServer, string) {
	t.Helper()
	server := rebindNoticeFixture(t,
		"[network]\nlisten_addr = '127.0.0.1:0'\npreview_listen_addr = '"+previewAddr+"'\n", failNextRebind)
	return server, server.manager.ListenerAddress("network.preview_listen_addr")
}

// runPreviewTransitionTest asserts the apply-time channel (channel 2) fires the
// preview notice on the transition from a non-exposed start (loopback or unset) into a
// network-bound preview, naming the SERVING bound address and never the control-plane
// notice. startPreviewAddr is the preview's pre-apply bind address; name describes it
// for failure messages.
func runPreviewTransitionTest(t *testing.T, startPreviewAddr, name string) {
	t.Helper()
	server, startBound := previewExposureFixture(t, startPreviewAddr, false)
	// anti-vacuous: the start preview must NOT be exposed (absent or loopback).
	if startBound != "" {
		require.True(t, config.IsLoopbackListenAddr(startBound),
			"anti-vacuous: the start preview must be absent or loopback (not exposed), got %s", startBound)
	}

	// Move the preview onto the network. The bind uses an ephemeral port (0.0.0.0:0)
	// and failNextRebind is false, so the rebind SUCCEEDS and the new preview binds a
	// concrete network address.
	setGlobalConfigValue(t, "network.preview_listen_addr", "0.0.0.0:0")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys,
		"the network rebind must succeed (ephemeral port). Warnings:\n%s", strings.Join(result.Warnings, "\n"))

	notice := previewNotice(result.Warnings)
	require.NotEmpty(t, notice,
		"the transition from %s into a network-bound preview must surface the notice on the save surface. Warnings:\n%s",
		name, strings.Join(result.Warnings, "\n"))
	// The notice must NOT be the control-plane one masquerading as the preview notice.
	require.Empty(t, controlPlaneNotice(result.Warnings),
		"a preview-listener transition must not surface the control-plane notice")
	require.NotContains(t, notice, "DeliverPrompt",
		"the preview notice never serves the control plane — it must not borrow that warning")

	// The notice must name the SERVING bound address (the dialable target the daemon is
	// actually accepting on), not the requested "0.0.0.0:0" that resolved to an
	// ephemeral port.
	servingPreview := server.manager.ListenerAddress("network.preview_listen_addr")
	require.NotEmpty(t, servingPreview, "anti-vacuous: the preview must now be serving")
	require.False(t, config.IsLoopbackListenAddr(servingPreview),
		"anti-vacuous: the serving preview must be network-bound (exposed)")
	require.Contains(t, notice, servingPreview,
		"the notice must name the serving bound address, got: %s", notice)
}

// TestApplyConfigPreviewExposureNoticeFiresOnTransitionFromLoopback is the primary
// reproducer (channel 2): a loopback preview listener moves to a network bind. Pre-fix
// the apply-time emitter handled only network.listen_addr, so this transition produced
// NO warning in result.Warnings even though bindPreviewLogged computed the parallel
// notice and dropped it in the daemon log. The fix appends the preview notice to
// result.Warnings on the transition into exposure.
func TestApplyConfigPreviewExposureNoticeFiresOnTransitionFromLoopback(t *testing.T) {
	runPreviewTransitionTest(t, "127.0.0.1:0", "loopback")
}

// TestApplyConfigPreviewExposureNoticeFiresOnTransitionFromUnset is the disabled-start
// variant: an unset preview_listen_addr (the shipped default — the listener is off
// entirely) transitions into a network bind. This is unset-or-loopback → non-loopback,
// the exact transition the report names, and the absent-listener case the gate must
// treat as "not exposed" (preReconcilePreviewAddr is "" so wasPreviewExposed is false).
func TestApplyConfigPreviewExposureNoticeFiresOnTransitionFromUnset(t *testing.T) {
	runPreviewTransitionTest(t, "", "unset/disabled")
}

// TestApplyConfigPreviewExposureNoticeFiresWhenAbsentListenerBecomesNetworkBound is the
// preview analog of the control-plane TestApplyConfigExposureNoticeFiresWhenAbsentListenerBecomesExposed:
// when the preview listener machinery exists but no socket is accepting (an unexpected
// Serve exit cleared previewBoundAddr/previewConfigAddr), the pre-reconcile serving
// posture is "no preview" — NOT the old.PreviewListenAddr config value. A subsequent
// apply that re-establishes a network-bound preview must fire the notice (the transition
// from absent to exposed), and pre-fix would have been suppressed if wasPreviewExposed
// had been computed from old.PreviewListenAddr (the never-bound network address).
func TestApplyConfigPreviewExposureNoticeFiresWhenAbsentListenerBecomesNetworkBound(t *testing.T) {
	// Start from a network-bound preview so old.PreviewListenAddr is a non-loopback
	// address — the value that a pre-fix emitter keyed on old.PreviewListenAddr would
	// have mistaken for "already exposed".
	server, oldBound := previewExposureFixture(t, "0.0.0.0:0", false)
	require.False(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the start address must be non-loopback so the absent state is the point")

	// Simulate an unexpected Serve exit that cleared both previewBoundAddr and
	// previewConfigAddr: the listener machinery still exists, but no socket accepts.
	server.manager.webListeners.mu.Lock()
	server.manager.webListeners.previewBoundAddr = ""
	server.manager.webListeners.previewConfigAddr = ""
	server.manager.webListeners.mu.Unlock()
	require.Empty(t, server.manager.ListenerAddress("network.preview_listen_addr"),
		"anti-vacuous: the absent-preview state must be visible through ListenerAddress")

	// Re-establish the preview at a network address. The on-disk value is already
	// "0.0.0.0:0", but clearing previewConfigAddr makes reconcile see a diff and rebind.
	setGlobalConfigValue(t, "network.preview_listen_addr", "0.0.0.0:0")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys, "the rebind to the restored preview must succeed")

	newBound := server.manager.ListenerAddress("network.preview_listen_addr")
	require.NotEmpty(t, newBound, "anti-vacuous: the preview must now be serving")
	require.False(t, config.IsLoopbackListenAddr(newBound),
		"anti-vacuous: the restored serving address must be non-loopback (exposed)")

	// The notice MUST fire: this is a transition from "no preview listener" to an exposed
	// one. Computing wasPreviewExposed from old.PreviewListenAddr (the never-bound network
	// address) would have yielded true and suppressed it.
	notice := previewNotice(result.Warnings)
	require.NotEmpty(t, notice,
		"the notice must fire for the transition from an absent preview to a network-bound one. Warnings:\n%s",
		strings.Join(result.Warnings, "\n"))
	require.Contains(t, notice, newBound,
		"the notice must name the newly serving bound address, got: %s", notice)
}

// TestApplyConfigPreviewExposureNoticeSuppressedOnFailedRebindFromLoopback is the
// preview analog of control-plane case 1 (TestApplyConfigSuppressesExposureNoticeOnFailedRebindFromLoopback):
// a NOT-exposed loopback preview moves to a network address whose rebind FAILS. The
// old loopback preview keeps serving (still not exposed), so the preview notice must NOT
// fire — keying the notice on the REQUESTED network address would be a false positive
// contradicting the sibling rebind-failure warning. The fix keys on the SERVING
// (loopback) bound address, so servingPreviewExposed is false and the gate suppresses.
func TestApplyConfigPreviewExposureNoticeSuppressedOnFailedRebindFromLoopback(t *testing.T) {
	server, oldBound := previewExposureFixture(t, "127.0.0.1:0", true)
	require.True(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the still-serving preview must be loopback (not exposed)")

	// Move the preview onto the network — the rebind FAILS (forced), so the old loopback
	// listener keeps serving.
	setGlobalConfigValue(t, "network.preview_listen_addr", "0.0.0.0:8444")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.FailedListenerKeys, "network.preview_listen_addr")
	require.NotContains(t, result.Applied, "network.preview_listen_addr",
		"a key whose rebind failed must not be reported as applied")
	require.True(t, hasRebindFailure(result.Warnings),
		"anti-vacuous: the rebind must have actually failed and warned")
	require.Equal(t, oldBound, server.manager.ListenerAddress("network.preview_listen_addr"),
		"the failed rebind must leave the old loopback preview as the serving (bound) address")

	// The preview notice must NOT fire: the daemon is serving a loopback preview, not
	// exposed, regardless of the requested network address.
	require.Empty(t, previewNotice(result.Warnings),
		"the preview notice must not fire when the still-serving preview is loopback after a failed rebind. Warnings:\n%s",
		strings.Join(result.Warnings, "\n"))
}

// TestApplyConfigPreviewExposureNoticeDoesNotResurfaceWhenAlreadyExposed pins the
// transition gate (warning fatigue): an already network-bound preview that attempts to
// move BETWEEN two non-loopback addresses (rebind FAILS, here) must NOT resurface the
// notice — the transition into exposure happened before this save, not now. The control
// plane's parallel case is TestApplyConfigExposureNoticeDoesNotResurfaceAfterFailedRebindFromExposed.
func TestApplyConfigPreviewExposureNoticeDoesNotResurfaceWhenAlreadyExposed(t *testing.T) {
	server, oldBound := previewExposureFixture(t, "0.0.0.0:0", true)
	require.False(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the start preview must be non-loopback (already exposed)")

	// Move to a DIFFERENT non-loopback address — the rebind FAILS, so the old
	// network-bound listener keeps serving.
	setGlobalConfigValue(t, "network.preview_listen_addr", "0.0.0.0:8444")
	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.FailedListenerKeys, "network.preview_listen_addr")
	require.True(t, hasRebindFailure(result.Warnings),
		"anti-vacuous: the rebind must have actually failed and warned")
	require.Equal(t, oldBound, server.manager.ListenerAddress("network.preview_listen_addr"),
		"the failed rebind must leave the old network-bound preview serving")

	// The notice must NOT resurface: the transition gate (!wasPreviewExposed) is false.
	require.Empty(t, previewNotice(result.Warnings),
		"the preview notice must not resurface when the preview is already network-bound. Warnings:\n%s",
		strings.Join(result.Warnings, "\n"))
}

// TestApplyConfigPreviewExposureNoticeDoesNotFireOnUnrelatedSaveWhileLoopback is the
// no-false-positive guard: an unrelated save while the preview is loopback (not exposed)
// must not fire the preview notice. The transition gate (wasPreviewExposed=false,
// servingPreviewExposed=false) suppresses it, and the report's note that the preview
// notice must not surface on every unrelated `config set` is what this pins.
func TestApplyConfigPreviewExposureNoticeDoesNotFireOnUnrelatedSaveWhileLoopback(t *testing.T) {
	server, _ := previewExposureFixture(t, "127.0.0.1:0", false)

	setGlobalConfigValue(t, "default_program", "codex")
	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.Applied, "default_program")
	require.Empty(t, previewNotice(result.Warnings),
		"an unrelated save while the preview is loopback must not surface the preview notice. Warnings:\n%s",
		strings.Join(result.Warnings, "\n"))
}

// TestApplyConfigPreviewExposureNoticeDoesNotFireOnDisable pins the transition-out case:
// moving a network-bound preview back to loopback or "" (disabled) is a transition OUT
// of exposure, so no notice fires — the emitter gates on the INTO-exposure transition,
// matching the control-plane discipline.
func TestApplyConfigPreviewExposureNoticeDoesNotFireOnDisable(t *testing.T) {
	server, _ := previewExposureFixture(t, "0.0.0.0:0", false)

	// Disable the preview entirely (network.preview_listen_addr = "").
	setGlobalConfigValue(t, "network.preview_listen_addr", "")
	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, previewNotice(result.Warnings),
		"disabling a network-bound preview is a transition OUT of exposure — no notice. Warnings:\n%s",
		strings.Join(result.Warnings, "\n"))
	require.Empty(t, server.manager.ListenerAddress("network.preview_listen_addr"),
		"anti-vacuous: the preview listener must be torn down")
}

// TestApplyConfigPreviewNoticePerKeySaveSurfaceCarriesNotice is the END-TO-END save
// surface test: `af config set network.preview_listen_addr <network>` (the per-key
// SetConfigValue flow every first-class surface routes through) must carry the preview
// notice in resp.Warnings — the field the CLI/TUI/web render — on the transition into a
// network-bound preview. Both warning channels are exercised: the per-write
// exposureWarning populates resp.Result.Warnings (channel 1), and the apply-time emitter
// populates resp.Warnings (channel 2). For a transition, channel 2 carries the notice
// in the rendered response field; channel 1 carries it in the per-write fallback field.
func TestApplyConfigPreviewNoticePerKeySaveSurfaceCarriesNotice(t *testing.T) {
	server, _ := previewExposureFixture(t, "127.0.0.1:0", false)

	var resp SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.preview_listen_addr", Value: "0.0.0.0:0"}, &resp))
	require.Equal(t, "network.preview_listen_addr", resp.Result.Key)

	servingPreview := server.manager.ListenerAddress("network.preview_listen_addr")
	require.NotEmpty(t, servingPreview)
	require.False(t, config.IsLoopbackListenAddr(servingPreview),
		"anti-vacuous: the preview must now be network-bound")

	// Channel 2 (apply-time): resp.Warnings carries the notice — this is the field the
	// save surfaces render, and the one that was empty pre-fix.
	notice := previewNotice(resp.Warnings)
	require.NotEmpty(t, notice,
		"the per-key save surface (resp.Warnings) must carry the preview notice on the transition into "+
			"exposure. Warnings:\n%s", strings.Join(resp.Warnings, "\n"))
	require.Contains(t, notice, servingPreview,
		"the notice must name the serving bound address, got: %s", notice)
	require.NotContains(t, notice, "DeliverPrompt",
		"the save surface must carry the PREVIEW notice, not the control-plane one")

	// Channel 1 (per-write): resp.Result.Warnings carries the notice from exposureWarning,
	// the fallback the TUI selects when the apply field is empty. Pre-fix this was empty
	// too (exposureWarning's key gate dropped network.preview_listen_addr).
	require.NotEmpty(t, previewNotice(resp.Result.Warnings),
		"the per-write channel (resp.Result.Warnings / exposureWarning) must also carry the preview "+
			"notice on a network-bound preview write. Result.Warnings:\n%s",
		strings.Join(resp.Result.Warnings, "\n"))
}

// TestApplyConfigPreviewPerKeyAlreadyExposedMoveCarriesPerWriteNotice is the channel-1
// parity test inside the daemon save flow: an ALREADY network-bound preview moved
// between two non-loopback addresses gets NO apply-time notice (the transition gate
// suppresses channel 2 — the move is not an INTO-exposure transition), but the per-write
// exposureWarning still warns (channel 1 is not transition-gated; it fires on every
// exposed network.preview_listen_addr write, matching the control-plane semantic).
// The rebind is forced to fail here so the test is deterministic and free of port races;
// channel 1 fires from the resulting on-disk config regardless of the rebind outcome.
//
// The control plane has this parity: exposureWarning warns on every exposed
// network.listen_addr/require_token write, and the apply-time emitter gates the
// transition. Pre-fix the preview key had neither for this residual case.
func TestApplyConfigPreviewPerKeyAlreadyExposedMoveCarriesPerWriteNotice(t *testing.T) {
	server, oldBound := previewExposureFixture(t, "0.0.0.0:0", true)
	require.False(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the start preview must already be network-bound (exposed)")

	// Move to a DIFFERENT non-loopback address; the rebind FAILS (forced), so the old
	// network-bound listener keeps serving. Channel 1 fires from the resulting config.
	var resp SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.preview_listen_addr", Value: "0.0.0.0:8444"}, &resp))
	require.Equal(t, "network.preview_listen_addr", resp.Result.Key)
	require.NotContains(t, resp.Applied, "network.preview_listen_addr",
		"anti-vacuous: a key whose rebind failed must not be reported as applied")
	require.True(t, hasRebindFailure(resp.Warnings),
		"anti-vacuous: the rebind failure must warn")
	require.Equal(t, oldBound, resp.ListenerAddr,
		"the failed rebind must report the still-serving bound address")

	// Channel 2 (apply-time): NO preview notice — the transition gate (wasPreviewExposed
	// is true) suppresses it; this is the "already exposed, just moving" residual.
	require.Empty(t, previewNotice(resp.Warnings),
		"the apply-time channel must not resurface on an already-exposed move (transition gate). Warnings:\n%s",
		strings.Join(resp.Warnings, "\n"))

	// Channel 1 (per-write): the preview notice MUST ride through resp.Result.Warnings —
	// the per-write emitter fires on every exposed network.preview_listen_addr write, so
	// the operator is still warned. Pre-fix this was empty (exposureWarning's key gate
	// excluded the preview key), so the already-exposed move warned for the control plane
	// but not the preview.
	require.NotEmpty(t, previewNotice(resp.Result.Warnings),
		"the per-write channel (resp.Result.Warnings) must carry the preview notice even when the "+
			"apply-time transition gate suppresses it. Result.Warnings:\n%s",
		strings.Join(resp.Result.Warnings, "\n"))
}
