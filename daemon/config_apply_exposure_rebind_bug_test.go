package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The apply-time exposure-notice emitter in ApplyConfig must describe the SERVING
// posture (the address the daemon is actually accepting on, with the live-applied
// require_token), not the REQUESTED posture. The two diverge exactly when a
// network.listen_addr rebind FAILS: config has already moved on to the requested
// address while the daemon keeps serving on the previous bound socket, and
// require_token applies live through a separate channel (per-request, no rebind).
//
// Keying the notice on the requested posture there had two manifestations:
//
//   - Case 1 (false positive): a loopback listener (not exposed) moves to a
//     network-exposed tokenless address whose rebind FAILS; the old loopback
//     listener keeps serving (still not exposed), yet the notice fired anyway —
//     contradicting the sibling rebind-failure warning and the empty Applied in
//     the same response.
//
//   - Case 2 (wrong address on a genuine exposure): a token-gated NETWORK listener
//     (non-loopback + require_token=true — not exposed) moves to a tokenless
//     network address whose address rebind FAILS; require_token=false applies
//     LIVE, so the old non-loopback listener becomes genuinely exposed at the
//     very moment the rebind "fails". The notice fired (correctly) but named the
//     REQUESTED address that never bound, not the dialable bound address that IS
//     exposed.
//
// The fix keys the notice on the serving bound address (m.ListenerAddress) and
// the live require_token. These tests pin both cases plus the regressions the fix
// must NOT introduce: a successful rebind still emits, a live require_token flip
// with no rebind still emits, and an unrelated save while already exposed still
// does not resurface the notice.

// setGlobalConfigValue writes a single global config key to the sandboxed home,
// failing the test on error. config.SetGlobalConfigValue returns two values, so a
// helper keeps the call sites single-statement and readable.
func setGlobalConfigValue(t *testing.T, key, value string) {
	t.Helper()
	_, err := config.SetGlobalConfigValue(key, value)
	require.NoError(t, err)
}

// exposureNotice returns the oneWarnings entry that carries the tokenless-network
// security-advisory text, or "" if none is present. It isolates the notice from
// the rebind-failure warning, which fires beside it and legitimately names the
// requested address that failed to bind.
func exposureNotice(warnings []string) string {
	for _, w := range warnings {
		if strings.Contains(w, "reachable from the network") {
			return w
		}
	}
	return ""
}

// hasRebindFailure reports whether warnings carry a failed-rebind warning (the
// bind-new-before-close fallback message), confirming the rebind really was
// attempted and failed — an anti-vacuous guard so a suppression assertion cannot
// pass merely because no rebind was tried.
func hasRebindFailure(warnings []string) bool {
	return strings.Contains(strings.Join(warnings, "\n"), "still serving")
}

// exposureRebindFixture builds a controlServer whose manager owns a REAL bound web
// listener bound from tomlBody (disk and live config match), with the next rebind
// forced to fail when failNextRebind is set. It returns the server and the
// kernel-resolved bound address the listener is actually accepting on.
func exposureRebindFixture(t *testing.T, tomlBody string, failNextRebind bool) (*controlServer, string) {
	t.Helper()
	server := rebindNoticeFixture(t, tomlBody, failNextRebind)
	bound := server.manager.ListenerAddress("network.listen_addr")
	require.NotEmpty(t, bound, "fixture must start from a listener that actually bound")
	return server, bound
}

// --- Case 1: false-positive notice suppressed on a failed rebind from loopback. ---

// TestApplyConfigSuppressesExposureNoticeOnFailedRebindFromLoopback is the case-1
// reproducer: a NOT-exposed loopback listener moves to a network-exposed tokenless
// address whose rebind FAILS. The old loopback listener keeps serving (still not
// exposed), so the tokenless-network exposure notice must NOT fire — it would
// contradict the sibling rebind-failure warning and the empty Applied in the same
// response. Pre-fix this fired; the fix keys on the serving (loopback) address.
func TestApplyConfigSuppressesExposureNoticeOnFailedRebindFromLoopback(t *testing.T) {
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '127.0.0.1:0'\nrequire_token = false\n", true)
	require.True(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the still-serving address must be loopback (not exposed)")

	setGlobalConfigValue(t, "network.listen_addr", "0.0.0.0:8443")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)

	// The rebind failed: the key is deferred (not applied) and warned.
	require.Contains(t, result.FailedListenerKeys, "network.listen_addr")
	require.NotContains(t, result.Applied, "network.listen_addr",
		"a key whose rebind failed must not be reported as applied")
	require.True(t, hasRebindFailure(result.Warnings),
		"anti-vacuous: the rebind must have actually failed and warned")

	// The old loopback listener is still the one serving.
	require.Equal(t, oldBound, server.manager.ListenerAddress("network.listen_addr"),
		"the failed rebind must leave the old loopback listener as the serving (bound) address")

	// The exposure notice must NOT fire: the daemon is serving a loopback address,
	// not exposed, regardless of what the operator asked for.
	require.Empty(t, exposureNotice(result.Warnings),
		"the exposure notice must not fire when the still-serving listener is loopback "+
			"(not exposed) after a failed rebind. Warnings:\n%s",
		strings.Join(result.Warnings, "\n"))
}

// --- Case 2 (batch): notice names the serving bound address, not the requested one. ---

// TestApplyConfigExposureNoticeNamesServingAddressOnFailedRebindFromTokenGatedNetwork
// is the case-2 reproducer (legacy batch ApplyConfig entry point): a token-gated
// NETWORK listener (not exposed) moves to a tokenless network address whose
// rebind FAILS, while require_token=false applies LIVE. The old non-loopback
// listener becomes genuinely exposed, so the notice must fire — but it must name
// the SERVING bound address (the dialable target that is actually exposed), not
// the requested address that never bound. Pre-fix it named the un-bound requested
// address; the fix keys on the serving bound address.
func TestApplyConfigExposureNoticeNamesServingAddressOnFailedRebindFromTokenGatedNetwork(t *testing.T) {
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = true\n", true)
	require.False(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the start address must be non-loopback so a live require_token flip can expose it")

	setGlobalConfigValue(t, "network.listen_addr", "0.0.0.0:9443")
	setGlobalConfigValue(t, "network.require_token", "false")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)

	require.Contains(t, result.FailedListenerKeys, "network.listen_addr")
	require.NotContains(t, result.Applied, "network.listen_addr")
	require.Contains(t, result.Applied, "network.require_token",
		"require_token applies live (no rebind) even when the listen_addr rebind fails")

	servingAddr := server.manager.ListenerAddress("network.listen_addr")
	require.Equal(t, oldBound, servingAddr,
		"the failed rebind must leave the old non-loopback listener as the serving (bound) address")

	notice := exposureNotice(result.Warnings)
	require.NotEmpty(t, notice,
		"the daemon IS exposed (old non-loopback listener + live require_token=false), so the notice must fire")
	require.Contains(t, notice, servingAddr,
		"the notice must name the SERVING bound address that is actually exposed, got: %s", notice)
	require.NotContains(t, notice, "0.0.0.0:9443",
		"the notice must not name the requested address that never bound, got: %s", notice)
}

// --- Case 2 (per-key primary `af config set` flow): the order-dependent surface. ---

// TestApplyConfigExposureNoticePerKeyOrderNamesServingAddress reproduces case 2 on
// the PRIMARY `af config set` save surface, which routes each write through its
// own SetConfigValue + ApplyConfig. In the order `set listen_addr` (rebind fails)
// THEN `set require_token=false`, the notice lands on step B, a non-listener key
// whose reply carries no dialable-address compensator (resp.ListenerAddr is ""), so
// the notice itself is the operator's only in-response source for the exposed
// target — and it must name the SERVING bound address, not the requested one that
// never bound.
func TestApplyConfigExposureNoticePerKeyOrderNamesServingAddress(t *testing.T) {
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = true\n", true)
	require.False(t, config.IsLoopbackListenAddr(oldBound))

	// Step A: set listen_addr to a different network address — rebind FAILS.
	var respA SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.listen_addr", Value: "0.0.0.0:9443"}, &respA))
	require.Contains(t, respA.Result.Key, "network.listen_addr")
	require.True(t, hasRebindFailure(respA.Warnings),
		"anti-vacuous: step A's rebind must have failed and warned")
	require.Empty(t, exposureNotice(respA.Warnings),
		"step A: require_token is still true at the requested address, so no exposure notice fires yet")

	// Step A is a listener-key write, so its reply carries the bound (serving)
	// address as the #3722 per-key dialable-target compensator.
	require.Equal(t, oldBound, respA.ListenerAddr,
		"step A is a listener-key write, so its reply must carry the still-bound old address")

	// Step B: set require_token=false — applies LIVE; the old non-loopback listener
	// becomes genuinely exposed while the (re-attempted) rebind fails again. The
	// notice fires on this step.
	var respB SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.require_token", Value: "false"}, &respB))
	require.Equal(t, "network.require_token", respB.Result.Key)
	require.Equal(t, "", respB.ListenerAddr,
		"step B is a non-listener key, so its reply carries no dialable-address compensator")

	noticeB := exposureNotice(respB.Warnings)
	require.NotEmpty(t, noticeB,
		"step B: the daemon IS exposed (old non-loopback listener + live require_token=false), so the notice must fire")
	require.Contains(t, noticeB, oldBound,
		"step B's notice must name the SERVING bound address that is actually exposed, got: %s", noticeB)
	require.NotContains(t, noticeB, "0.0.0.0:9443",
		"step B's notice must not name the requested address that never bound, got: %s", noticeB)
}

// TestApplyConfigExposureNoticePerKeyReverseOrderSuppressesWhenAlreadyExposed pins
// the order-dependence called out in the report: when require_token=false is set
// FIRST, the listener is already tokenless-and-exposed on the old bound non-loopback
// address before the listen_addr rebind is attempted, so wasExposed=true and the
// apply-time emitter's !wasExposed gate suppresses the notice on the failing
// listen_addr step. Only the rebind-failure warning fires; no exposure notice.
func TestApplyConfigExposureNoticePerKeyReverseOrderSuppressesWhenAlreadyExposed(t *testing.T) {
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = true\n", true)
	require.False(t, config.IsLoopbackListenAddr(oldBound))

	// Step A: set require_token=false — applies LIVE, exposing the old non-loopback
	// listener. The notice fires here (the transition into exposure), naming the
	// serving bound address.
	var respA SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.require_token", Value: "false"}, &respA))
	require.Equal(t, "network.require_token", respA.Result.Key)
	require.NotEmpty(t, exposureNotice(respA.Warnings),
		"step A is the transition into exposure, so the notice must fire")
	require.Contains(t, exposureNotice(respA.Warnings), oldBound,
		"step A's notice must name the serving bound address")

	// Step B: set listen_addr to a different network address — rebind FAILS. The
	// listener is ALREADY exposed, so the notice must NOT resurface; only the
	// rebind-failure warning fires.
	var respB SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.listen_addr", Value: "0.0.0.0:9443"}, &respB))
	require.True(t, hasRebindFailure(respB.Warnings),
		"anti-vacuous: step B's rebind must have failed and warned")
	require.Empty(t, exposureNotice(respB.Warnings),
		"the notice must not resurface on the failing listen_addr step once already exposed. Warnings:\n%s",
		strings.Join(respB.Warnings, "\n"))
}

// --- Regressions the fix must NOT introduce. ---

// TestApplyConfigExposureNoticeNamesServingAddressOnSuccessfulRebind guards against
// an over-suppression that would hide a genuine exposure: a SUCCESSFUL rebind from
// a loopback listener to a network-exposed tokenless address must STILL fire the
// notice, naming the newly bound serving address.
func TestApplyConfigExposureNoticeNamesServingAddressOnSuccessfulRebind(t *testing.T) {
	server, _ := exposureRebindFixture(t,
		"[network]\nlisten_addr = '127.0.0.1:0'\nrequire_token = false\n", false)

	// Move to a tokenless network bind on an ephemeral port. The rebind SUCCEEDS.
	setGlobalConfigValue(t, "network.listen_addr", "0.0.0.0:0")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys, "the network bind must succeed")

	servingAddr := server.manager.ListenerAddress("network.listen_addr")
	require.NotEmpty(t, servingAddr)
	require.False(t, config.IsLoopbackListenAddr(servingAddr),
		"anti-vacuous: the new serving address must be non-loopback (exposed)")

	notice := exposureNotice(result.Warnings)
	require.NotEmpty(t, notice,
		"a successful transition into the exposed posture must surface the notice")
	require.Contains(t, notice, servingAddr,
		"the notice must name the newly bound serving address, got: %s", notice)
}

// TestApplyConfigExposureNoticeFiresOnLiveRequireTokenFlipWithoutRebind pins the
// live-auth-apply channel: a require_token=false flip on a non-loopback bound
// listener, with listen_addr UNCHANGED (no rebind), genuinely exposes the daemon.
// The notice must fire and name the serving bound address. (Pre-fix it named the
// config address, which equals the requested; post-fix it names the kernel-resolved
// bound address — the dialable one.)
func TestApplyConfigExposureNoticeFiresOnLiveRequireTokenFlipWithoutRebind(t *testing.T) {
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = true\n", false)
	require.False(t, config.IsLoopbackListenAddr(oldBound))

	// Flip require_token to false; listen_addr is UNCHANGED, so NO rebind.
	setGlobalConfigValue(t, "network.require_token", "false")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys, "no listen_addr change means no rebind, nothing fails")
	require.NotContains(t, result.Applied, "network.listen_addr")
	require.Contains(t, result.Applied, "network.require_token")

	notice := exposureNotice(result.Warnings)
	require.NotEmpty(t, notice,
		"the live require_token=false flip must expose the daemon and fire the notice")
	require.Contains(t, notice, oldBound,
		"the notice must name the serving bound address, got: %s", notice)
}

// TestApplyConfigExposureNoticeDoesNotResurfaceAfterFailedRebindFromExposed pins the
// scenario identified in the P2 review: a tokenless non-loopback listener (exposed)
// attempts to rebind to a loopback address — the rebind FAILS, so the config advances
// to the (requested) loopback address while the socket remains serving the old
// non-loopback one. On every subsequent unrelated ApplyConfig while that rebind is
// still pending, old (= m.Config()) carries the requested-but-never-bound loopback
// address, so wasExposed computed from old.ListenAddr is false even though the daemon
// is continuously exposed. The fix captures the serving address before reconcile.
func TestApplyConfigExposureNoticeDoesNotResurfaceAfterFailedRebindFromExposed(t *testing.T) {
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n", true)
	require.False(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the start address must be non-loopback (exposed)")

	// Step 1: attempt to rebind to loopback — rebind FAILS. Config advances to
	// loopback, but the socket keeps serving the old non-loopback bound address.
	setGlobalConfigValue(t, "network.listen_addr", "127.0.0.1:0")
	first, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, first.FailedListenerKeys, "network.listen_addr",
		"anti-vacuous: the rebind must have failed")
	require.True(t, hasRebindFailure(first.Warnings),
		"anti-vacuous: the rebind must have warned")
	require.Equal(t, oldBound, server.manager.ListenerAddress("network.listen_addr"),
		"the old non-loopback listener must still be serving after the failed rebind")
	// No exposure notice on this step: the daemon was ALREADY exposed before the
	// rebind was attempted (tokenless non-loopback), so !wasExposed is false.
	require.Empty(t, exposureNotice(first.Warnings),
		"no exposure notice on step 1: the daemon was already exposed before the rebind. Warnings:\n%s",
		strings.Join(first.Warnings, "\n"))

	// Step 2: an unrelated save while the failed rebind is still pending. The
	// live config carries the requested loopback address (which never bound), but
	// the daemon is still serving the old non-loopback socket and IS exposed. The
	// notice must NOT resurface: the transition into exposure happened before step
	// 1, not here. Pre-fix this fired because wasExposed was computed from
	// old.ListenAddr (the requested loopback) rather than the serving address.
	setGlobalConfigValue(t, "default_program", "codex")
	second, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, second.Applied, "default_program")
	require.Empty(t, exposureNotice(second.Warnings),
		"the notice must not resurface on an unrelated save while the daemon is "+
			"continuously exposed (failed rebind from an already-exposed socket). Warnings:\n%s",
		strings.Join(second.Warnings, "\n"))
}

// TestApplyConfigUnrelatedChangeWhileExposedDoesNotResurfaceWithRealListener is the
// stronger sibling of TestApplyConfig_UnrelatedChangeWhileExposedDoesNotResurfaceExposureNotice:
// it exercises the !wasExposed warning-fatigue gate against a REAL bound web listener
// (the path that can fail a rebind), so the serving-posture logic and the gate are
// exercised together. An unrelated save while already exposed must not resurface the
// notice, and the transition into exposure must still surface it once.
func TestApplyConfigUnrelatedChangeWhileExposedDoesNotResurfaceWithRealListener(t *testing.T) {
	server, _ := exposureRebindFixture(t,
		"[network]\nlisten_addr = '127.0.0.1:0'\nrequire_token = true\n", false)

	// (1) Transition INTO exposure: move to a tokenless network bind. Rebind SUCCEEDS.
	setGlobalConfigValue(t, "network.listen_addr", "0.0.0.0:0")
	setGlobalConfigValue(t, "network.require_token", "false")
	first, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, first.FailedListenerKeys)
	require.NotEmpty(t, exposureNotice(first.Warnings),
		"anti-vacuous: the transition into exposure MUST surface the notice")

	// (2) An UNRELATED change while exposed must NOT resurface the notice.
	setGlobalConfigValue(t, "default_program", "codex")
	second, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, second.Applied, "default_program")
	require.Empty(t, exposureNotice(second.Warnings),
		"the notice must not resurface on an unrelated save while already exposed. Warnings:\n%s",
		strings.Join(second.Warnings, "\n"))
}

// TestApplyConfigExposureNoticeFiresWhenAbsentListenerBecomesExposed pins the
// absent-listener case identified in the P2 review: when webListeners exists but
// the bound address is "" (initial bind failed or an unexpected Serve exit cleared
// webBoundAddr), the pre-reconcile serving posture is "no listener" — NOT the
// requested address sitting in old.ListenAddr. If old.ListenAddr carries a
// tokenless non-loopback address and we incorrectly retain it as the serving
// address, wasExposed becomes true, and a subsequent apply that successfully
// restores a tokenless non-loopback listener suppresses the exposure notice
// (the transition from "no listener" to "exposed listener" is silent). The fix
// always uses ListenerAddress() when webListeners != nil, preserving "" here.
func TestApplyConfigExposureNoticeFiresWhenAbsentListenerBecomesExposed(t *testing.T) {
	// Start with a tokenless non-loopback listener so old.ListenAddr is a
	// non-loopback address — the value that pre-fix incorrectly stood in for the
	// serving address when the listener was absent.
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n", false)
	require.False(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the start address must be non-loopback")

	// Simulate an unexpected Serve exit that cleared both webBoundAddr and
	// webConfigAddr: the listener machinery (webListeners) still exists, but
	// no socket is accepting. An unexpected Serve exit (lines 242-243 of
	// listener_reload.go) sets both fields to "". The webHandle is left pointing
	// to the stale (dead) handle, matching the "unexpected death" path.
	server.manager.webListeners.mu.Lock()
	server.manager.webListeners.webBoundAddr = ""
	server.manager.webListeners.webConfigAddr = ""
	server.manager.webListeners.mu.Unlock()
	require.Empty(t, server.manager.ListenerAddress("network.listen_addr"),
		"anti-vacuous: the absent-listener state must be visible through ListenerAddress")

	// Now apply a config that causes the listener to be (re-)established at a
	// tokenless non-loopback address. The rebind succeeds (listenTCP is not forced
	// to fail). webConfigAddr is "" so reconcile sees a diff ("0.0.0.0:0" != "")
	// and triggers bindWebLocked, which starts a new accepting socket.
	setGlobalConfigValue(t, "network.listen_addr", "0.0.0.0:0")

	result, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys,
		"the rebind to the restored listener must succeed")

	newBound := server.manager.ListenerAddress("network.listen_addr")
	require.NotEmpty(t, newBound, "anti-vacuous: the listener must now be serving")
	require.False(t, config.IsLoopbackListenAddr(newBound),
		"anti-vacuous: the restored serving address must be non-loopback (exposed)")

	// The exposure notice MUST fire: this is a transition from "no listener" to an
	// exposed one. Pre-fix it was suppressed because old.ListenAddr (non-loopback
	// tokenless) was used as the serving address, yielding wasExposed=true.
	notice := exposureNotice(result.Warnings)
	require.NotEmpty(t, notice,
		"the notice must fire for the transition from absent listener to an exposed one. Warnings:\n%s",
		strings.Join(result.Warnings, "\n"))
	require.Contains(t, notice, newBound,
		"the notice must name the newly serving bound address, got: %s", notice)
}
