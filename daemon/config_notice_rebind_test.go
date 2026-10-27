package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The save-surface notice must never contradict the warning printed beside it
// (#3397, the #3233–#3237 outcome-truthfulness family). A network.listen_addr /
// network.preview_listen_addr change is applied by REBINDING the socket, and
// bind-new-before-close leaves the OLD listener serving when that bind fails. So
// "the running daemon is using the new value now" is false in exactly the case the
// daemon is loudest about: stdout claimed applied while stderr said the daemon was
// still serving the old address.
//
// There are FOUR save surfaces, and the reason this file covers all four rather
// than only the two that were broken is the shape of the original defect. The
// rebind-failure check used to live at each call site, so `set` (server and client)
// had it and `unset` (server and client) was written without it — nothing failed,
// because nothing forced a surface to answer. The fix moves the decision into
// config.EffectNotice; these tests are what makes "no surface can be missed" a
// checked claim rather than a design intention.
//
// Which tests are RED first, on unmodified master — every one of them an UNSET, the
// half that was missed:
//   - TestServerUnsetReportsDeferredWhenListenerRebindFails
//   - TestClientFallbackUnsetCannotConfirmWhenListenerRebindFails
//   - TestClientFallbackUnsetAliasSpellingCanonicalizesTheResultKey
//
// The last two were renamed by #4247, which changed what the CLIENT FALLBACK may
// claim: that path only runs against a daemon too old to report a config digest,
// so it cannot establish which bytes the apply loaded and answers `unconfirmed`
// rather than `deferred`. The defect above is still caught on all four surfaces —
// none of them may say "using the new value now" — and the two SERVER surfaces
// still carry the deferred sentence, since their digest match proves the apply
// loaded this save. See requireFallbackCannotConfirm.
//
// Every other test in this file is GREEN on master and is a regression guard, not
// evidence of the bug: the two `set` surfaces already compensated correctly and
// must come out of the refactor behaving identically, and the success/no-daemon/
// teardown guards pin that the fix did not make every save report "deferred".

// rebindNoticeFixture builds a controlServer whose manager owns a REAL bound web
// listener, over a config.toml holding tomlBody.
//
// failNextRebind swaps the listener factory AFTER the initial bind succeeds, so the
// fixture starts from a genuinely serving daemon and only the rebind under test
// fails. That is deterministic and, unlike occupying a port, never touches the fixed
// default address (127.0.0.1:8443) that an unset of network.listen_addr falls back
// to — the bind is refused before it is attempted.
func rebindNoticeFixture(t *testing.T, tomlBody string, failNextRebind bool) *controlServer {
	t.Helper()
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	body := "schema_version = " + strconv.Itoa(config.GlobalConfigSchemaVersion) + "\n" + tomlBody
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(body), 0o600))

	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed, "the fixture must start from a listener that actually bound")
	t.Cleanup(func() { _ = wl.close() })

	if failNextRebind {
		wl.listenTCP = func(string, string) (net.Listener, error) {
			return nil, errors.New("address already in use (forced by the test)")
		}
	}
	return &controlServer{manager: m}
}

// requireDeferredNotice asserts the honest three-state answer: the write landed on
// disk, it did NOT take effect live, and the notice says so instead of claiming it
// did. The "must not claim" half is the assertion that matters — a notice that
// merely omits the good news while stdout still reads "Applied" is the bug.
func requireDeferredNotice(t *testing.T, notice, key string) {
	t.Helper()
	require.NotContains(t, notice, "using the new value now",
		"a listener key whose rebind FAILED must never be reported as live — the daemon is still serving the old address")
	require.Contains(t, notice, "could not be applied to the running daemon",
		"the notice must name the failure the warning describes")
	require.Contains(t, notice, key, "the deferred notice must name the key that did not apply")
	require.Contains(t, notice, "next daemon start", "the value is on disk, so it takes effect on the next start")
}

// requireFallbackCannotConfirm is the CLIENT FALLBACK's answer for a live key
// since #4247, and it replaces requireDeferredNotice on that path alone.
//
// The fallback runs only against a daemon too old to serve SetConfigValue
// (pre-#1960), whose ApplyConfigResponse carries no config digest — so nothing
// there can establish WHICH bytes that daemon's apply loaded, and a save on that
// path withholds the live claim instead of making one it cannot support.
//
// That reaches a failed rebind too, and the reason is worth stating because the
// deferred CLASS rows are deliberately left alone by the same rule. A class row
// says WHEN a key takes effect, which is true whichever value ended up in the
// file. The rebind-deferred sentence says what happened to THIS SAVE — it names
// this save's key as the thing that could not be applied — and the fallback
// cannot know the daemon was rebinding this save's address rather than a
// competing writer's. So the class rows stand and this one does not.
//
// The exact sentence is named here rather than pattern-matched: the whole point
// of these four surfaces is that they say the same thing.
func requireFallbackCannotConfirm(t *testing.T, notice string) {
	t.Helper()
	require.Equal(t,
		"Saved — the daemon’s live config apply could not be confirmed (see the warnings for the reason).",
		notice)
}

// rebindFailureControl models a running daemon OLD enough not to serve
// SetConfigValue/UnsetConfigValue — so a client takes its local-write fallback and
// pokes ApplyConfig afterwards — whose apply reports a listener rebind that failed.
// It mirrors what a real daemon returns for that outcome: the key is NOT in Applied
// (withoutKeys drops it, #3030), a warning names the address, and FailedListenerKeys
// carries the machine-readable half.
type rebindFailureControl struct {
	failedListenerKeys []string
}

func (s *rebindFailureControl) ApplyConfig(_ ApplyConfigRequest, resp *ApplyConfigResponse) error {
	resp.FailedListenerKeys = s.failedListenerKeys
	for _, key := range s.failedListenerKeys {
		resp.Warnings = append(resp.Warnings,
			"apply "+key+" \"127.0.0.1:9\": address already in use — daemon still serving on 127.0.0.1:8443")
	}
	if len(s.failedListenerKeys) == 0 {
		resp.Applied = []string{"network.listen_addr"}
	}
	return nil
}

// --- Surface 1: server-side unset. RED on unmodified master (the reported bug). ---

// TestServerUnsetReportsDeferredWhenListenerRebindFails is the reported defect
// (#3397): `af config unset network.listen_addr` against a daemon whose rebind fails
// printed "Applied — the running daemon is using the new value now." on stdout while
// resp.Warnings said the daemon was still serving the old address.
func TestServerUnsetReportsDeferredWhenListenerRebindFails(t *testing.T) {
	bound := grabFreeLoopbackAddr(t)
	server := rebindNoticeFixture(t, "[network]\nlisten_addr = '"+bound+"'\n", true)

	var resp UnsetConfigValueResponse
	require.NoError(t, server.UnsetConfigValue(UnsetConfigValueRequest{Key: "network.listen_addr"}, &resp))
	require.NotNil(t, resp.Result)
	require.True(t, resp.Result.Removed, "the fixture must actually remove the key, or the rebind never happens")

	require.NotEmpty(t, resp.Warnings, "a failed rebind must warn — the notice is what must agree with the warning")
	requireDeferredNotice(t, resp.RestartNotice, "network.listen_addr")
	require.Equal(t, config.ApplyStatusDeferred, resp.ApplyOutcome)
}

// TestServerSetPreviewListenAddrReportsDeferredWhenRebindFails covers the OTHER
// socket key, so a fix that reached only one spelling would be caught. It goes
// through set rather than unset for the reason the next test explains. GREEN on
// master, like the other set surfaces.
func TestServerSetPreviewListenAddrReportsDeferredWhenRebindFails(t *testing.T) {
	bound := grabFreeLoopbackAddr(t)
	preview := grabFreeLoopbackAddr(t)
	target := grabFreeLoopbackAddr(t)
	server := rebindNoticeFixture(t,
		"[network]\nlisten_addr = '"+bound+"'\npreview_listen_addr = '"+preview+"'\n", true)

	var resp SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.preview_listen_addr", Value: target}, &resp))
	requireDeferredNotice(t, resp.RestartNotice, "network.preview_listen_addr")
	require.Equal(t, config.ApplyStatusDeferred, resp.ApplyOutcome)
}

// TestServerUnsetPreviewListenAddrReportsAppliedBecauseTeardownCannotFail is the
// sharpest form of the over-correction guard, and the reason the test above uses set.
//
// network.preview_listen_addr defaults to "" (disabled), so unsetting it does not
// REBIND the preview listener — it TEARS IT DOWN, and bindPreviewLocked("") returns
// nil unconditionally. The honest answer is therefore that the change did take
// effect, and it stays that way here even though this fixture has forced every bind
// to fail: a teardown never reaches the listener factory. A fix that keyed the
// deferred notice off "this is a socket key and something is failing" rather than
// off FailedListenerKeys would report deferred here and be wrong.
func TestServerUnsetPreviewListenAddrReportsAppliedBecauseTeardownCannotFail(t *testing.T) {
	bound := grabFreeLoopbackAddr(t)
	preview := grabFreeLoopbackAddr(t)
	server := rebindNoticeFixture(t,
		"[network]\nlisten_addr = '"+bound+"'\npreview_listen_addr = '"+preview+"'\n", true)

	var resp UnsetConfigValueResponse
	require.NoError(t, server.UnsetConfigValue(UnsetConfigValueRequest{Key: "network.preview_listen_addr"}, &resp))
	require.True(t, resp.Result.Removed)
	require.Empty(t, resp.Warnings, "tearing the preview listener down cannot fail, so nothing to warn about")
	require.Contains(t, resp.RestartNotice, "using the new value now",
		"disabling the preview listener SUCCEEDS — a teardown is not a bind, so this must not report deferred")
}

// --- Surface 2: client fallback unset. RED on unmodified master. ---

// TestClientFallbackUnsetCannotConfirmWhenListenerRebindFails covers the daemonless
// half of the same defect: a daemon too old to serve UnsetConfigValue sends the client
// down its local-write path, which pokes ApplyConfig and then worded the notice from
// the poke's error alone — so a rebind failure reported in FailedListenerKeys was
// dropped and the change claimed live.
//
// Since #4247 this surface answers `unconfirmed` rather than `deferred`: that old
// daemon reports no config digest, so the fallback cannot tell which bytes its
// apply loaded and withholds the claim instead of naming this save's key as the
// one that failed to rebind. The defect this test was written for is still
// caught — the notice must never be the live one — and the rebind reason still
// reaches the user, in the warnings the notice points at. The server surfaces
// above keep the deferred sentence, because their digest match proves the apply
// loaded this save.
func TestClientFallbackUnsetCannotConfirmWhenListenerRebindFails(t *testing.T) {
	home := configClientHome(t)
	seedNetworkConfig(t, home)
	serveControlStub(t, &rebindFailureControl{failedListenerKeys: []string{"network.listen_addr"}})

	resp, err := UnsetGlobalConfigValue("network.listen_addr")
	require.NoError(t, err)
	require.NotNil(t, resp.Result)
	require.True(t, resp.Result.Removed)

	require.NotEmpty(t, resp.Warnings, "the client must surface the daemon's rebind warning")
	require.NotContains(t, resp.RestartNotice, "using the new value now",
		"a listener key whose rebind FAILED must never be reported as live, on any surface")
	requireFallbackCannotConfirm(t, resp.RestartNotice)
	require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
}

// --- Surface 3: server-side set. GREEN on master — regression guard. ---

// TestServerSetReportsDeferredWhenListenerRebindFails was already correct before
// #3397: SetConfigValue consulted FailedListenerKeys at the call site. It is here to
// prove the refactor that moved that decision into config.EffectNotice did not change
// this surface's behaviour. Its passing is NOT evidence of the reported bug.
func TestServerSetReportsDeferredWhenListenerRebindFails(t *testing.T) {
	bound := grabFreeLoopbackAddr(t)
	target := grabFreeLoopbackAddr(t)
	server := rebindNoticeFixture(t, "[network]\nlisten_addr = '"+bound+"'\n", true)

	var resp SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.listen_addr", Value: target}, &resp))
	require.NotNil(t, resp.Result)

	require.NotEmpty(t, resp.Warnings, "a failed rebind must warn")
	requireDeferredNotice(t, resp.RestartNotice, "network.listen_addr")
	require.Equal(t, config.ApplyStatusDeferred, resp.ApplyOutcome)
}

// --- Surface 4: client fallback set. GREEN on master — regression guard. ---

// TestClientFallbackSetCannotConfirmWhenListenerRebindFails is the second surface
// that already compensated correctly. Same role as surface 3: it pins that the two
// working sites came out of the refactor identical — and, since #4247, that the
// two CLIENT surfaces answer identically too.
//
// It reports `unconfirmed` for the same reason as its unset twin: this path only
// runs against a daemon too old to report a digest, so which bytes its apply
// loaded is unknowable, and the rebind-deferred sentence would claim that THIS
// save's address is the one that failed to bind.
func TestClientFallbackSetCannotConfirmWhenListenerRebindFails(t *testing.T) {
	home := configClientHome(t)
	seedNetworkConfig(t, home)
	serveControlStub(t, &rebindFailureControl{failedListenerKeys: []string{"network.listen_addr"}})

	resp, err := SetGlobalConfigValue("network.listen_addr", "127.0.0.1:9")
	require.NoError(t, err)
	require.NotNil(t, resp.Result)
	require.NotContains(t, resp.RestartNotice, "using the new value now",
		"a listener key whose rebind FAILED must never be reported as live, on any surface")
	requireFallbackCannotConfirm(t, resp.RestartNotice)
	require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
}

// --- The alias-spelling case. GREEN on master — it pins an invariant, not a fix. ---

// TestClientFallbackSetAliasSpellingCanonicalizesTheResultKey settles the latent
// question this PR was asked to answer rather than assume: the rebind-failure check
// compares result.Key against FailedListenerKeys, and `af config set` accepts BOTH
// spellings of a migrated key while unset removes both.
//
// The two sides cannot in fact disagree today, and this pins the reason. Every entry
// in FailedListenerKeys is a hardcoded canonical literal (webListeners.reconcile), and
// config.SetGlobalConfigValue / config.UnsetGlobalConfigValue both canonicalize before
// building the result — so a set spelled `listen_addr` arrives at the notice as
// "network.listen_addr" and matches. That is an invariant spread across three files
// with nothing previously holding it in place; a future result.Key that kept the
// user's spelling would silently reopen the exact bug.
//
// Since #4247 this surface's NOTICE no longer witnesses that match: the fallback
// cannot confirm which bytes the old daemon loaded, so it answers `unconfirmed`
// without consulting FailedListenerKeys at all. The name says what is left, and
// what is left is the load-bearing half — the end-to-end proof that the writer
// canonicalizes result.Key. The notice side of the same invariant is pinned
// directly, from both directions, by
// config.TestEffectNoticeMatchesFailedListenerKeysAcrossAliasSpellings.
func TestClientFallbackSetAliasSpellingCanonicalizesTheResultKey(t *testing.T) {
	home := configClientHome(t)
	seedNetworkConfig(t, home)
	serveControlStub(t, &rebindFailureControl{failedListenerKeys: []string{"network.listen_addr"}})

	resp, err := SetGlobalConfigValue("listen_addr", "127.0.0.1:9")
	require.NoError(t, err)
	require.Equal(t, "network.listen_addr", resp.Result.Key,
		"the result must carry the canonical spelling, which is what makes the raw comparison safe")
	requireFallbackCannotConfirm(t, resp.RestartNotice)
	require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
}

// TestClientFallbackUnsetAliasSpellingCanonicalizesTheResultKey is the unset half of
// the same question — the surface that removes BOTH alias spellings. RED on master,
// since it runs through the unset path that dropped FailedListenerKeys entirely; the
// alias spelling is an additional axis on top of that, not the cause of the failure.
//
// Renamed and re-aimed with its set twin above, for the same reason: on the
// fallback the notice is `unconfirmed` whatever the key spelling, so result.Key is
// what this test still witnesses.
func TestClientFallbackUnsetAliasSpellingCanonicalizesTheResultKey(t *testing.T) {
	home := configClientHome(t)
	seedNetworkConfig(t, home)
	serveControlStub(t, &rebindFailureControl{failedListenerKeys: []string{"network.listen_addr"}})

	resp, err := UnsetGlobalConfigValue("listen_addr")
	require.NoError(t, err)
	require.Equal(t, "network.listen_addr", resp.Result.Key)
	requireFallbackCannotConfirm(t, resp.RestartNotice)
	require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
}

// --- The other direction: a SUCCESSFUL apply must still say "Applied". ---

// TestServerSetReportsAppliedWhenListenerRebindSucceeds is the guard against
// over-correcting: a real rebind onto a free port succeeds, so the honest answer is
// still that the daemon is using the new value now.
func TestServerSetReportsAppliedWhenListenerRebindSucceeds(t *testing.T) {
	bound := grabFreeLoopbackAddr(t)
	target := grabFreeLoopbackAddr(t)
	server := rebindNoticeFixture(t, "[network]\nlisten_addr = '"+bound+"'\n", false)

	var resp SetConfigValueResponse
	require.NoError(t, server.SetConfigValue(
		SetConfigValueRequest{Key: "network.listen_addr", Value: target}, &resp))
	require.Contains(t, resp.RestartNotice, "using the new value now",
		"a rebind that SUCCEEDED must still report the change as live")
	require.Equal(t, config.ApplyStatusApplied, resp.ApplyOutcome)
}

// TestServerUnsetReportsAppliedWhenNothingRebinds unsets network.require_token, an
// applied-live key that is read per request and so never rebinds a socket. The
// listener address is untouched, the apply succeeds outright, and the notice must
// still be the live one.
//
// It is deliberately not network.listen_addr: unsetting THAT falls back to the fixed
// default 127.0.0.1:8443, and a test has no business binding a fixed port on a shared
// runner. The successful-rebind half of the contract is covered above, on set, where
// the destination can be a port the test owns.
func TestServerUnsetReportsAppliedWhenNothingRebinds(t *testing.T) {
	bound := grabFreeLoopbackAddr(t)
	server := rebindNoticeFixture(t,
		"[network]\nlisten_addr = '"+bound+"'\nrequire_token = true\n", false)

	var resp UnsetConfigValueResponse
	require.NoError(t, server.UnsetConfigValue(UnsetConfigValueRequest{Key: "network.require_token"}, &resp))
	require.True(t, resp.Result.Removed)
	require.Contains(t, resp.RestartNotice, "using the new value now",
		"an applied-live key the daemon applied with no rebind involved is live now")
}

// TestClientFallbackCannotConfirmEvenWhenNoListenerFailed was the over-correction
// guard on the client surfaces — "an apply reporting no failed listener keys still
// says applied" — and #4247 inverted its answer, so it is renamed to what it now
// asserts rather than left describing behaviour it no longer pins.
//
// The new claim is the sharper one, and it is why the rename is not a demotion:
// the fallback's uncertainty comes from the MISSING DIGEST, not from the rebind.
// A clean apply — no failed listener keys, the daemon reporting the key applied —
// is still `unconfirmed` here, because a pre-#1960 daemon cannot say which bytes
// it loaded and a competing writer may have won between the local write and the
// poke. So this and the two rebind-failure tests above agree, on purpose, and a
// change that made the fallback's answer depend on FailedListenerKeys again would
// split them.
//
// The over-correction guard itself is not lost. It lives where the digest still
// discriminates: TestServerSetReportsAppliedWhenListenerRebindSucceeds and
// TestServerUnsetReportsAppliedWhenNothingRebinds above still require the live
// sentence, and TestClientFallbackAppliesTheDigestSkewRule requires a DEFERRED
// key to keep its class sentence on this very path — so "every save now reports
// unconfirmed" would fail three tests.
func TestClientFallbackCannotConfirmEvenWhenNoListenerFailed(t *testing.T) {
	home := configClientHome(t)
	seedNetworkConfig(t, home)
	serveControlStub(t, &rebindFailureControl{})

	setResp, err := SetGlobalConfigValue("network.listen_addr", "127.0.0.1:9")
	require.NoError(t, err)
	require.Equal(t, []string{"network.listen_addr"}, setResp.Applied,
		"premise: the daemon itself reported the key APPLIED, so nothing but the missing digest withholds the claim")
	requireFallbackCannotConfirm(t, setResp.RestartNotice)
	require.Equal(t, config.ApplyStatusUnconfirmed, setResp.ApplyOutcome)

	unsetResp, err := UnsetGlobalConfigValue("network.listen_addr")
	require.NoError(t, err)
	requireFallbackCannotConfirm(t, unsetResp.RestartNotice)
	require.Equal(t, config.ApplyStatusUnconfirmed, unsetResp.ApplyOutcome)
}

// --- And with nothing answering the socket, the daemonless sentence is unchanged. ---

// TestClientFallbackWithNoDaemonKeepsTheSavedNotice: with no daemon there is nothing
// to apply and nothing to fail, so both client surfaces keep the pre-#3397 sentence
// verbatim. This is the "a caller with no apply result stays expressible" contract.
func TestClientFallbackWithNoDaemonKeepsTheSavedNotice(t *testing.T) {
	home := configClientHome(t)
	seedNetworkConfig(t, home)

	const want = "Saved — no daemon is running to apply it, so it takes effect on the next daemon start."

	setResp, err := SetGlobalConfigValue("network.listen_addr", "127.0.0.1:9")
	require.NoError(t, err)
	require.Equal(t, want, setResp.RestartNotice)

	unsetResp, err := UnsetGlobalConfigValue("network.listen_addr")
	require.NoError(t, err)
	require.Equal(t, want, unsetResp.RestartNotice)
}

// seedNetworkConfig writes a global config.toml carrying an explicit network table,
// so an unset has something to remove and a set has a file to edit.
func seedNetworkConfig(t *testing.T, home string) {
	t.Helper()
	body := "schema_version = " + strconv.Itoa(config.GlobalConfigSchemaVersion) +
		"\n[network]\nlisten_addr = '127.0.0.1:8443'\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(body), 0o600))
}
