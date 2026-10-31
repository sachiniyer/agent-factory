package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestApplyConfigTokenlessNetworkRefusesBind: a tokenless non-loopback address
// is REFUSED — the TCP listener never binds — while the daemon and its unix
// sockets keep working (#5137; the #2168 warn-and-serve posture was the problem
// statement, not the fix). The config writer refuses the second write itself,
// so the test hand-edits the file the way a hand-edit or a stale config would
// reach the daemon, and reconcile must then refuse the bind, record the shared
// refusal on the lifecycle, and warn — without reporting a failed listener key
// (a refusal is not a bind failure).
func TestApplyConfigTokenlessNetworkRefusesBind(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, _, _ := boundWebListeners(t, cfg)

	// Hand-edit past the config-set refusal (#5137: the writer declines this
	// combination, but the file is user-editable and the daemon must refuse the
	// bind rather than serve).
	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n"), 0600))

	result, err := m.ApplyConfig()
	require.NoError(t, err, "a refused listener is still a clean apply — the refusal is posture, not an apply failure")
	require.Empty(t, result.FailedListenerKeys,
		"a refused bind is a decision, not a failed rebind — it must not land in FailedListenerKeys")

	snap := m.lifecycle.snapshot().listeners
	require.Empty(t, snap.TCPBoundAddr, "the refused listener must never bind")
	require.NotEmpty(t, snap.TCPRefusalReason, "the refusal must be recorded for af daemon status / doctor")
	require.Contains(t, snap.TCPRefusalReason, "0.0.0.0:0")
	for _, fix := range []string{"network.require_token true", "af token show",
		"network.listen_addr 127.0.0.1:8443", "network.allow_unauthenticated_network true"} {
		require.Contains(t, snap.TCPRefusalReason, fix,
			"the recorded refusal must carry the same three fixes as every other surface")
	}

	refused := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "refused") && strings.Contains(w, "DeliverPrompt") {
			refused = true
		}
	}
	require.True(t, refused, "the transition into refusal must warn on ApplyConfig, got %v", result.Warnings)
}

// TestApplyConfigTokenlessNetworkOptInBinds is the opted-in twin: the same
// posture with allow_unauthenticated_network = true binds and serves, and the
// apply-time surface carries the exposure notice rather than the refusal.
func TestApplyConfigTokenlessNetworkOptInBinds(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, _, _ := boundWebListeners(t, cfg)

	_, err := config.SetGlobalConfigValue("allow_unauthenticated_network", "true")
	require.NoError(t, err)
	_, err = config.SetGlobalConfigValue("listen_addr", "0.0.0.0:0")
	require.NoError(t, err, "under the opt-in the tokenless network write must succeed")
	_, err = config.SetGlobalConfigValue("require_token", "false")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys, "the opted-in network bind must succeed, not fail")

	exposed := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "require_token is false") {
			exposed = true
		}
	}
	require.True(t, exposed, "the opted-in exposure notice must be surfaced at save time, got %v", result.Warnings)
	snap := m.lifecycle.snapshot().listeners
	require.NotEmpty(t, snap.TCPBoundAddr, "the listener must have bound the network address")
	require.Empty(t, snap.TCPRefusalReason, "an opted-in listener is serving, not refused")
}

// TestApplyConfigRefusalClearsOnOptIn is the live-reconfig half: a refused
// listener must bind on the next reconcile once the opt-in lands — the refusal
// is posture, not a startup check — and TCPRefusalReason must clear.
func TestApplyConfigRefusalClearsOnOptIn(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, _, _ := boundWebListeners(t, cfg)

	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n"), 0600))
	_, err := m.ApplyConfig()
	require.NoError(t, err)
	require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
		"precondition: the tokenless network posture must be refused")

	_, err = config.SetGlobalConfigValue("allow_unauthenticated_network", "true")
	require.NoError(t, err)
	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys)
	snap := m.lifecycle.snapshot().listeners
	require.Empty(t, snap.TCPRefusalReason, "the opt-in must clear the recorded refusal")
	require.NotEmpty(t, snap.TCPBoundAddr, "the opted-in listener must bind on reconcile")
}

// TestApplyConfigRefusalRetiresBoundSocketBeforeTheSwap pins the #5137 ordering
// guard: the auth keys are live-posture, so ApplyConfig's live-config swap would
// relax the per-request gate on a still-bound socket the moment it landed, while
// reconcile's refusal bookkeeping runs afterward — an apply-window in which a
// network socket serves the control API unauthenticated.
// retireWebBeforePostureSwap runs BEFORE the swap, so a hand edit that strips
// the token under a bound network listener retires the socket first.
func TestApplyConfigRefusalRetiresBoundSocketBeforeTheSwap(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true // tokened: the network bind itself is allowed
	m, _, addr := boundWebListeners(t, cfg)
	require.False(t, config.IsLoopbackListenAddr(addr),
		"anti-vacuous: the fixture must be serving on a network interface")

	// The socket answers right up to the apply (the token gate may 401 the
	// request, but the listener accepts — getStatus fails the test otherwise).
	getStatus(t, addr, "/v1/health")

	// Hand-edit past the config-set refusal: drop the token under the bound
	// network address with no opt-in — the refused posture.
	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n"), 0600))

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys,
		"a refused listener is a decision, not a failed rebind")

	snap := m.lifecycle.snapshot().listeners
	require.False(t, snap.TCPBound, "the refused posture must retire the bound socket")
	require.Empty(t, snap.TCPBoundAddr)
	require.NotEmpty(t, snap.TCPRefusalReason, "the refusal must be recorded")

	// The listener is really dead, not just bookkeeping-dead.
	_, dialErr := net.DialTimeout("tcp", addr, 2*time.Second)
	require.Error(t, dialErr, "the retired socket must refuse new connections")
}

// TestRetireWebBeforePostureSwapRetiresRetainedSocket pins the second half of
// the ordering guard (#5137): a failed rebind deliberately retains the old
// listener, so the FILE can name a safe loopback while the SOCKET answering is
// still the old network bind. A later apply that strips the token would carry
// that retained socket into an unauthenticated posture — the guard judges the
// SERVING socket under the incoming auth, retires it without recording a
// refusal (the configured address is allowed), and lets reconcile bind the
// configured one.
func TestRetireWebBeforePostureSwapRetiresRetainedSocket(t *testing.T) {
	server, oldBound := exposureRebindFixture(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\nallow_unauthenticated_network = true\n", true)
	require.False(t, config.IsLoopbackListenAddr(oldBound),
		"anti-vacuous: the retained socket must be network-bound")

	// Move the file to loopback — the rebind FAILS, so the socket keeps serving
	// the old network address while the config names a safe one.
	setGlobalConfigValue(t, "network.listen_addr", "127.0.0.1:0")
	first, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, first.FailedListenerKeys, "network.listen_addr",
		"anti-vacuous: the rebind must have failed so the old socket is retained")
	require.Equal(t, oldBound, server.manager.ListenerAddress("network.listen_addr"),
		"the retained socket still serves the network address")

	// Un-poison the factory so the configured loopback bind succeeds this time.
	wl := server.manager.webListeners
	wl.listenTCP = net.Listen

	// Revoke the opt-in. The RESULTING configured posture (loopback + tokenless)
	// is safe, so no refusal is recorded — but the retained network socket must
	// not be carried into the tokenless posture.
	setGlobalConfigValue(t, "network.allow_unauthenticated_network", "false")
	second, err := server.manager.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, second.FailedListenerKeys,
		"the configured loopback bind must succeed once the socket is retired")

	snap := server.manager.lifecycle.snapshot().listeners
	require.Empty(t, snap.TCPRefusalReason,
		"the configured posture is allowed — no refusal is recorded for the retired retained socket")
	require.True(t, snap.TCPBound, "the configured loopback listener must have bound")
	require.True(t, config.IsLoopbackListenAddr(snap.TCPBoundAddr),
		"the serving socket must be the configured loopback, got %s", snap.TCPBoundAddr)

	_, dialErr := net.DialTimeout("tcp", oldBound, 2*time.Second)
	require.Error(t, dialErr, "the retained network socket must have been retired")
}

// TestApplyConfigFailedAllowedBindClearsStaleRefusal pins finding 4: leaving
// the refused posture for an allowed bind whose bind then FAILS must not keep
// reporting the refusal — the posture is no longer refused, and "configured,
// not bound" is the honest state for a bind that was attempted and failed.
func TestApplyConfigFailedAllowedBindClearsStaleRefusal(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, _ := boundWebListeners(t, cfg)

	// Enter the refused posture via hand edit.
	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n"), 0600))
	_, err := m.ApplyConfig()
	require.NoError(t, err)
	require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
		"precondition: the refusal is recorded")

	// Leave it: opt in, but the bind FAILS. The posture is allowed (explicit
	// opt-in), the listener is genuinely not bound — a stale refusal would be
	// the wrong report on both counts.
	setGlobalConfigValue(t, "allow_unauthenticated_network", "true")
	wl.listenTCP = func(string, string) (net.Listener, error) {
		return nil, errors.New("address already in use (forced by the test)")
	}
	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.FailedListenerKeys, "network.listen_addr",
		"anti-vacuous: the allowed bind must have been attempted and failed")

	snap := m.lifecycle.snapshot().listeners
	require.Empty(t, snap.TCPRefusalReason,
		"the posture is no longer refused — the recorded refusal is stale and must clear")
	require.True(t, snap.TCPConfigured)
	require.Equal(t, "0.0.0.0:0", snap.TCPListenAddr,
		"the configured half names the attempted address, so status reads 'not bound' rather than refused")
	require.False(t, snap.TCPBound)
}

// TestRefusalSeversHijackedWebSocketStreams is the hole retire() cannot close
// alone (#5137): http.Server forgets a connection the moment it is hijacked —
// Shutdown's drain never waits for it and Close never closes it — so an
// unauthenticated client holding /v1/events would keep reading the events
// plane for as long as it liked after the opt-in was withdrawn. The refusal
// retires the listener AND severs its hijacked streams.
func TestRefusalSeversHijackedWebSocketStreams(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	cfg.AllowUnauthenticatedNetwork = true // opt-in: binds and serves unauthenticated
	m, wl, addr := boundWebListeners(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+loopbackDialAddr(addr)+"/v1/events", loopbackDialOpts)
	require.NoError(t, err, "the opted-in tokenless listener admits the events stream")

	// Withdrawing the opt-in refuses the posture live: retire the socket AND
	// sever the stream it already admitted. A plain retire would leave the WS
	// reading forever — the server stopped counting it at the upgrade.
	revoked := *m.Config()
	revoked.AllowUnauthenticatedNetwork = false
	m.storeLivePosture(&revoked)
	failed, err := wl.reconcile(&revoked)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
		"precondition: the posture is refused")

	_, _, err = conn.Read(ctx)
	require.Error(t, err,
		"a hijacked stream must die with the refused listener — the server forgot it at Accept")
}

// TestRefusalSeversWebSocketStreamsAcrossGenerations is the rebind half of the
// same hole (#5137): a stream opened on generation A survives A's graceful
// retirement when the listener moves to B — but its handle is then discarded,
// so a sever that asked only the CURRENT handle could never reach it. The
// refusal must sever every generation the listener kind is still serving.
func TestRefusalSeversWebSocketStreamsAcrossGenerations(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	cfg.AllowUnauthenticatedNetwork = true
	m, wl, addrA := boundWebListeners(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+loopbackDialAddr(addrA)+"/v1/events", loopbackDialOpts)
	require.NoError(t, err, "the opted-in tokenless listener admits the events stream")

	// Rebind to a new generation. The old handle is retired and dropped while
	// the stream on it stays open — exactly the discarded-handle case a
	// per-generation tracker would lose.
	moved := *m.Config()
	moved.ListenAddr = "127.0.0.1:0"
	m.storeLivePosture(&moved)
	failed, err := wl.reconcile(&moved)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.NotEqual(t, addrA, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"precondition: the listener moved to a new generation")

	// Withdrawing the opt-in must sever A's stream too, not just B's.
	revoked := *m.Config()
	revoked.ListenAddr = "0.0.0.0:0"
	revoked.AllowUnauthenticatedNetwork = false
	m.storeLivePosture(&revoked)
	failed, err = wl.reconcile(&revoked)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
		"precondition: the posture is refused")

	_, _, err = conn.Read(ctx)
	require.Error(t, err,
		"a stream still open on a retired generation must die with the refusal — "+
			"its handle was discarded at the rebind, so only tracker-wide severing reaches it")
}

// TestRefusalSeversStreamsLeftOpenByOptOut pins the nil-handle half of the
// tracker-wide sever (#5137 review): a stream opened under the opt-in survives
// the listen_addr="" teardown in the tracker, and a later refused posture must
// reach it even though there is no current handle left to retire.
func TestRefusalSeversStreamsLeftOpenByOptOut(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	cfg.AllowUnauthenticatedNetwork = true
	m, wl, addr := boundWebListeners(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+loopbackDialAddr(addr)+"/v1/events", loopbackDialOpts)
	require.NoError(t, err)

	// The "" opt-out retires the listener gracefully — nothing is bound, but
	// the hijacked stream stays open in the tracker.
	disabled := *m.Config()
	disabled.ListenAddr = ""
	m.storeLivePosture(&disabled)
	failed, err := wl.reconcile(&disabled)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.False(t, m.lifecycle.snapshot().listeners.TCPBound,
		"precondition: the opt-out retired the socket while the stream stayed open")

	// A refused posture must sever the orphaned stream even with no handle.
	refused := *m.Config()
	refused.ListenAddr = "0.0.0.0:0"
	refused.AllowUnauthenticatedNetwork = false
	m.storeLivePosture(&refused)
	failed, err = wl.reconcile(&refused)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
		"precondition: the posture is refused")

	_, _, err = conn.Read(ctx)
	require.Error(t, err,
		"a stream still open after the opt-out must die with the refusal — severing cannot wait for a current handle")
}

// TestTrackerRefusesLateHijacksFromSeveredGenerations pins the generation
// watermark (#5137 review): a WS handler whose upgrade passed the old gate
// before a policy retire can report StateHijacked only AFTER a new listener has
// bound. A shared severing boolean would lift on the new bind and admit that
// conn — an unauthenticated stream surviving indefinitely under the tokened
// posture. The watermark keeps every pre-sever generation dead no matter when
// its stragglers arrive.
func TestTrackerRefusesLateHijacksFromSeveredGenerations(t *testing.T) {
	tr := newConnTracker()
	genA := tr.begin()

	// The refusal severs, then a new (allowed) generation binds — the straggler
	// still has not hijacked.
	tr.sever()
	genB := tr.begin()

	late, latePeer := net.Pipe()
	defer func() { _ = latePeer.Close() }()
	tr.track(genA, late)
	require.Empty(t, tr.conns, "a severed generation's late hijack must not be tracked")
	// net.Pipe's close is asymmetric: the closed end reads ErrClosedPipe, the
	// peer reads EOF — the peer's EOF is what proves the hijack died on arrival.
	_, err := latePeer.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF,
		"the late hijack must be closed on arrival, not admitted to the new generation")

	// The new generation admits and tracks normally.
	fresh, freshPeer := net.Pipe()
	defer func() { _ = freshPeer.Close() }()
	tr.track(genB, fresh)
	require.Len(t, tr.conns, 1, "the post-sever generation is above the watermark")
	require.Contains(t, tr.conns, fresh)
}

// TestClosedWebSocketUnregistersFromTracker pins the bounded-growth half of the
// tracker (#5137 review): a hijacked conn is invisible to http.Server's ConnState
// after the upgrade, so the wrapped conn must remove ITSELF on Close — otherwise
// every completed stream stays referenced for the daemon's life.
func TestClosedWebSocketUnregistersFromTracker(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	cfg.AllowUnauthenticatedNetwork = true
	_, wl, addr := boundWebListeners(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+loopbackDialAddr(addr)+"/v1/events", loopbackDialOpts)
	require.NoError(t, err)

	tracked := func() int {
		wl.webTracker.mu.Lock()
		defer wl.webTracker.mu.Unlock()
		return len(wl.webTracker.conns)
	}
	// The assert cannot be a bare Equal: WriteHeader(101) flushes to the wire
	// BEFORE hj.Hijack() reaches setState(StateHijacked) — net/http's
	// hijackLocked Resets (discards) the buffered writer, so the response the
	// client read must already have gone out. websocket.Dial returning proves
	// the upgrade was accepted, not that track() has run; the registration is
	// guaranteed in-flight on the handler goroutine, so wait for it.
	require.Eventually(t, func() bool { return tracked() == 1 },
		5*time.Second, time.Millisecond,
		"the upgrade must register the hijacked conn")

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "done"))
	require.Eventually(t, func() bool { return tracked() == 0 },
		5*time.Second, 10*time.Millisecond,
		"a closed stream must unregister itself — the tracker may not hold it for the daemon's life")
}

// TestRefusalFreezesRetiredListenerOnServedPosture pins the drain window a bare
// retire leaves open (#5137 review): retire lets in-flight handlers finish, and
// an already-accepted keep-alive conn can dispatch a request that reaches
// withLivePosture AFTER the refused config has published — reading live, that
// request evaluates tokenless on a socket the daemon just refused. The retired
// listener's posture must stay frozen on the config it was serving.
func TestRefusalFreezesRetiredListenerOnServedPosture(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true
	m, wl, _ := boundWebListeners(t, cfg)

	// The ApplyConfig sequence, at the two points where it touches the
	// listener: the pre-swap refusal retire, then the publish of the refused
	// (tokenless, no opt-in) config.
	next := *cfg
	next.RequireToken = false
	wl.retireWebBeforePostureSwap(&next)
	m.storeLivePosture(&next)

	require.False(t, m.Config().RequireToken, "the refused tokenless config is now live")
	wl.webFreeze.mu.Lock()
	pins := append([]frozenRange(nil), wl.webFreeze.pins...)
	wl.webFreeze.mu.Unlock()
	require.Len(t, pins, 1, "a refusal retire must pin the gate to the config the socket was serving")
	require.True(t, pins[0].cfg.RequireToken,
		"drain-window requests must evaluate the posture they were admitted under")
	require.False(t, pins[0].cfg.AllowUnauthenticatedNetwork)
	require.Equal(t, wl.postureSeq, pins[0].through,
		"the pin must cover every control generation issued so far, not just the bound handle")
}

// TestRefusalFreezesEveryDrainingGeneration is the cross-generation half of the
// drain window (#5137 review): a refusal can land while an OLDER listener
// generation is still draining after a normal rebind — a freeze scoped to the
// bound handle would leave that earlier socket's keep-alive requests gating on
// the just-published refused config. The pin covers every generation the kind
// ever issued; a listener bound after the refusal reads live.
func TestRefusalFreezesEveryDrainingGeneration(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true
	m, wl, _ := boundWebListeners(t, cfg)

	// A normal rebind issues generation 2 and retires generation 1, which can
	// still be draining when the refusal below lands.
	rebind := *cfg
	rebind.ListenAddr = "127.0.0.1:0"
	failed, err := wl.reconcile(&rebind)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.Equal(t, uint64(2), wl.postureSeq, "the rebind must issue a second generation")

	// The refusal apply: pre-swap retire, then publish the refused config.
	next := *cfg
	next.ListenAddr = "0.0.0.0:0"
	next.RequireToken = false
	wl.retireWebBeforePostureSwap(&next)
	m.storeLivePosture(&next)

	wl.webFreeze.mu.Lock()
	pins := append([]frozenRange(nil), wl.webFreeze.pins...)
	wl.webFreeze.mu.Unlock()
	require.Len(t, pins, 1, "the refusal must pin the kind's shared freeze")
	require.True(t, pins[0].cfg.RequireToken,
		"draining generations gate on the config they were serving, never the refused one")
	require.Equal(t, uint64(2), pins[0].through,
		"the pin covers the retired generation AND the one bound at the refusal")

	// The retired generation's posture consults the freeze; a post-refusal
	// generation has a seq beyond `through` and reads live.
	draining := &livePosture{snapshot: m.Config, freeze: &wl.webFreeze, seq: 1}
	require.True(t, draining.current().RequireToken,
		"the earlier generation keeps the posture it was admitted under")
	nextGen := &livePosture{snapshot: m.Config, freeze: &wl.webFreeze, seq: pins[0].through + 1}
	require.False(t, nextGen.current().RequireToken,
		"a listener bound after the refusal was admitted under the newer config and reads live")
}

// TestPostureFreezeNeverPinsARefusedConfig pins the guard on the freeze value:
// a refused posture was never legitimately served, so re-pinning drains onto
// it would hand them the very gate the freeze keeps out — an earlier pin must
// survive a later refused config arriving at the same code path.
func TestPostureFreezeNeverPinsARefusedConfig(t *testing.T) {
	served := config.DefaultConfig()
	served.RequireToken = true
	refused := *served
	refused.ListenAddr = "0.0.0.0:0"
	refused.RequireToken = false
	require.NotEmpty(t, config.ListenerBindRefusal(&refused))

	var f postureFreeze
	f.pin(served, 3)
	f.pin(&refused, 5)
	require.Len(t, f.pins, 1, "a refused config must never join the freeze")
	require.Same(t, served, f.pins[0].cfg, "a refused config must never replace a serving pin")
	require.Equal(t, uint64(3), f.pins[0].through, "the rejected pin must not advance the covered generations")
}

// TestPostureFreezePreservesEachGenerationsOwnPin pins the security rule the
// ranges exist for (#5137 review): a later pin must not move an ALREADY-covered
// generation onto a weaker config. A tokened generation frozen during a
// relaxation stays tokened even when a subsequent refusal freezes a newer
// generation onto the tokenless posture IT legitimately served under the
// opt-in.
func TestPostureFreezePreservesEachGenerationsOwnPin(t *testing.T) {
	tokened := config.DefaultConfig()
	tokened.RequireToken = true
	optInTokenless := *tokened
	optInTokenless.ListenAddr = "0.0.0.0:0"
	optInTokenless.RequireToken = false
	optInTokenless.AllowUnauthenticatedNetwork = true
	require.Empty(t, config.ListenerBindRefusal(&optInTokenless))

	var f postureFreeze
	f.pin(tokened, 2)
	f.pin(&optInTokenless, 5)

	live := func() *config.Config { return &optInTokenless }
	older := &livePosture{snapshot: live, freeze: &f, seq: 2}
	got := older.current()
	require.True(t, got.RequireToken,
		"the earlier generation keeps the pin that first covered it — never re-pinned weaker")
	require.False(t, got.AllowUnauthenticatedNetwork)
	newer := &livePosture{snapshot: live, freeze: &f, seq: 5}
	require.Same(t, &optInTokenless, newer.current(),
		"the newer generation froze onto the posture it was actually serving")
}

// TestAuthPostureRelaxedCatchesLoopbackTokenDrop pins the predicate's
// loopback half (#5137 review): require_loopback_token true→false relaxes the
// gate even while require_token stays on, because the switch is exactly what
// exempts loopback peers under it.
func TestAuthPostureRelaxedCatchesLoopbackTokenDrop(t *testing.T) {
	old := config.DefaultConfig()
	old.RequireToken = true
	old.RequireLoopbackToken = true
	incoming := *old
	incoming.RequireLoopbackToken = false
	require.True(t, authPostureRelaxed(old, &incoming),
		"dropping the loopback-token switch exempts loopback peers even while require_token holds")

	steady := *old
	require.False(t, authPostureRelaxed(old, &steady), "an unchanged posture relaxes nothing")
	incomingTighten := *old
	incomingTighten.RequireToken = false
	incomingTighten.RequireLoopbackToken = true
	require.True(t, authPostureRelaxed(old, &incomingTighten),
		"dropping require_token relaxes even when the loopback switch tightens")
	require.False(t, authPostureRelaxed(&incoming, old),
		"the reverse direction is a tightening, not a relaxation")

	// The mixed transition from the review finding: require_loopback_token
	// was INERT under the old tokenless posture (tokenDisabled already
	// admitted everyone), so turning require_token on while flipping the
	// loopback switch off tightens the effective gate — pinning drains to the
	// old tokenless config would hide the strengthen.
	oldTokenless := *old
	oldTokenless.RequireToken = false
	incomingMixed := oldTokenless
	incomingMixed.RequireToken = true
	incomingMixed.RequireLoopbackToken = false
	require.False(t, authPostureRelaxed(&oldTokenless, &incomingMixed),
		"require_token false→true is never a relaxation, whatever the inert loopback flag does")
}

// TestAuthRelaxationFreezesDrainingGenerations pins the shape from the review
// finding exactly: a tokened network listener rebound to a tokened loopback
// one leaves the first generation draining off webHandle's view, and a second
// apply that drops require_token must still freeze it — the serving-socket
// refusal check sees only the loopback generation and never fires.
func TestAuthRelaxationFreezesDrainingGenerations(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true
	m, wl, _ := boundWebListeners(t, cfg)

	// The normal rebind: generation 2 (loopback) supersedes generation 1
	// (network), which keeps draining invisibly to webHandle/webConfigAddr.
	rebind := *cfg
	rebind.ListenAddr = "127.0.0.1:0"
	failed, err := wl.reconcile(&rebind)
	require.NoError(t, err)
	require.Empty(t, failed)

	// The relaxation apply the finding describes: tokenless loopback is an
	// ALLOWED config, so no refusal path runs — yet generation 1's still-
	// draining network socket must not start answering unauthenticated.
	next := *cfg
	next.ListenAddr = "127.0.0.1:0"
	next.RequireToken = false
	require.Empty(t, config.ListenerBindRefusal(&next),
		"tokenless loopback is an allowed posture — no refusal fires")
	wl.retireWebBeforePostureSwap(&next)
	m.storeLivePosture(&next)

	wl.webFreeze.mu.Lock()
	pins := append([]frozenRange(nil), wl.webFreeze.pins...)
	wl.webFreeze.mu.Unlock()
	require.Len(t, pins, 1,
		"an auth-relaxing apply must freeze draining generations onto the serving config")
	require.True(t, pins[0].cfg.RequireToken,
		"the retired network generation keeps requiring the token it was admitted under")
	require.Equal(t, uint64(1), pins[0].through,
		"the relaxation pin covers generation 1 but NOT generation 2 — the bound listener must keep the live read or the allowed apply would never reach it")

	// Generation 2 is still bound and serves the posture just written;
	// generation 1 stays on the tokened config it was admitted under.
	bound := &livePosture{snapshot: m.Config, freeze: &wl.webFreeze, seq: 2}
	require.False(t, bound.current().RequireToken,
		"the bound loopback generation reads live and serves the allowed tokenless posture")
	draining := &livePosture{snapshot: m.Config, freeze: &wl.webFreeze, seq: 1}
	require.True(t, draining.current().RequireToken,
		"the retired network generation keeps gating on the config that admitted it")
}

// TestRelaxationRebindFreezesBoundGeneration pins the other half of the
// relaxation drain window (#5137 review): when the relaxing apply also MOVES
// listen_addr, the bound generation is being retired too — publish would
// otherwise hand it the relaxed posture for the whole bind gap, and a failed
// rebind would retain it weakened indefinitely. The pin covers it, so the
// retained loopback socket keeps requiring the token it admitted with until a
// successor is adopted.
func TestRelaxationRebindFreezesBoundGeneration(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.RequireToken = true
	cfg.RequireLoopbackToken = true
	m, wl, _ := boundWebListeners(t, cfg)

	// Move to a network address while dropping the loopback-token switch:
	// allowed (require_token still holds), but generation 1 — a loopback
	// socket that admitted peers under "everyone tokens" — is being retired
	// by this same apply.
	next := *cfg
	next.ListenAddr = "0.0.0.0:0"
	next.RequireLoopbackToken = false
	require.Empty(t, config.ListenerBindRefusal(&next))
	wl.retireWebBeforePostureSwap(&next)
	m.storeLivePosture(&next)

	wl.webFreeze.mu.Lock()
	pins := append([]frozenRange(nil), wl.webFreeze.pins...)
	wl.webFreeze.mu.Unlock()
	require.Len(t, pins, 1, "the relaxing rebind must freeze the generation it retires")
	require.Equal(t, uint64(1), pins[0].through,
		"the pin reaches the bound generation itself — it does not keep the live read across the bind gap")
	require.True(t, pins[0].cfg.RequireLoopbackToken)

	retained := &livePosture{snapshot: m.Config, freeze: &wl.webFreeze, seq: 1}
	got := retained.current()
	require.True(t, got.RequireToken)
	require.True(t, got.RequireLoopbackToken,
		"a retained socket after a failed rebind keeps its admitting posture, not the relaxed one")
}

// TestFrozenPostureStillTightensFromLive pins the overlay the other direction
// (#5137 review): a pin is a floor, not a ceiling — a later apply that
// TIGHTENS auth must reach generations frozen under a weaker posture, or a
// drain pinned to a loopback-exempt config would keep bypassing the bearer
// token after the operator turned it on.
func TestFrozenPostureStillTightensFromLive(t *testing.T) {
	pinned := config.DefaultConfig()
	pinned.RequireToken = true
	pinned.RequireLoopbackToken = false // loopback peers exempt — the weaker pin
	live := *pinned
	live.RequireLoopbackToken = true // the later tighten

	var f postureFreeze
	f.pin(pinned, 1)
	frozen := &livePosture{snapshot: func() *config.Config { return &live }, freeze: &f, seq: 1}
	require.True(t, frozen.current().RequireLoopbackToken,
		"a tightening apply must reach the frozen generation — the pin is a floor, not a ceiling")

	// And the converse, already pinned elsewhere: a live RELAXATION never
	// lowers a frozen gate.
	liveRelaxed := *pinned
	liveRelaxed.RequireToken = false
	strictPin := config.DefaultConfig()
	strictPin.RequireToken = true
	strictPin.RequireLoopbackToken = true
	var f2 postureFreeze
	f2.pin(strictPin, 1)
	frozen2 := &livePosture{snapshot: func() *config.Config { return &liveRelaxed }, freeze: &f2, seq: 1}
	got := frozen2.current()
	require.True(t, got.RequireToken)
	require.True(t, got.RequireLoopbackToken,
		"a live relaxation must not lower the gate a generation was pinned with")
}

// TestFailedRebindUnfreezesRetainedGeneration pins the settle judgement
// (#5137 review): when the relaxing rebind's bind FAILS, the generation it
// pinned for the publish→adopt window is RETAINED — and no pin may gate a
// still-serving socket, or it would require the old posture indefinitely
// while ApplyConfig reports the relaxation applied. The apply's settle
// unfroze it, so the live config the operator wrote reaches it.
func TestFailedRebindUnfreezesRetainedGeneration(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.RequireToken = true
	cfg.RequireLoopbackToken = true
	m, wl, _ := boundWebListeners(t, cfg)

	// Occupy the rebind's target port so the bind fails and generation 1 is
	// retained rather than superseded.
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer hold.Close()

	next := *cfg
	next.ListenAddr = hold.Addr().String()
	next.RequireLoopbackToken = false
	wl.retireWebBeforePostureSwap(&next)
	m.storeLivePosture(&next)

	wl.webFreeze.mu.Lock()
	pinned := len(wl.webFreeze.pins)
	wl.webFreeze.mu.Unlock()
	require.Equal(t, 1, pinned, "the relaxing rebind pinned the bound generation for the bind gap")

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr)
	require.Contains(t, failed, "network.listen_addr")

	wl.webFreeze.mu.Lock()
	pins := wl.webFreeze.pins
	wl.webFreeze.mu.Unlock()
	require.Empty(t, pins, "a retained generation returns to the live read — the pin outlives nothing it must not gate")
	retained := &livePosture{snapshot: m.Config, freeze: &wl.webFreeze, seq: 1}
	require.False(t, retained.current().RequireLoopbackToken,
		"the retained socket serves the relaxed posture the apply actually committed")
}

// TestOptInRevocationRejectsLateDrainHandlers pins the revocation fence
// (#5137 review): an opted-in tokenless network listener whose opt-in the
// apply revokes is refused — and its drains must NOT keep the unauthenticated
// admission they were serving, or a handler that crosses the gate during the
// drain still enters the control API anonymously after the operator revoked
// permission for exactly that. The refusal pin is the served posture
// TIGHTENED to require the token.
func TestOptInRevocationRejectsLateDrainHandlers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	cfg.AllowUnauthenticatedNetwork = true
	m, wl, _ := boundWebListeners(t, cfg)

	// Revoke the opt-in without changing require_token: the config is now
	// refused, and the served tokenless posture must not pin the drain.
	next := *cfg
	next.AllowUnauthenticatedNetwork = false
	require.NotEmpty(t, config.ListenerBindRefusal(&next))
	wl.retireWebBeforePostureSwap(&next)
	m.storeLivePosture(&next)

	wl.webFreeze.mu.Lock()
	pins := append([]frozenRange(nil), wl.webFreeze.pins...)
	wl.webFreeze.mu.Unlock()
	require.Len(t, pins, 1)
	require.True(t, pins[0].cfg.RequireToken,
		"the refusal pin must be the served posture TIGHTENED — revoking the opt-in cannot leave drains answering unauthenticated")
	require.True(t, pins[0].cfg.RequireLoopbackToken)

	draining := &livePosture{snapshot: m.Config, freeze: &wl.webFreeze, seq: 1}
	require.True(t, draining.current().RequireToken,
		"a late handler on the refused socket now crosses a token-required gate")
}

// TestPostureFreezeExpiresDrainedRanges pins the reclamation bound (#5137
// review): a pin exists to carry a generation through its ~seconds retire
// drain, and nothing else ever removes ranges — without expiry a long-running
// daemon would accumulate a config snapshot per relax/refuse apply and grow
// the mutex'd scan on every request's admission path. Ranges older than the
// horizon (tens of multiples of the drain window) are reclaimed at the next
// pin/consult, and a generation under an expired range reads live — which is
// unreachable in practice because its drain ended long before expiry.
func TestPostureFreezeExpiresDrainedRanges(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RequireToken = true

	var f postureFreeze
	f.pins = []frozenRange{{cfg: cfg, through: 3, pinnedAt: time.Now().Add(-2 * posturePinHorizon)}}

	// A generation under an expired range falls back to the live read: its
	// drain ended long before expiry, so consulting it is already dead.
	live := *cfg
	live.RequireToken = false
	live.AllowUnauthenticatedNetwork = true
	drained := &livePosture{snapshot: func() *config.Config { return &live }, freeze: &f, seq: 2}
	require.False(t, drained.current().RequireToken)

	// And a fresh pin reclaims the dead range rather than growing beside it.
	f.pin(cfg, 4)
	require.Len(t, f.pins, 1,
		"expired ranges are reclaimed at the next pin — pins cannot grow without bound")
	require.Equal(t, uint64(4), f.pins[0].through)
}

// TestRetireWebBeforePostureSwapKeepsSandboxCredentials: retiring the control
// listener before a relaxed posture publishes is about the client listener only.
// A sandbox's callback rides the dedicated endpoint (#5571 W3.4), so the fence
// must leave its credentials registered (W3.6).
func TestRetireWebBeforePostureSwapKeepsSandboxCredentials(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true
	m, wl, _ := boundWebListeners(t, cfg)

	a, err := m.sandboxTokens.mint("sess-a")
	require.NoError(t, err)
	b, err := m.sandboxTokens.mint("sess-b")
	require.NoError(t, err)

	next := *cfg
	next.RequireToken = false
	wl.retireWebBeforePostureSwap(&next)
	for _, secret := range []string{a, b} {
		_, ok := m.sandboxTokens.sessionFor(secret)
		require.True(t, ok, "the pre-swap fence retired the client listener and must not touch sandbox credentials")
	}
}

// TestNonRefusalRetireLeavesPostureLive pins the boundary from the other side
// (#2480): a retire that is NOT a refusal — here the listen_addr="" opt-out —
// leaves the gate on the live read, so an auth tightening keeps applying to the
// draining conn it arrived on rather than being latched off with the old socket.
func TestNonRefusalRetireLeavesPostureLive(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	cfg.AllowUnauthenticatedNetwork = true
	_, wl, _ := boundWebListeners(t, cfg)

	next := *cfg
	next.ListenAddr = ""
	next.RequireToken = true
	failed, err := wl.reconcile(&next)
	require.NoError(t, err)
	require.Empty(t, failed)

	wl.webFreeze.mu.Lock()
	pins := wl.webFreeze.pins
	wl.webFreeze.mu.Unlock()
	require.Empty(t, pins,
		"a non-refusal retire must keep the live-posture read, not latch the old config")
}
