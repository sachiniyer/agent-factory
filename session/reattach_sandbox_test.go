package session

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reattachFixture(t *testing.T, srvToken string) (*Instance, *sandboxProvisioner) {
	t.Helper()
	inst, p, _ := reattachFixtureStops(t, srvToken, false)
	return inst, p
}

// reattachFixtureStops is reattachFixture whose stand-in agent-server also serves
// /v1/agent/stop, counting the calls and failing them when failStop is set.
func reattachFixtureStops(t *testing.T, srvToken string, failStop bool) (*Instance, *sandboxProvisioner, *atomic.Int32) {
	t.Helper()
	stops := &atomic.Int32{}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	key, err := runtimetoken.EnsureKey()
	require.NoError(t, err)
	ref, err := runtimetoken.NewTokenRef()
	require.NoError(t, err)
	want, err := runtimetoken.DeriveAgentServerToken(key, ref)
	require.NoError(t, err)
	if srvToken == "" {
		srvToken = want
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/alive", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+srvToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": agentAliveResp{Alive: true}})
	})
	mux.HandleFunc("/v1/agent/stop", func(w http.ResponseWriter, r *http.Request) {
		stops.Add(1)
		if failStop {
			http.Error(w, "stop is not implemented", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]bool{"ok": true}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	backend, teardown, err := restoreRuntimeCleanup("reattach me", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh fake-target", SessionDir: "/remote/af-sessions/x", RemotePID: "4242",
		RemoteAddr: "127.0.0.1:" + port, AgentServerVersion: "1.0.0", TokenRef: ref,
	}})
	require.NoError(t, err)
	p := backend.(*sandboxBackend).provisioner
	// The "tunnel" is the test server itself: the forward's local end is simply
	// the server's address, so the probe reaches the stand-in agent-server.
	p.startTunnelFn = func(string) (string, error) { return srv.Listener.Addr().String(), nil }
	inst := &Instance{Title: "reattach me", liveness: LiveLost, backend: backend, runtimeTeardown: teardown}
	return inst, p, stops
}

func TestReattachSandboxBindsALiveAgentServerWithoutProvisioning(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	require.NoError(t, inst.ReattachSandbox())
	assert.True(t, inst.started)
	assert.Equal(t, LiveRunning, inst.liveness, "reachability is not idleness")
	assert.NotNil(t, inst.remoteClient)
	require.Len(t, inst.Tabs, 1, "the agent tab is seeded exactly as a launch seeds it")
}

func TestReattachSandboxLeavesTheRowInertWhenTheTokenIsRefused(t *testing.T) {
	inst, _ := reattachFixture(t, "some-other-servers-token")
	err := inst.ReattachSandbox()
	require.Error(t, err)
	assert.False(t, inst.started)
	assert.Equal(t, LiveLost, inst.liveness)
	assert.Nil(t, inst.remoteClient)
	require.NotNil(t, inst.runtimeTeardown, "a failed reattach must keep the row killable")
}

func TestReattachSandboxSkipsRowsWithoutAPersistedIdentity(t *testing.T) {
	backend, teardown, err := restoreRuntimeCleanup("old row", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh fake-target", SessionDir: "/remote/af-sessions/x", RemotePID: "4242",
	}})
	require.NoError(t, err)
	inst := &Instance{Title: "old row", liveness: LiveLost, backend: backend, runtimeTeardown: teardown}
	assert.ErrorIs(t, inst.ReattachSandbox(), ErrReattachIneligible, "rows written before W1.1 keep the inert path")
}

func TestReattachSandboxSkipsTombstonedRows(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	inst.userKilled = true
	assert.ErrorIs(t, inst.ReattachSandbox(), ErrReattachIneligible)
	assert.False(t, inst.started)
}

// The callback identity (ref + -R spec) rides the cleanup handle back into the
// rebuilt provisioner, for both backends that reattach (#5571 W3.6).
func TestCallbackIdentityRoundTripsThroughTheCleanupHandle(t *testing.T) {
	const ref, reverse = "v1.abc", "127.0.0.1:23456:/run/af/sandbox-callback.sock"
	for _, tc := range []struct {
		kind string
		data *RuntimeCleanupData
	}{
		{"sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
			SSHCommand: "ssh t", SessionDir: "/r/x", RemoteAddr: "127.0.0.1:1", TokenRef: "v1.t",
			CallbackRef: ref, CallbackReverse: reverse}}},
		{"ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
			Config: config.SSHConfig{Host: "h.example.com"}, SessionDir: "/r/x", RemoteAddr: "127.0.0.1:1", TokenRef: "v1.t",
			HostKeyVerification: "strict", CallbackRef: ref, CallbackReverse: reverse}}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			backend, _, err := restoreRuntimeCleanup("t", tc.kind, tc.data)
			require.NoError(t, err)
			p := reattachProvisioner(backend)
			require.NotNil(t, p)
			assert.True(t, p.callbackForwardPersisted())
			assert.Equal(t, []string{reverse}, p.reverseFwds, "the restored provisioner re-dials with the same -R")
			assert.Equal(t, reverse, p.callbackReverse())
			assert.Equal(t, ref, p.callbackRef)
		})
	}

	t.Run("half an identity is not restored", func(t *testing.T) {
		p := &sandboxProvisioner{}
		p.restoreCallbackIdentity(ref, "")
		p.restoreCallbackIdentity("", reverse)
		assert.False(t, p.callbackForwardPersisted())
		assert.Empty(t, p.callbackReverse())
	})
}

type restoringCreds struct {
	fakeCreds
	err  error
	refs []string
}

func (r *restoringCreds) RestoreCallback(ref string) error {
	r.refs = append(r.refs, ref)
	return r.err
}

// Reattach re-registers the persisted credential before it reopens the
// transport, so the sandbox's already-delivered token keeps working (W3.6).
func TestReattachRestoresTheCallbackCredential(t *testing.T) {
	inst, p := reattachFixture(t, "")
	p.restoreCallbackIdentity("v1.cb", "127.0.0.1:23456:/s.sock")
	creds := &restoringCreds{}
	inst.sandboxCreds = creds

	require.NoError(t, inst.ReattachSandbox())
	assert.Equal(t, []string{"v1.cb"}, creds.refs)
	assert.Empty(t, inst.callbackUnavailable, "a restored callback must not leave a degraded reason on the row")
}

func TestReattachSaysSoWhenTheCallbackCannotBeRestored(t *testing.T) {
	t.Run("restore refused", func(t *testing.T) {
		inst, p := reattachFixture(t, "")
		p.restoreCallbackIdentity("v1.cb", "127.0.0.1:23456:/s.sock")
		inst.sandboxCreds = &restoringCreds{err: errors.New("key missing")}
		require.NoError(t, inst.ReattachSandbox(), "the data plane still reattaches")
		assert.Contains(t, inst.callbackUnavailable, "key missing")
	})
	t.Run("no restorer attached", func(t *testing.T) {
		inst, p := reattachFixture(t, "")
		p.restoreCallbackIdentity("v1.cb", "127.0.0.1:23456:/s.sock")
		require.NoError(t, inst.ReattachSandbox())
		assert.NotEmpty(t, inst.callbackUnavailable)
	})
	t.Run("a row that never had a callback says nothing", func(t *testing.T) {
		inst, _ := reattachFixture(t, "")
		inst.sandboxCreds = &restoringCreds{err: errors.New("must not be called")}
		require.NoError(t, inst.ReattachSandbox())
		assert.Empty(t, inst.callbackUnavailable)
	})
}

// ssh's ExitOnForwardFailure makes a -R whose remote port is still held fatal
// to the whole generation. Reattach must still open the data plane — without the
// callback — and keep the forward for the supervisor's next re-dial.
func TestOpenTunnelForReattachFallsBackWithoutTheCallbackForward(t *testing.T) {
	forceTunnelLifelineExe(t, "")
	prev := waitForSandboxTunnelFn
	var p *sandboxProvisioner
	waitForSandboxTunnelFn = func(string, string, time.Duration, time.Duration) error {
		// Stand-in readiness probe: a generation that exits on its own never
		// became ready.
		p.reapMu.Lock()
		child := p.tunnel
		p.reapMu.Unlock()
		select {
		case <-child.done:
			return errors.New("the tunnel exited")
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	}
	t.Cleanup(func() { waitForSandboxTunnelFn = prev })

	// An ssh stand-in that dies whenever it is asked for a -R.
	p = &sandboxProvisioner{sshCmd: `sh -c 'case "$*" in *" -R "*) exit 1;; esac; sleep 60' x`, remoteAddr: "127.0.0.1:7"}
	p.restoreCallbackIdentity("v1.cb", "127.0.0.1:23456:/s.sock")
	t.Cleanup(func() {
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
	})

	addr, withCallback, note, err := p.openTunnelForReattach()
	require.NoError(t, err, "the data plane must open even when the -R cannot")
	assert.NotEmpty(t, addr)
	assert.False(t, withCallback, "the caller is told the callback forward did not come up")
	assert.Contains(t, note, "could not be confirmed")
	assert.Equal(t, []string{"127.0.0.1:23456:/s.sock"}, p.reverseFwds,
		"the forward stays on the provisioner so the next supervised re-dial carries it")
}

// confirmedTunnelFixture swaps the readiness probe and the confirmation for a fixture
// whose ssh child stays up, like OpenSSH does while a remote-forward reply is
// still in flight.
func confirmedTunnelFixture(t *testing.T, confirm func(*sandboxProvisioner) error) *sandboxProvisioner {
	t.Helper()
	forceTunnelLifelineExe(t, "")
	prevWait, prevConfirm := waitForSandboxTunnelFn, confirmCallbackForward
	waitForSandboxTunnelFn = func(string, string, time.Duration, time.Duration) error { return nil }
	confirmCallbackForward = confirm
	t.Cleanup(func() { waitForSandboxTunnelFn, confirmCallbackForward = prevWait, prevConfirm })
	p := &sandboxProvisioner{sshCmd: `sh -c 'sleep 60' x`, remoteAddr: "127.0.0.1:7", sessionDir: "/work/s"}
	p.restoreCallbackIdentity("v1.cb", "/work/s/af-callback.sock:/home/u/sandbox-callback.sock")
	t.Cleanup(func() {
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
	})
	return p
}

// #5896: the -L listener answers before sshd's verdict on the -R arrives, so an
// unconfirmed forward must NOT be reported as restored.
func TestOpenTunnelForReattachDoesNotReportAnUnconfirmedCallbackForward(t *testing.T) {
	p := confirmedTunnelFixture(t, func(*sandboxProvisioner) error { return errors.New("the sandbox host never bound the socket") })
	addr, withCallback, note, err := p.openTunnelForReattach()
	require.NoError(t, err, "the data plane still opens")
	assert.NotEmpty(t, addr)
	assert.False(t, withCallback)
	assert.Contains(t, note, "never bound the socket")
	assert.NotEmpty(t, p.reverseFwds, "the supervisor keeps the forward for its next re-dial")
}

func TestOpenTunnelForReattachReportsAConfirmedCallbackForward(t *testing.T) {
	p := confirmedTunnelFixture(t, func(*sandboxProvisioner) error { return nil })
	_, withCallback, note, err := p.openTunnelForReattach()
	require.NoError(t, err)
	assert.True(t, withCallback)
	assert.Empty(t, note)
}

func TestConfirmCallbackForwardNeedsTheSocketOnTheSandboxHost(t *testing.T) {
	prevT, prevP := callbackForwardConfirmTimeout, callbackForwardConfirmPoll
	callbackForwardConfirmTimeout, callbackForwardConfirmPoll = 400*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { callbackForwardConfirmTimeout, callbackForwardConfirmPoll = prevT, prevP })

	var calls int
	p := &sandboxProvisioner{callbackSocket: "/work/s/af-callback.sock"}
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		calls++
		assert.Contains(t, script, "test -S")
		if calls < 3 {
			return nil, errors.New("exit status 1") // sshd's reply not processed yet
		}
		return nil, nil
	}
	require.NoError(t, confirmCallbackForward(p), "the asynchronous reply is polled for")

	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) { return nil, errors.New("exit status 1") }
	err := confirmCallbackForward(p)
	require.Error(t, err, "a socket that never appears is a rejected forward")
	assert.Contains(t, err.Error(), "never bound")

	done := make(chan struct{})
	close(done)
	p.tunnel = &tunnelChild{done: done}
	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) { return nil, nil }
	err = confirmCallbackForward(p)
	require.Error(t, err, "an ssh that exited on ExitOnForwardFailure is not a working path even if a stale socket exists")

	empty := &sandboxProvisioner{}
	require.Error(t, confirmCallbackForward(empty), "an unknown socket path cannot be confirmed")
}

// #5973: reachability proves the agent-server answers, not that the agent is
// idle. A reattached row must not read as idle to ClassifyActivity (which
// `af sessions watch` and task concurrency consume) before an observation says so.
func TestReattachDoesNotPublishIdleOnReachability(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	inst.ID = "reattach-id"
	require.NoError(t, inst.ReattachSandbox())
	assert.NotEqual(t, LiveReady, inst.liveness)

	activity, _ := ClassifyActivity(inst.ToInstanceData())
	assert.NotEqual(t, ActivityIdle, activity, "a still-working agent must not read as idle")
}

// #5974: adopting the same runtime provisions nothing, so a recorded callback
// reason survives unless this reattach positively restored the callback.
func TestReattachPreservesAPersistedCallbackReason(t *testing.T) {
	t.Run("no callback was ever granted: the create-time reason stays", func(t *testing.T) {
		inst, _ := reattachFixture(t, "")
		inst.callbackUnavailable = "the daemon could not mint one"
		require.NoError(t, inst.ReattachSandbox())
		assert.Equal(t, "the daemon could not mint one", inst.callbackUnavailable)
	})
	t.Run("a recorded reason is kept over a new loss", func(t *testing.T) {
		inst, p := reattachFixture(t, "")
		p.restoreCallbackIdentity("v1.cb", "127.0.0.1:23456:/s.sock")
		inst.callbackUnavailable = "earlier reason"
		inst.sandboxCreds = &restoringCreds{err: errors.New("key missing")}
		require.NoError(t, inst.ReattachSandbox())
		assert.Equal(t, "earlier reason", inst.callbackUnavailable)
	})
	t.Run("positive evidence clears it", func(t *testing.T) {
		inst, p := reattachFixture(t, "")
		p.restoreCallbackIdentity("v1.cb", "127.0.0.1:23456:/s.sock")
		inst.callbackUnavailable = "lost on an earlier reattach"
		inst.sandboxCreds = &restoringCreds{}
		require.NoError(t, inst.ReattachSandbox())
		assert.Empty(t, inst.callbackUnavailable, "credential restored and forward confirmed")
	})
}
