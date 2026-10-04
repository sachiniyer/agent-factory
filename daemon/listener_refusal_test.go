package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
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

// stubUpgradeJournal swaps the journal-load seam: reconcile's candidate
// deferral consults the on-disk upgrade transaction, and a test drives the
// branches by naming what the "disk" answers.
func stubUpgradeJournal(t *testing.T, journal upgradetxn.Journal, err error) {
	t.Helper()
	prev := loadUpgradeJournalFn
	loadUpgradeJournalFn = func(string) (upgradetxn.Journal, error) { return journal, err }
	t.Cleanup(func() { loadUpgradeJournalFn = prev })
}

// upgradeCandidateListeners builds a Manager carrying an upgrade transaction
// ID — the candidate half of a self-upgrade — and wires its webListeners the
// way boundWebListeners does for the ordinary daemon.
func upgradeCandidateListeners(t *testing.T, cfg *config.Config, transactionID string) (*Manager, *webListeners) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := newManagerShellForDaemon(cfg, transactionID)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	t.Cleanup(func() { _ = wl.close() })
	return m, wl
}

// TestUpgradeCandidateDefersRefusalWhileJournalExpectsBound is the #5137
// upgrade-window fix for the direction the new-binary validation clause cannot
// cover: the supervisor checking a candidate's listeners is the PREVIOUS
// binary's recovery actor, and a pre-#5137 one requires TCPBound when its
// journal recorded it — a refusal there fails validation and rolls back to the
// still-exposed old daemon. So a candidate whose journal expected the socket
// bound keeps it bound for the transaction; adoption replaces it with a fresh
// daemon that refuses honestly. Every non-candidate or mismatched-journal case
// must still refuse — the deferral is not a general bypass.
func TestUpgradeCandidateDefersRefusalWhileJournalExpectsBound(t *testing.T) {
	refusedCfg := func() *config.Config {
		cfg := config.DefaultConfig()
		cfg.ListenAddr = "0.0.0.0:0"
		cfg.RequireToken = false
		return cfg
	}
	journalExpecting := func(id string, bound bool) upgradetxn.Journal {
		return upgradetxn.Journal{
			ID:     id,
			Daemon: upgradetxn.DaemonSnapshot{Listeners: upgradetxn.ListenerExpectation{TCPBound: bound}},
		}
	}

	t.Run("the journaled candidate keeps the socket bound", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5137")
		stubUpgradeJournal(t, journalExpecting("txn-5137", true), nil)

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		snap := m.lifecycle.snapshot().listeners
		require.True(t, snap.TCPBound,
			"a candidate must keep the journaled-bound listener through probation or the old supervisor rolls back")
		require.Empty(t, snap.TCPRefusalReason,
			"a deferred refusal is still serving — recording the reason would claim refused while bound")
		// But bound ≠ unauthenticated: the journal records no auth posture, so
		// the deferred socket cannot tell "the old daemon was already exposed"
		// from "it was tokened and the file was hand-edited" — it demands the
		// token for the whole probation window.
		addr := snap.TCPBoundAddr
		require.NotEmpty(t, addr)
		require.Equal(t, http.StatusUnauthorized, getStatus(t, addr, "/v1/health"),
			"a deferred socket must not serve the control API unauthenticated even while it stays bound")
		tokenPath, err := TokenPath()
		require.NoError(t, err)
		token, err := LoadToken(tokenPath)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/health", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"the floor tightens rather than refuses — a peer WITH the token still gets the control API")
	})

	t.Run("a journal expecting no bound listener still refuses", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5137")
		stubUpgradeJournal(t, journalExpecting("txn-5137", false), nil)

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		snap := m.lifecycle.snapshot().listeners
		require.False(t, snap.TCPBound)
		require.NotEmpty(t, snap.TCPRefusalReason,
			"the deferral exists so a candidate RESTORES the old listener, not to invent one")
	})

	t.Run("a journal for a different transaction still refuses", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5137")
		stubUpgradeJournal(t, journalExpecting("txn-other", true), nil)

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
			"a stale or foreign journal must never defer the refusal")
	})

	t.Run("a cleaned-up journal lets a late reconcile refuse", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5137")
		stubUpgradeJournal(t, upgradetxn.Journal{}, errors.New("no active upgrade transaction"))

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
			"after the transaction is gone the same candidate posture refuses — the deferral ends with the journal")
	})

	t.Run("an ordinary daemon never defers", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "")
		stubUpgradeJournal(t, journalExpecting("txn-5137", true), nil)

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
			"a bound-listener journal belongs to a transaction — a daemon outside it refuses")
	})
}

// TestUpgradeCandidateApplyKeepsJournaledSocket is the apply-time half of the
// same deferral: probation blocks config writes, but a released candidate —
// probation over, journal not yet cleaned up — admits them, and a hand-edit to
// the refused posture must still NOT retire the socket a supervisor
// re-validation can still expect bound.
func TestUpgradeCandidateApplyKeepsJournaledSocket(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5137")
	stubUpgradeJournal(t, upgradetxn.Journal{
		ID:     "txn-5137",
		Daemon: upgradetxn.DaemonSnapshot{Listeners: upgradetxn.ListenerExpectation{TCPBound: true}},
	}, nil)

	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	addr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "precondition: the candidate deferred the refusal and bound")
	require.Equal(t, http.StatusUnauthorized, getStatus(t, addr, "/v1/health"),
		"the deferred socket stays bound but demands the token — never serves the refused posture")

	// An apply that reads back the refused posture — the file a hand-edit
	// would leave — must not take the retire-first path either.
	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n"), 0600))
	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys)
	require.True(t, m.lifecycle.snapshot().listeners.TCPBound,
		"an apply under the active journal must keep the socket the supervisor expects")
	require.Equal(t, http.StatusUnauthorized, getStatus(t, addr, "/v1/health"),
		"the socket is still bound AND still enforcing the token floor")
	require.NotContains(t, result.Warnings, config.ListenerBindRefusal(m.Config()),
		"the deferred candidate is bound and token-gated, not refused — the warning "+
			"must follow the reconcile outcome, not the refused file")
}

// TestUpgradeCandidateFloorsRetainedSocketOnTokenlessWrite is the
// released-candidate half of the probation floor (#5137): probation blocks
// config writes, but a candidate whose journal is still live admits them — and
// a write that turns the token OFF under a retained socket must not leave that
// socket serving unauthenticated. The deferral exists to keep a socket BOUND
// for the old supervisor; it is not a license to serve the incoming posture
// when the journal recorded no auth state.
func TestUpgradeCandidateFloorsRetainedSocketOnTokenlessWrite(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true // bound tokened — the posture a pre-#5137 daemon could serve
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5137")
	stubUpgradeJournal(t, upgradetxn.Journal{
		ID:     "txn-5137",
		Daemon: upgradetxn.DaemonSnapshot{Listeners: upgradetxn.ListenerExpectation{TCPBound: true}},
	}, nil)

	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	addr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "precondition: the candidate bound the journaled socket")

	// The divergence: the file moves to a tokenless LOOPBACK posture while the
	// rebind fails, so the socket that keeps answering is the old NETWORK one.
	// Without the floor the swap would publish require_token=false onto it.
	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("[network]\nlisten_addr = '127.0.0.1:0'\nrequire_token = false\n"), 0600))
	wl.listenTCP = func(string, string) (net.Listener, error) {
		return nil, errors.New("address already in use (forced by the test)")
	}
	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.FailedListenerKeys, "network.listen_addr",
		"anti-vacuous: the loopback rebind must have been attempted and failed, retaining the network socket")
	require.Equal(t, addr, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"the retained socket is still the journaled bound one")

	require.Equal(t, http.StatusUnauthorized, getStatus(t, addr, "/v1/health"),
		"a kept-bound network socket must demand the token through the journal window — "+
			"a published tokenless posture cannot reach it")
}

// TestUpgradeCandidateFloorHeldThroughJournalLossTeardown pins the ordering the
// journal's mid-apply disappearance creates (#5137 review): the deferral check
// runs TWICE per apply — retireWebBeforePostureSwap pre-swap and reconcile
// post-swap — so a journal present for the first but gone for the second leaves
// reconcile holding a still-bound socket with the tokenless posture already
// published onto it. The probation floor must stay armed through the refusal
// teardown and clear only after the socket is gone: a request landing between
// an early clear and the retire would read the swapped config with no floor and
// be served the full control API unauthenticated.
func TestUpgradeCandidateFloorHeldThroughJournalLossTeardown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5137")

	// The deferral establishes the bound+floored socket first.
	journal := upgradetxn.Journal{
		ID:     "txn-5137",
		Daemon: upgradetxn.DaemonSnapshot{Listeners: upgradetxn.ListenerExpectation{TCPBound: true}},
	}
	stubUpgradeJournal(t, journal, nil)
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	addr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "precondition: the deferred socket is bound")
	require.True(t, m.probationTokenFloor.Load(), "precondition: the floor is armed")

	// The journal then vanishes mid-apply: present for the pre-swap check (the
	// socket is kept bound) and gone for reconcile's (the refusal must run for
	// real against a still-serving socket).
	journalCalls := 0
	loadUpgradeJournalFn = func(string) (upgradetxn.Journal, error) {
		journalCalls++
		if journalCalls == 1 {
			return journal, nil
		}
		return upgradetxn.Journal{}, errors.New("no active upgrade transaction")
	}

	// Any request answered 200 in the teardown window is the hole: the floor was
	// cleared before the socket retired. 401 = still floored, 0 = already gone.
	answers := make(chan int, 4096)
	stop := make(chan struct{})
	go func() {
		defer close(answers)
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := http.Get("http://" + addr + "/v1/health")
			if err != nil {
				answers <- 0
				continue
			}
			_ = resp.Body.Close()
			answers <- resp.StatusCode
		}
	}()

	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n"), 0600))
	_, err = m.ApplyConfig()
	require.NoError(t, err)
	close(stop)
	for code := range answers {
		require.NotEqual(t, http.StatusOK, code,
			"a 200 in the teardown window means the floor cleared before the socket retired")
	}

	require.GreaterOrEqual(t, journalCalls, 2,
		"anti-vacuous: the apply must have seen the journal present then gone")
	snap := m.lifecycle.snapshot().listeners
	require.False(t, snap.TCPBound, "with the journal gone the refused socket retires for real")
	require.NotEmpty(t, snap.TCPRefusalReason)
	require.False(t, m.probationTokenFloor.Load(),
		"the floor clears once the socket it protected is gone")
}

// TestPingReportsServingAddrAfterFailedRebind is the status half of the
// retained-socket case (#5137 review): a failed live rebind deliberately keeps
// the PREVIOUS listener serving while Manager.Config() already carries the
// requested address. Ping's BootConfig must therefore report the address the
// listener owner is actually serving (the lifecycle's configured half), not the
// file-shaped request — otherwise RunningConfigMatches sees file==requested==
// reported and answers "yes" for a socket still bound on the old address, and
// doctor misses the pending restart.
func TestPingReportsServingAddrAfterFailedRebind(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.RequireToken = true
	m, wl, addr := boundWebListeners(t, cfg)

	// The live rebind fails: the requested address is stored in the live config
	// but the old socket keeps serving.
	wl.listenTCP = func(string, string) (net.Listener, error) {
		return nil, errors.New("address already in use (forced by the test)")
	}
	moved := *m.Config()
	moved.ListenAddr = "127.0.0.2:0"
	m.live.Store(&moved)
	failed, err := wl.reconcile(&moved)
	require.Error(t, err)
	require.ErrorContains(t, err, "still serving on the previous address",
		"precondition: the rebind was attempted, failed, and retained the old socket")
	require.Contains(t, failed, "network.listen_addr")
	require.Equal(t, "127.0.0.2:0", m.Config().ListenAddr,
		"precondition: the live config already carries the requested address")
	require.Equal(t, addr, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"precondition: the retained socket still answers on the old address")

	var resp PingResponse
	require.NoError(t, (&controlServer{manager: m}).Ping(PingRequest{}, &resp))
	require.NotNil(t, resp.BootConfig)
	require.Equal(t, "127.0.0.1:0", resp.BootConfig.ListenAddr,
		"BootConfig must name the socket's producing address, not the stored request")
	require.Contains(t, RunningConfigDifference(resp.BootConfig, m.Config()),
		`running "127.0.0.1:0", file "127.0.0.2:0"`,
		"the drift report must surface the failed rebind, not claim a match")
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
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/v1/events", nil)
	require.NoError(t, err, "the opted-in tokenless listener admits the events stream")

	// Withdrawing the opt-in refuses the posture live: retire the socket AND
	// sever the stream it already admitted. A plain retire would leave the WS
	// reading forever — the server stopped counting it at the upgrade.
	revoked := *m.Config()
	revoked.AllowUnauthenticatedNetwork = false
	m.live.Store(&revoked)
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
	conn, _, err := websocket.Dial(ctx, "ws://"+addrA+"/v1/events", nil)
	require.NoError(t, err, "the opted-in tokenless listener admits the events stream")

	// Rebind to a new generation. The old handle is retired and dropped while
	// the stream on it stays open — exactly the discarded-handle case a
	// per-generation tracker would lose.
	moved := *m.Config()
	moved.ListenAddr = "127.0.0.1:0"
	m.live.Store(&moved)
	failed, err := wl.reconcile(&moved)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.NotEqual(t, addrA, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"precondition: the listener moved to a new generation")

	// Withdrawing the opt-in must sever A's stream too, not just B's.
	revoked := *m.Config()
	revoked.ListenAddr = "0.0.0.0:0"
	revoked.AllowUnauthenticatedNetwork = false
	m.live.Store(&revoked)
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
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/v1/events", nil)
	require.NoError(t, err)

	// The "" opt-out retires the listener gracefully — nothing is bound, but
	// the hijacked stream stays open in the tracker.
	disabled := *m.Config()
	disabled.ListenAddr = ""
	m.live.Store(&disabled)
	failed, err := wl.reconcile(&disabled)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.False(t, m.lifecycle.snapshot().listeners.TCPBound,
		"precondition: the opt-out retired the socket while the stream stayed open")

	// A refused posture must sever the orphaned stream even with no handle.
	refused := *m.Config()
	refused.ListenAddr = "0.0.0.0:0"
	refused.AllowUnauthenticatedNetwork = false
	m.live.Store(&refused)
	failed, err = wl.reconcile(&refused)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
		"precondition: the posture is refused")

	_, _, err = conn.Read(ctx)
	require.Error(t, err,
		"a stream still open after the opt-out must die with the refusal — severing cannot wait for a current handle")
}

// TestPingReportsProbationTokenFloor pins the status-surface half of the floor
// (#5137 review): the deferred socket demands the bearer token while the loaded
// file reads tokenless, so Ping must publish the enforced posture — a raw
// require_token would have status and doctor calling a floored socket
// unauthenticated.
func TestPingReportsProbationTokenFloor(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false // refused on the file — the deferral floors the gate
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5137")
	stubUpgradeJournal(t, upgradetxn.Journal{
		ID:     "txn-5137",
		Daemon: upgradetxn.DaemonSnapshot{Listeners: upgradetxn.ListenerExpectation{TCPBound: true}},
	}, nil)

	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	require.True(t, m.probationTokenFloor.Load(), "precondition: the deferral floored the gate")

	var resp PingResponse
	require.NoError(t, (&controlServer{manager: m}).Ping(PingRequest{}, &resp))
	require.NotNil(t, resp.BootConfig)
	require.True(t, resp.BootConfig.RequireToken,
		"Ping must report what the socket enforces — the floor, not the tokenless file")
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
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/v1/events", nil)
	require.NoError(t, err)

	tracked := func() int {
		wl.webTracker.mu.Lock()
		defer wl.webTracker.mu.Unlock()
		return len(wl.webTracker.conns)
	}
	require.Equal(t, 1, tracked(), "the upgrade must register the hijacked conn")

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "done"))
	require.Eventually(t, func() bool { return tracked() == 0 },
		5*time.Second, 10*time.Millisecond,
		"a closed stream must unregister itself — the tracker may not hold it for the daemon's life")
}
