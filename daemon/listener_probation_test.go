package daemon

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// stubUpgradeJournal swaps the journal-load seam: reconcile's candidate
// deferral consults the on-disk upgrade transaction, and a test drives the
// branches by naming what the "disk" answers.
func stubUpgradeJournal(t *testing.T, journal upgradetxn.Journal, err error) {
	t.Helper()
	prev := loadUpgradeJournalFn
	loadUpgradeJournalFn = func(string) (upgradetxn.Journal, error) { return journal, err }
	t.Cleanup(func() { loadUpgradeJournalFn = prev })
}

// journalExpectingBound is the active-transaction journal of a candidate whose
// previous daemon had (or had not) its TCP listener bound.
func journalExpectingBound(id string, bound bool) upgradetxn.Journal {
	return upgradetxn.Journal{
		ID:     id,
		Daemon: upgradetxn.DaemonSnapshot{Listeners: upgradetxn.ListenerExpectation{TCPBound: bound}},
	}
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

// writeProbationConfig hand-edits config.toml the way a stale file or a
// hand-edit reaches a candidate — the `af config` writers refuse the refused
// posture, so the file is the only way in.
func writeProbationConfig(t *testing.T, body string) {
	t.Helper()
	tomlPath := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte(body), 0600))
}

// TestUpgradeCandidateDefersRefusalWhileJournalExpectsBound is the #5183
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

	t.Run("the journaled candidate keeps the socket bound", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5183")
		stubUpgradeJournal(t, journalExpectingBound("txn-5183", true), nil)

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
		req, err := http.NewRequest(http.MethodGet, "http://"+loopbackDialAddr(addr)+"/v1/health", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := loopbackHTTPClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"the floor tightens rather than refuses — a peer WITH the token still gets the control API")
	})

	t.Run("a journal expecting no bound listener still refuses", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5183")
		stubUpgradeJournal(t, journalExpectingBound("txn-5183", false), nil)

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		snap := m.lifecycle.snapshot().listeners
		require.False(t, snap.TCPBound)
		require.NotEmpty(t, snap.TCPRefusalReason,
			"the deferral exists so a candidate RESTORES the old listener, not to invent one")
		require.False(t, m.tokenFloorArmed(), "nothing is carried, so nothing is floored")
	})

	t.Run("a journal for a different transaction still refuses", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5183")
		stubUpgradeJournal(t, journalExpectingBound("txn-other", true), nil)

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
			"a stale or foreign journal must never defer the refusal")
	})

	t.Run("a cleaned-up journal lets a late reconcile refuse", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "txn-5183")
		stubUpgradeJournal(t, upgradetxn.Journal{}, errors.New("no active upgrade transaction"))

		failed, err := wl.reconcile(m.Config())
		require.NoError(t, err)
		require.Empty(t, failed)
		require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
			"after the transaction is gone the same candidate posture refuses — the deferral ends with the journal")
	})

	t.Run("an ordinary daemon never defers", func(t *testing.T) {
		m, wl := upgradeCandidateListeners(t, refusedCfg(), "")
		stubUpgradeJournal(t, journalExpectingBound("txn-5183", true), nil)

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
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5183")
	stubUpgradeJournal(t, journalExpectingBound("txn-5183", true), nil)

	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	addr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "precondition: the candidate deferred the refusal and bound")
	require.Equal(t, http.StatusUnauthorized, getStatus(t, addr, "/v1/health"),
		"the deferred socket stays bound but demands the token — never serves the refused posture")

	// An apply that reads back the refused posture — the file a hand-edit
	// would leave — must not take the retire-first path either.
	writeProbationConfig(t, "[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n")
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
// released-candidate half of the probation floor: probation blocks config
// writes, but a candidate whose journal is still live admits them — and a
// write that turns the token OFF under a retained socket must not leave that
// socket serving unauthenticated. The deferral exists to keep a socket BOUND
// for the old supervisor; it is not a license to serve the incoming posture
// when the journal recorded no auth state.
func TestUpgradeCandidateFloorsRetainedSocketOnTokenlessWrite(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true // bound tokened — the posture a pre-#5137 daemon could serve
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5183")
	stubUpgradeJournal(t, journalExpectingBound("txn-5183", true), nil)

	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	addr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "precondition: the candidate bound the journaled socket")

	// The divergence: the file moves to a tokenless LOOPBACK posture while the
	// rebind fails, so the socket that keeps answering is the old NETWORK one.
	// Without the floor the swap would publish require_token=false onto it —
	// and the settle unfreeze returns the retained generation to the live read.
	writeProbationConfig(t, "[network]\nlisten_addr = '127.0.0.1:0'\nrequire_token = false\n")
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
// journal's mid-apply disappearance creates (#5137 review): the carry decision
// is read ONCE per apply — retireWebBeforePostureSwap records it and reconcile
// consumes it — so a journal present at that read governs the whole apply even
// if the journal is gone by the time reconcile runs: the socket stays bound
// AND floored. The refusal then lands on the NEXT apply, whose own read sees
// the journal gone: the floor must stay armed through that retire and clear
// only after the socket is gone — a request landing between an early clear
// and the retire would read the swapped tokenless config with no floor and be
// served the full control API unauthenticated.
func TestUpgradeCandidateFloorHeldThroughJournalLossTeardown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5183")

	// The deferral establishes the bound+floored socket first.
	journal := journalExpectingBound("txn-5183", true)
	stubUpgradeJournal(t, journal, nil)
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	addr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "precondition: the deferred socket is bound")
	require.True(t, m.tokenFloorArmed(), "precondition: the floor is armed")

	// The journal then vanishes: present for the first apply's single read
	// (socket kept bound+floored through the whole apply) and gone for the
	// second's (the refusal retires it for real). stubUpgradeJournal's cleanup
	// restores the production seam.
	journalCalls := 0
	loadUpgradeJournalFn = func(string) (upgradetxn.Journal, error) {
		journalCalls++
		if journalCalls == 1 {
			return journal, nil
		}
		return upgradetxn.Journal{}, errors.New("no active upgrade transaction")
	}

	// Any request answered 200 anywhere in the window is the hole: the floor
	// was cleared while a socket still served the tokenless posture. 401 =
	// floored, 0 = already gone — both safe; only 200 means unfloored serving.
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
			resp, err := loopbackHTTPClient.Get("http://" + loopbackDialAddr(addr) + "/v1/health")
			if err != nil {
				answers <- 0
				continue
			}
			_ = resp.Body.Close()
			answers <- resp.StatusCode
		}
	}()

	writeProbationConfig(t, "[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n")

	// Apply 1: the journal is still there for the pre-swap read, so the carry
	// decision stands for the whole apply — the socket stays bound and floored
	// even though the journal is gone by reconcile.
	_, err = m.ApplyConfig()
	require.NoError(t, err)
	require.Equal(t, 1, journalCalls,
		"one journal read per apply — reconcile consumes the pre-swap decision, it does not re-read")
	snap := m.lifecycle.snapshot().listeners
	require.True(t, snap.TCPBound,
		"the carry decision governs the whole apply — a mid-apply journal loss cannot split it")
	require.True(t, m.tokenFloorArmed(), "the floor rides the kept socket")

	// Apply 2: the journal is gone for this apply's own read, so the refusal
	// runs for real — retiring the socket while the floor still holds.
	_, err = m.ApplyConfig()
	require.NoError(t, err)
	require.Equal(t, 2, journalCalls,
		"anti-vacuous: the journal was present for apply 1's read and gone for apply 2's")
	close(stop)
	for code := range answers {
		require.NotEqual(t, http.StatusOK, code,
			"a 200 in the teardown window means the floor cleared before the socket retired")
	}

	snap = m.lifecycle.snapshot().listeners
	require.False(t, snap.TCPBound, "with the journal gone the refused socket retires for real")
	require.NotEmpty(t, snap.TCPRefusalReason)
	require.False(t, m.tokenFloorArmed(),
		"the floor clears once the socket it protected is gone")
}

// TestPingReportsProbationTokenFloor pins the status-surface half of the floor:
// the deferred socket demands the bearer token while the loaded file reads
// tokenless, so Ping must publish the enforced posture — a raw require_token
// would have status and doctor calling a floored socket unauthenticated.
func TestPingReportsProbationTokenFloor(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false // refused on the file — the deferral floors the gate
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5183")
	stubUpgradeJournal(t, journalExpectingBound("txn-5183", true), nil)
	cs := &controlServer{manager: m}

	var before PingResponse
	require.NoError(t, cs.Ping(PingRequest{}, &before))
	require.NotNil(t, before.BootConfig)
	require.False(t, before.BootConfig.RequireToken,
		"control: with no floor armed Ping reports the tokenless file it booted with")

	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	require.True(t, m.tokenFloorArmed(), "precondition: the deferral floored the gate")

	var resp PingResponse
	require.NoError(t, cs.Ping(PingRequest{}, &resp))
	require.NotNil(t, resp.BootConfig)
	require.True(t, resp.BootConfig.RequireToken,
		"Ping must report what the socket enforces — the floor, not the tokenless file")
}

// TestUpgradeJournalLossMidApplyKeepsFloorOnRetainedSocket pins the journal-read
// TOCTOU (#5137 review): retireWebBeforePostureSwap and reconcile each used to
// read the upgrade journal independently. A journal removed between the two
// reads left the pre-swap phase keeping the socket bound under the floor while
// reconcile computed carries=false, cleared the floor, and — when the applied
// rebind FAILED and retained the network socket — left it answering the new
// tokenless posture unauthenticated. One carry decision now serves the apply.
func TestUpgradeJournalLossMidApplyKeepsFloorOnRetainedSocket(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true // the pre-upgrade daemon serves the network tokened
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5183")
	stubUpgradeJournal(t, journalExpectingBound("txn-5183", true), nil)
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	bound := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, bound)
	require.Equal(t, http.StatusUnauthorized, getStatus(t, bound, "/v1/health"),
		"precondition: the socket answers on the network but demands the token")

	// The apply moves listen_addr to a loopback the bind cannot take AND drops
	// the token requirement, so the retained network socket would serve the
	// tokenless posture — the exact shape the floor exists for. The journal
	// vanishes between the pre-swap phase and reconcile.
	moved := *m.Config()
	moved.ListenAddr = "127.0.0.2:0"
	moved.RequireToken = false
	moved.AllowUnauthenticatedNetwork = false
	wl.listenTCP = func(string, string) (net.Listener, error) {
		return nil, errors.New("forced rebind failure")
	}
	wl.retireWebBeforePostureSwap(&moved) // journal still present: carries
	m.storeLivePosture(&moved)
	stubUpgradeJournal(t, upgradetxn.Journal{}, errors.New("journal gone"))

	failed, err = wl.reconcile(&moved)
	require.Error(t, err)
	require.Contains(t, failed, "network.listen_addr")
	require.True(t, m.tokenFloorArmed(),
		"the floor must outlive the journal's mid-apply disappearance — the socket is still serving")
	require.Equal(t, bound, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"the retained socket still answers on the old network address")
	require.Equal(t, http.StatusUnauthorized, getStatus(t, bound, "/v1/health"),
		"a retained network socket under a tokenless apply keeps demanding the bearer token")
}

// TestApplyConfigExposureNoticeReadsProbationFloor pins the classification gap
// (#5137 review): an upgrade candidate whose journal expects the socket bound
// floors every kept-bound listener to the bearer token — including a bind the
// apply itself just made for an OPTED-IN tokenless network posture. The socket
// answers 401, so the apply-time exposure notice must classify the EFFECTIVE
// posture (require_token OR floor), not the file's tokenless request —
// otherwise ApplyConfig warns "serves its full control API … with no
// authentication" about a socket that authenticates every caller.
func TestApplyConfigExposureNoticeReadsProbationFloor(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.RequireToken = true
	m, wl := upgradeCandidateListeners(t, cfg, "txn-floor")
	stubUpgradeJournal(t, journalExpectingBound("txn-floor", true), nil)
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)

	// The operator opts in to the tokenless network listener mid-upgrade. The
	// apply binds it — floored, because the journal window is open.
	writeProbationConfig(t,
		"[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\nallow_unauthenticated_network = true\n")

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys, "the opted-in bind succeeds — floored, not refused")
	require.True(t, m.tokenFloorArmed(),
		"precondition: the journaled-candidate floor is armed for the kept socket")

	bound := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, bound)
	require.Equal(t, http.StatusUnauthorized, getStatus(t, bound, "/v1/health"),
		"precondition: the floored socket demands the bearer token despite the tokenless file")

	for _, w := range result.Warnings {
		require.NotContains(t, w, "af serves its",
			"the exposure notice must not claim unauthenticated service while the floor gates it: %q", w)
	}
}

// TestApplyConfigRequireTokenOffKeepsSandboxCredentialsUnderProbationFloor:
// turning require_token off no longer revokes sandbox callback credentials
// (#5571 W3.6), including under the probation floor, where the kept socket still
// demands the token and no unauthenticated-exposure claim may be made about it.
func TestApplyConfigRequireTokenOffKeepsSandboxCredentialsUnderProbationFloor(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = true
	m, wl := upgradeCandidateListeners(t, cfg, "txn-revoke")
	stubUpgradeJournal(t, journalExpectingBound("txn-revoke", true), nil)
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	secret, err := m.sandboxTokens.mint("sess-a")
	require.NoError(t, err)

	writeProbationConfig(t, "[network]\nlisten_addr = '0.0.0.0:0'\nrequire_token = false\n")
	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.True(t, m.tokenFloorArmed(), "precondition: the carried socket is floored")
	bound := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, bound, "precondition: the journaled socket stayed bound")
	require.Equal(t, http.StatusUnauthorized, getStatus(t, bound, "/v1/health"),
		"precondition: the socket still demands the token")

	joined := strings.Join(result.Warnings, "\n")
	_, kept := m.sandboxTokens.sessionFor(secret)
	require.True(t, kept, "the credential rides the sandbox-only endpoint and survives the posture change")
	require.NotContains(t, joined, "sandbox callback credential", "nothing was revoked")
	require.NotContains(t, joined, "answers unauthenticated callers",
		"the floored socket authenticates every caller — no live unauthenticated claim")
}

// TestPreSwapNeverDisarmsTheFloorEarly pins the arm-only contract
// (#5137 review): a candidate holding a journaled tokenless-network socket
// under the floor then applies require_token=true. The incoming posture is no
// longer unauthenticated, but the live config still serves the OLD tokenless
// one until ApplyConfig publishes the swap — clearing the floor in the
// pre-swap phase would leave the bound network socket answering
// unauthenticated for the gap. The disarm is reconcile's, and only after the
// published config demands the token itself.
func TestPreSwapNeverDisarmsTheFloorEarly(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false // refused posture — the candidate carries it deferred
	m, wl := upgradeCandidateListeners(t, cfg, "txn-arm")
	stubUpgradeJournal(t, journalExpectingBound("txn-arm", true), nil)
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	require.True(t, m.tokenFloorArmed(), "precondition: the deferred socket is floored")
	bound := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, bound)
	require.Equal(t, http.StatusUnauthorized, getStatus(t, bound, "/v1/health"))

	// The apply tightens to require_token=true — servingUnauthenticatedLocked
	// answers false for the incoming posture, which used to disarm the floor
	// HERE, before the tokened config was ever published.
	moved := *m.Config()
	moved.RequireToken = true
	wl.retireWebBeforePostureSwap(&moved)
	require.True(t, m.tokenFloorArmed(),
		"the pre-swap phase may only ARM the floor — the live config still serves tokenless until the swap")
	require.Equal(t, http.StatusUnauthorized, getStatus(t, bound, "/v1/health"),
		"the socket must still be demanding the token across the swap gap")

	m.storeLivePosture(&moved)
	failed, err = wl.reconcile(&moved)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.False(t, m.tokenFloorArmed(),
		"post-publish the swapped posture demands the token itself — reconcile disarms the floor")
	require.Equal(t, http.StatusUnauthorized, getStatus(t, bound, "/v1/health"),
		"and the demand is unchanged: the socket now authenticates on its own posture")
}

// TestProbationFloorOverlaysFrozenGeneration pins how the floor composes with
// the #5137 posture freeze: a generation frozen onto an opted-in tokenless
// serving config still gets the floor, because the floor overlays whichever
// config the freeze chose — and it only ever ORs require_token, never touching
// the loopback switch or mutating the pinned or published config.
func TestProbationFloorOverlaysFrozenGeneration(t *testing.T) {
	opted := config.DefaultConfig()
	opted.ListenAddr = "0.0.0.0:8443"
	opted.RequireToken = false
	opted.RequireLoopbackToken = false
	opted.AllowUnauthenticatedNetwork = true

	var f postureFreeze
	f.pin(opted, 1)
	require.Len(t, f.pins, 1, "precondition: the opted-in served posture pins generation 1")

	floored := false
	pair := func() (*config.Config, bool) { return opted, floored }
	frozen := &livePosture{posturePair: pair, freeze: &f, seq: 1}
	unfrozen := &livePosture{posturePair: pair, freeze: &f, seq: 2}

	require.False(t, frozen.current().RequireToken,
		"control: unfloored, the frozen tokenless posture is what gates generation 1")

	floored = true
	for name, p := range map[string]*livePosture{"frozen": frozen, "unfrozen": unfrozen} {
		got := p.current()
		require.True(t, got.RequireToken, "%s: the floor must reach every generation", name)
		require.False(t, got.RequireLoopbackToken, "%s: the floor raises require_token only", name)
	}
	require.False(t, opted.RequireToken,
		"the overlay copies — it must never mutate the pinned or published config")

	tokened := *opted
	tokened.RequireToken = true
	require.Same(t, &tokened, withProbationTokenFloor(&tokened),
		"a config that already demands the token passes through without an allocation")
}

// TestProbationFloorDisarmKeepsDrainingNetworkGenerationTokened pins the Codex
// finding on the floor's disarm: a floored, tokenless network socket moves to
// loopback, and a SECOND apply lands while the retired network generation is
// still draining. That apply sees nothing left serving the network and disarms
// the floor — and because both raw configs read tokenless, the pre-swap
// relaxation check used to see no relaxation and pin nothing, so a request
// already dispatched on the draining network listener fell through to the
// unfloored live read and reached the control API unauthenticated. The
// freeze judges the SERVED posture (servedPosture), so the move that retired
// the network generation pins it onto the floored config it was admitted
// under.
func TestProbationFloorDisarmKeepsDrainingNetworkGenerationTokened(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	cfg.RequireToken = false // refused on the file — the deferral floors the gate
	m, wl := upgradeCandidateListeners(t, cfg, "txn-5183")
	stubUpgradeJournal(t, journalExpectingBound("txn-5183", true), nil)
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	require.True(t, m.tokenFloorArmed(), "precondition: the deferred network socket is floored")
	networkSeq := wl.boundSeq
	require.NotZero(t, networkSeq)

	// Apply 1 moves the socket to loopback, still tokenless. The network
	// generation retires into its drain; the floor stays armed through the
	// apply because reconcile judged it while the network socket was bound.
	moved := *m.Config()
	moved.ListenAddr = "127.0.0.1:0"
	wl.retireWebBeforePostureSwap(&moved)
	m.storeLivePosture(&moved)
	failed, err = wl.reconcile(&moved)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.Greater(t, wl.boundSeq, networkSeq, "precondition: a loopback successor adopted")

	// Apply 2 — any write before the drain finishes — disarms the floor.
	again := moved
	wl.retireWebBeforePostureSwap(&again)
	m.storeLivePosture(&again)
	failed, err = wl.reconcile(&again)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.False(t, m.tokenFloorArmed(),
		"anti-vacuous: nothing serves the network any more, so this apply disarmed the floor")

	// The gate exactly as webBind wires it, one per generation.
	gate := func(seq uint64) *livePosture {
		return &livePosture{posturePair: m.authPosturePair, freeze: &wl.webFreeze, seq: seq}
	}
	require.True(t, gate(networkSeq).current().RequireToken,
		"a request still draining on the retired network generation must keep demanding the token after the disarm")
	require.False(t, gate(wl.boundSeq).current().RequireToken,
		"control: the loopback socket in service keeps the live read — its tokenless posture is allowed")
}

// TestLivePosturePublicationNeverPairsTokenlessConfigWithAClearedFloor is the
// regression for the Codex finding on the upgrade-probation gate. The apply
// sequence is three ordered writes — arm the floor, publish the new config,
// disarm once the socket settles — and a request that read cfg and floor as
// two independent atomics could straddle the last two into a pair no real
// state ever held: the OLD tokenless config observed before the publish with
// the floor observed after its clear, admitting the request unauthenticated
// on a socket every neighboring snapshot gated. m.live carries the pair as one
// atomic.Pointer[livePosturePublication], so the property below is met by
// construction: every Load returns a complete publication, and this writer's
// sequence never publishes a tokenless config unfloored, so no reader may
// observe one.
func TestLivePosturePublicationNeverPairsTokenlessConfigWithAClearedFloor(t *testing.T) {
	tokenless := config.DefaultConfig()
	tokenless.RequireToken = false
	tokened := *tokenless
	tokened.RequireToken = true

	m := &Manager{cfg: tokenless}
	// Seed already-armed, mirroring production: the floor goes up BEFORE the
	// first posture that needs it can be published, so a tokenless config
	// exists in the publication only under the floor.
	m.live.Store(&livePosturePublication{cfg: tokenless, tokenFloored: true})

	// The sequential half pins the carry: arming pairs the flag with the
	// config it was decided under, and publishing a new config must carry the
	// armed floor forward rather than silently clearing it.
	c, floored := m.authPosturePair()
	require.Same(t, tokenless, c)
	require.True(t, floored)
	m.storeLivePosture(&tokened)
	c, floored = m.authPosturePair()
	require.Same(t, &tokened, c)
	require.True(t, floored, "a config publish must carry the armed floor forward")
	m.setTokenFloor(false)
	c, floored = m.authPosturePair()
	require.Same(t, &tokened, c)
	require.False(t, floored)
	m.setTokenFloor(true)

	// The concurrent half reproduces the straddle: a writer cycling
	// publish→disarm→arm→publish while readers demand that a tokenless config
	// is never observed with the floor cleared. The sequence never writes a
	// tokenless unfloored pair — that is the state the gate must never serve —
	// so any observed instance is a torn read of two atomics, i.e. this bug.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.storeLivePosture(&tokened)
			m.setTokenFloor(false)
			m.setTokenFloor(true)
			m.storeLivePosture(tokenless)
		}
	}()

	for i := 0; i < 20000; i++ {
		c, floored := m.authPosturePair()
		require.True(t, c.RequireToken || floored,
			"tokenless config + cleared floor is the phantom pair the gate used to admit")
	}
	close(stop)
	wg.Wait()
}
