package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The VS Code editor's per-session, restart-STABLE origin (#2743). code-server is
// VS Code Web and keeps workbench state in origin-scoped browser IndexedDB
// (vscode-web-state-db-global holds terminal.history.entries.commands, which is NOT
// workspace-scoped), so every session's editor sharing the SPA's origin means one
// session's terminal history is readable in another session's editor.

// TestEditorOrigin_DiffersPerSession is the leak, stated as a property: two sessions
// must not share an editor origin. The browser partitions IndexedDB by origin, so
// distinct origins are exactly what stops the history crossing — nothing else in the
// stack does, and the shared user-data directory (the vector this issue was first
// filed against) is not involved either way.
func TestEditorOrigin_DiffersPerSession(t *testing.T) {
	const secret = "persisted-editor-secret"

	a := editorOriginLabel(secret, "session-a")
	b := editorOriginLabel(secret, "session-b")
	require.NotEqual(t, a, b, "two sessions' editors must not share an origin — that IS the leak")
	require.True(t, isPreviewLabel(a), "an editor label must parse as a preview label, so one gate covers both")
	require.True(t, isPreviewLabel(b))
	require.LessOrEqual(t, len(a), 63, "a DNS label may not exceed 63 characters")
}

// TestEditorOrigin_StableAcrossRestarts is the property that separates a fix from a
// second, quieter data loss. The web-tab derivation rotates with an in-memory secret;
// applying that to an editor would blank its layout, its open editors and its
// terminal history on every daemon restart. The editor's secret is persisted, so the
// origin — and therefore the IndexedDB the browser hands back — is the same name
// tomorrow as today.
func TestEditorOrigin_StableAcrossRestarts(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	first, err := ensureEditorOriginSecret()
	require.NoError(t, err)
	require.NotEmpty(t, first)

	// A second daemon lifetime over the same af home resolves the SAME secret.
	second, err := ensureEditorOriginSecret()
	require.NoError(t, err)
	require.Equal(t, first, second, "the editor-origin secret must persist, or every restart is a wipe")
	require.Equal(t, editorOriginLabel(first, "s1"), editorOriginLabel(second, "s1"),
		"a session's editor origin must survive a daemon restart")

	// Contrast, pinned deliberately: the WEB preview secret is ephemeral by design and
	// keeps that lifetime. Collapsing the two would either weaken the preview posture
	// or destroy editor state.
	cfg := config.DefaultConfig()
	m1, err := NewManager(cfg)
	require.NoError(t, err)
	m2, err := NewManager(cfg)
	require.NoError(t, err)
	require.NotEqual(t, m1.previewSecret, m2.previewSecret,
		"the web-preview secret stays per-daemon — this fix must not quietly persist it")
	require.Equal(t, m1.editorOriginSecret, m2.editorOriginSecret,
		"the editor secret is the one that persists")
}

// TestEditorOriginSecret_OnDiskPosture pins how the persisted secret is stored. It is
// bearer material, so it gets the daemon token's posture: 0600 in the af home. It is
// on disk ON PURPOSE — that is the whole point — so the test asserts the mode rather
// than its absence, which is the opposite of the preview secret's contract.
func TestEditorOriginSecret_OnDiskPosture(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	secret, err := ensureEditorOriginSecret()
	require.NoError(t, err)
	path, err := editorOriginSecretPath()
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"the editor-origin secret is bearer material: same posture as the daemon token")
	require.Equal(t, filepath.Dir(path), filepath.Clean(home), "it lives in the af home")

	// An empty/truncated file must be re-minted, never used: a shared empty secret
	// would give every session's editor the SAME origin — the leak, restored.
	require.NoError(t, os.WriteFile(path, []byte("\n"), 0o600))
	remade, err := ensureEditorOriginSecret()
	require.NoError(t, err)
	require.NotEmpty(t, remade, "an empty secret file must be re-minted, never used as the key")
	require.NotEqual(t, secret, remade, "and re-minting must produce a new value, not resurrect the old one")
}

// TestEditorOrigin_MintedPerSessionNotPerTab pins that two vscode tabs in ONE session
// share one origin. There is one code-server per session, so a per-tab origin would
// put a single editor behind two IndexedDBs — the same process disagreeing with
// itself about its own state.
func TestEditorOrigin_MintedPerSessionNotPerTab(t *testing.T) {
	const secret = "persisted-editor-secret"
	require.Equal(t, editorOriginLabel(secret, "s1"), editorOriginLabel(secret, "s1"),
		"the derivation reads the session alone — no tab id enters it")

	// And it is disjoint from the web-tab derivation even under an identical secret,
	// so no editor origin can ever collide with a preview origin.
	require.NotEqual(t, editorOriginLabel(secret, "s1"), previewTabHostLabel(secret, "s1", "t1"))
	require.NotEqual(t, editorOriginLabel(secret, "s1"), previewTabHostLabel(secret, "s1", ""))
}

// TestEditorOrigin_MintedForAVSCodeTab drives the real path: a live VS Code tab must
// mint the PER-SESSION, persisted-secret origin — not the per-tab ephemeral one a web
// tab gets — and the two kinds in one session must land on different origins.
//
// This is what actually closes the leak: the editor stops sharing the SPA's origin,
// so the browser stops handing every session's editor the same IndexedDB.
func TestEditorOrigin_MintedForAVSCodeTab(t *testing.T) {
	upstream := echoUpstream(t, "devserver")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	editorTab := addVSCodeTab(t, m, sessionID)

	webOrigin := previewOriginFor(m, sessionID, tabIDs[0])
	editorOrigin := previewOriginFor(m, sessionID, editorTab)
	require.NotEmpty(t, webOrigin)
	require.NotEmpty(t, editorOrigin)
	require.NotEqual(t, webOrigin, editorOrigin,
		"an editor and a dev-server preview in one session are different origins")

	editorLabel, ok := previewHostLabel(editorOrigin[len("http://"):])
	require.True(t, ok)
	require.Equal(t, editorOriginLabel(m.editorOriginSecret, sessionID), editorLabel,
		"a vscode tab must use the PER-SESSION derivation under the persisted secret")
	require.NotEqual(t, previewTabHostLabel(m.previewSecret, sessionID, editorTab), editorLabel,
		"it must NOT use the per-tab ephemeral derivation — that origin dies on restart")

	// A SECOND vscode tab in the same session resolves to the SAME origin: one
	// code-server per session, so one origin and one IndexedDB.
	secondEditor := addVSCodeTab(t, m, sessionID)
	require.Equal(t, editorOrigin, previewOriginFor(m, sessionID, secondEditor),
		"two vscode tabs share one editor, so they must share one origin")

	// Fail-safe: with no persisted secret the daemon promises no stable editor origin
	// and the client keeps today's same-origin mirror. A WEB tab is unaffected.
	m.editorOriginSecret = ""
	require.Empty(t, previewOriginFor(m, sessionID, editorTab),
		"without a persisted secret there is no stable address to promise")
	require.NotEmpty(t, previewOriginFor(m, sessionID, tabIDs[0]),
		"a web tab rides the ephemeral secret and is unaffected")
	_ = previewAddr
}

// addVSCodeTab adds a VS Code tab to the fixture's session and returns its stable id.
// Creating one spawns nothing — a vscode tab resolves its editor at proxy time.
func addVSCodeTab(t *testing.T, m *Manager, sessionID string) string {
	t.Helper()
	inst, repoID, title, err := m.resolveStreamSession(sessionID, "")
	require.NoError(t, err)
	require.NotNil(t, inst)
	before := len(inst.GetTabs())
	_, err = m.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode"})
	require.NoError(t, err)
	tabs := inst.GetTabs()
	require.Len(t, tabs, before+1)
	id := tabs[before].ID
	require.NotEmpty(t, id)
	return id
}

// TestEditorOrigin_GateAuthenticatesTheEditorOrigin is the round trip the rest of
// this file did not make, and its absence hid a total functional failure.
//
// Minting and deriving were both tested in isolation and both correct, while the
// GATE re-derived every registered label with the web-tab rule — so an editor origin
// presented a label that could never match, and the listener answered the framed
// "address expired" notice instead of the editor. Every unit test still passed: the
// two halves were each right about themselves and wrong about each other.
//
// A vscode target cannot actually be served here (there is no code-server on a test
// box), and that is what makes the assertion sharp: reaching the editor's OWN
// failure — the install hint — proves the request got PAST the gate, which is the
// only thing under test. An "address expired" body would mean it did not.
func TestEditorOrigin_GateAuthenticatesTheEditorOrigin(t *testing.T) {
	upstream := echoUpstream(t, "devserver")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	editorTab := addVSCodeTab(t, m, sessionID)

	editorOrigin := previewOriginFor(m, sessionID, editorTab)
	require.NotEmpty(t, editorOrigin, "a live vscode tab must mint an origin")
	host := editorOrigin[len("http://"):]

	resp := previewHostGet(t, previewHTTPClient(), previewAddr, host, "/")
	body := readAllString(t, resp)
	require.NotContains(t, body, "Preview address expired",
		"the gate must AUTHENTICATE an editor origin — it has to re-derive the label the same way previewOriginFor minted it")
	require.NotContains(t, body, "unauthorized")

	// Past the gate, it fails on the editor itself, which is the expected outcome on a
	// box with no code-server. Either notice proves authentication succeeded.
	require.True(t,
		strings.Contains(body, "VS Code") || strings.Contains(body, "code-server"),
		"expected the editor's own notice once authenticated, got: %s", body)

	// And a WEB tab in the same session still authenticates under its own derivation,
	// so the fork did not simply move the breakage to the other kind.
	webHost := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	webResp := previewHostGet(t, previewHTTPClient(), previewAddr, webHost, "/")
	require.Equal(t, http.StatusOK, webResp.StatusCode)
	require.Contains(t, readAllString(t, webResp), "server=devserver")
}

// TestEditorOrigin_SurvivesTheRegisteringTabClosing pins that a session's editor
// origin does not depend on WHICH vscode tab last asked for it. All of a session's
// editor tabs share one label, so the registry entry holds the last registrant; if
// that tab closes while another editor pane stays open, resolving through the stored
// id would fail on a live editor.
func TestEditorOrigin_SurvivesTheRegisteringTabClosing(t *testing.T) {
	upstream := echoUpstream(t, "devserver")
	m, previewAddr, sessionID, _ := newPreviewOriginFixture(t, upstream.URL)
	first := addVSCodeTab(t, m, sessionID)
	second := addVSCodeTab(t, m, sessionID)

	// The SECOND tab registers last, so it owns the shared entry.
	origin := previewOriginFor(m, sessionID, second)
	require.NotEmpty(t, origin)
	require.Equal(t, origin, previewOriginFor(m, sessionID, first), "one label for the session")

	_, repoID, title, err := m.resolveStreamSession(sessionID, "")
	require.NoError(t, err)
	inst, _, _, err2 := m.resolveStreamSession(sessionID, "")
	require.NoError(t, err2)
	nameOf := ""
	for _, tb := range inst.GetTabs() {
		if tb.ID == second {
			nameOf = tb.Name
		}
	}
	require.NotEmpty(t, nameOf)
	_, err = m.CloseTab(CloseTabRequest{Title: title, RepoID: repoID, TabName: nameOf})
	require.NoError(t, err)

	// The first editor tab is still open, so the shared origin must still resolve.
	resp := previewHostGet(t, previewHTTPClient(), previewAddr, origin[len("http://"):], "/")
	body := readAllString(t, resp)
	require.NotContains(t, body, "Preview address expired",
		"a surviving editor tab must keep the session's origin working after the registrant closed")
}

// TestEditorOrigin_StabilityFollowsTheActiveListener pins that the ephemeral-port
// guard reads the listener that is SERVING, not the one config asks for.
//
// The two diverge exactly where it hurts: a daemon started on "127.0.0.1:0" whose
// live change to a fixed port FAILS to bind has already had the fixed value swapped
// into config, while reconcile deliberately leaves the old random-port listener
// accepting. Deciding from config there would vend a supposedly stable editor origin
// on an ephemeral port, and the editor's browser storage would vanish at the next
// restart — the exact loss the persisted secret exists to prevent, reached by
// trusting the wrong source.
// TestEditorOrigin_WithheldOnANetworkBoundListener pins the security half of the
// editor-origin precondition, which a web tab is deliberately not held to.
//
// On the preview listener the host label is the ONLY credential for every peer, and
// *.localhost resolution buys no protection against an attacker: a remote client just
// sends `Host: <label>.localhost` to an exposed ip:port. Behind an editor origin is a
// code-server the daemon spawned with auth disabled — a terminal, so arbitrary code
// execution as the user — and an editor's label comes from a PERSISTED secret, so it
// never rotates and one leak is permanent. A web tab's label dies at the next restart
// and fronts a dev server, which is a far smaller prize.
//
// Withholding costs nothing real: per-tab origins are same-machine only by nature, so
// a network-bound preview listener could never have served a remote viewer an editor.
func TestEditorOrigin_WithheldOnANetworkBoundListener(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8444", ":8444", "192.168.1.10:8444", "[::]:8444"} {
		require.False(t, editorOriginPortIsSafe(addr),
			"a network-bound preview listener must not carry an editor origin: %q", addr)
	}
	for _, addr := range []string{"127.0.0.1:8444", "localhost:8444", "[::1]:8444"} {
		require.True(t, editorOriginPortIsSafe(addr), "a loopback fixed port is the supported shape: %q", addr)
	}
	// Both conditions are required, not either.
	require.False(t, editorOriginPortIsSafe("127.0.0.1:0"), "loopback but ephemeral is still refused")
	require.False(t, editorOriginPortIsSafe(""), "nothing bound is nothing to promise")

	// End to end: a live editor tab on a network-bound listener mints nothing, while
	// the WEB tab beside it is unaffected — its origin is ephemeral by design and it
	// fronts a dev server, not a terminal.
	upstream := echoUpstream(t, "devserver")
	m, sessionID, _ := newPreviewDaemonWithTabs(t, func(cfg *config.Config) {
		cfg.PreviewListenAddr = "0.0.0.0:0"
	}, upstream.URL)
	editorTab := addVSCodeTab(t, m, sessionID)
	require.Empty(t, previewOriginFor(m, sessionID, editorTab),
		"no editor origin on a listener a network peer can reach")
}

func TestEditorOrigin_StabilityFollowsTheActiveListener(t *testing.T) {
	require.True(t, previewPortIsEphemeral("127.0.0.1:0"), ":0 lets the kernel re-pick every bind")
	require.True(t, previewPortIsEphemeral(""), "nothing bound is nothing stable")
	require.True(t, previewPortIsEphemeral("garbage"))
	require.False(t, previewPortIsEphemeral("127.0.0.1:8444"), "a fixed port is the whole precondition")

	upstream := echoUpstream(t, "devserver")
	m, _, sessionID, _ := newPreviewOriginFixture(t, upstream.URL)
	editorTab := addVSCodeTab(t, m, sessionID)

	// The fixture binds a concrete port, so an editor origin is on offer.
	require.NotEmpty(t, previewOriginFor(m, sessionID, editorTab))
	require.False(t, previewPortIsEphemeral(m.activePreviewConfigAddr()))

	// Now ask config for an ephemeral port WITHOUT rebinding, which is the shape a
	// failed live rebind leaves behind. The active listener still has its fixed port,
	// so the origin must still be offered — the guard must not follow the request.
	next := *m.Config()
	next.PreviewListenAddr = "127.0.0.1:0"
	m.live.Store(&next)
	require.Equal(t, "127.0.0.1:0", m.Config().PreviewListenAddr, "config now asks for an ephemeral port")
	require.False(t, previewPortIsEphemeral(m.activePreviewConfigAddr()),
		"the SERVING listener still has its fixed port — the guard reads that, not the request")
	require.NotEmpty(t, previewOriginFor(m, sessionID, editorTab),
		"an editor origin stays valid while the listener behind it is unchanged")
}

// TestEditorOrigin_RevokedWhenTheListenerBecomesUnsafe pins that the loopback
// requirement is enforced where every request passes, not only where the origin was
// minted. preview_listen_addr applies LIVE, so a loopback listener can become
// network-bound under a registration that already exists — and bindPreviewLocked
// swaps the listener without touching the registry. Gating only the mint would leave
// `Host: <old label>.localhost` authorizing an auth-disabled editor, i.e. a terminal,
// on the newly exposed port.
func TestEditorOrigin_RevokedWhenTheListenerBecomesUnsafe(t *testing.T) {
	upstream := echoUpstream(t, "devserver")
	m, previewAddr, sessionID, _ := newPreviewOriginFixture(t, upstream.URL)
	editorTab := addVSCodeTab(t, m, sessionID)

	origin := previewOriginFor(m, sessionID, editorTab)
	require.NotEmpty(t, origin, "loopback + fixed port: an editor origin is minted")
	host := origin[len("http://"):]
	label, ok := previewHostLabel(host)
	require.True(t, ok)
	_, registered := m.previewOrigins.lookup(label)
	require.True(t, registered, "and the registration exists — this is the stale entry under test")

	// The listener goes network-bound live. The registry is deliberately NOT cleared,
	// which is exactly the condition that made minting-time-only gating insufficient.
	next := *m.Config()
	next.PreviewListenAddr = "0.0.0.0:8444"
	m.live.Store(&next)
	require.NoError(t, m.webListeners.bindPreviewLocked("0.0.0.0:0"))

	// Bind-new-before-close means the OLD listener is gone, so the request has to go
	// to the address now serving. Asking the old one would fail at the transport and
	// prove nothing about the gate — which is exactly how this test first failed.
	exposedAddr := m.lifecycle.snapshot().listeners.PreviewBoundAddr
	require.NotEmpty(t, exposedAddr)
	require.NotEqual(t, previewAddr, exposedAddr, "the listener really moved")

	resp := previewHostGet(t, previewHTTPClient(), exposedAddr, host, "/")
	body := readAllString(t, resp)
	require.NotContains(t, body, "server=devserver", "an exposed listener must not serve the editor's origin")
	require.Contains(t, body, "Preview address expired",
		"the gate must re-check listener safety per request, not trust the registration")
}

// TestEditorOrigin_SurvivesADaemonRestartWithoutAReload pins the property the
// persisted secret exists for. The label is stable across restarts while
// previewOrigins is in-memory and empty on every boot, so a UI left open across a
// restart keeps addressing a perfectly valid host the new daemon has not learned.
// Requiring a manual reload there would defeat the whole point of persisting.
func TestEditorOrigin_SurvivesADaemonRestartWithoutAReload(t *testing.T) {
	upstream := echoUpstream(t, "devserver")
	m, previewAddr, sessionID, _ := newPreviewOriginFixture(t, upstream.URL)
	editorTab := addVSCodeTab(t, m, sessionID)

	origin := previewOriginFor(m, sessionID, editorTab)
	require.NotEmpty(t, origin)
	label, ok := previewHostLabel(origin[len("http://"):])
	require.True(t, ok)

	// Simulate the new daemon: same persisted secret and sessions, EMPTY registry —
	// which is precisely what a restart produces.
	m.previewOrigins = newPreviewOriginRegistry()
	_, known := m.previewOrigins.lookup(label)
	require.False(t, known, "the new boot has never seen this label")

	ref, recovered := m.recoverEditorOrigin(label)
	require.True(t, recovered, "a stable editor label must be resolvable without the per-boot registry")
	require.Equal(t, sessionID, ref.sessionID)
	require.Equal(t, session.TabKindVSCode, ref.kind)

	// And it is registered on the way, so the derivation is paid once per boot.
	_, nowKnown := m.previewOrigins.lookup(label)
	require.True(t, nowKnown)

	// A label belonging to no session is still refused — recovery is derivation, not
	// a bypass of the credential.
	_, bogus := m.recoverEditorOrigin(editorOriginLabel(m.editorOriginSecret, "no-such-session"))
	require.False(t, bogus)
	_ = previewAddr
}

// TestEditorOrigin_UnknownHostIsO1 pins that an unknown preview host costs a map
// lookup, not a scan. Any peer that can dial the preview listener may invent
// correctly-shaped af….localhost hosts with no credential, so the miss path must not
// do per-session work — the version this replaces computed an HMAC per live session
// WHILE HOLDING the manager's main mutex, which on a box with hundreds of sessions
// turns a free rejection into O(sessions) work and serializes real session
// operations behind the flood.
func TestEditorOrigin_UnknownHostIsO1(t *testing.T) {
	secret := "persisted"
	ids := []string{"s1", "s2", "s3"}
	scans := 0
	idFn := func() []string {
		scans++
		return ids
	}
	idx := newEditorLabelIndex()

	// First miss builds the index once.
	_, ok := idx.lookup("afdoesnotexist", idFn, secret)
	require.False(t, ok)
	require.Equal(t, 1, scans)

	// A flood of further unknown hosts must not rebuild: the rate limit holds them to
	// the cached answer.
	for i := 0; i < 50; i++ {
		_, ok := idx.lookup("afinvented", idFn, secret)
		require.False(t, ok)
	}
	require.Equal(t, 1, scans, "an unknown-host flood must not scan per request")

	// A known label still resolves, from the index the first miss built.
	sid, ok := idx.lookup(editorOriginLabel(secret, "s2"), idFn, secret)
	require.True(t, ok)
	require.Equal(t, "s2", sid)
	require.Equal(t, 1, scans, "and a hit needs no rebuild at all")
}

// TestEditorOrigin_WarmUpDeniesWithARetryingNotice pins WHICH framed page a denial
// renders while the daemon is still restoring.
//
// The preview listener binds long before RestoreInstances (#829), so an editor iframe
// can navigate before the daemon has the sessions it needs to recognise the label.
// That address becomes valid seconds later — so rendering the terminal "expired" page
// there strands the frame on a document that never re-requests, exactly the failure
// the recovery path was added to remove.
func TestEditorOrigin_WarmUpDeniesWithARetryingNotice(t *testing.T) {
	warming := true
	gate := &authGate{
		expectedTokenForRequest: func(*http.Request) (string, error) { return "", nil },
		presentedToken:          func(*http.Request) string { return "whatever" },
	}
	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		servePosture(rec, httptest.NewRequest(http.MethodGet, "/", nil),
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
			requestPosture{gate: gate, previewOrigin: true, previewWarmingUp: func() bool { return warming }})
		return rec
	}

	warm := serve()
	require.Contains(t, warm.Body.String(), "Starting up")
	require.Contains(t, warm.Body.String(), `http-equiv="refresh"`,
		"a warm-up denial MUST retry — the address becomes valid when restore finishes")
	require.NotContains(t, warm.Body.String(), "Preview address expired")

	warming = false
	restored := serve()
	require.Contains(t, restored.Body.String(), "Preview address expired")
	require.NotContains(t, restored.Body.String(), `http-equiv="refresh"`,
		"once restored, an unrecognised label is terminal — retrying would spin forever")
}
