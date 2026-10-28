package session

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reattachFixture(t *testing.T, srvToken string) (*Instance, *sandboxProvisioner) {
	t.Helper()
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
	return inst, p
}

func TestReattachSandboxBindsALiveAgentServerWithoutProvisioning(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	require.NoError(t, inst.ReattachSandbox())
	assert.True(t, inst.started)
	assert.Equal(t, LiveReady, inst.liveness)
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

	addr, withCallback, err := p.openTunnelForReattach()
	require.NoError(t, err, "the data plane must open even when the -R cannot")
	assert.NotEmpty(t, addr)
	assert.False(t, withCallback, "the caller is told the callback forward did not come up")
	assert.Equal(t, []string{"127.0.0.1:23456:/s.sock"}, p.reverseFwds,
		"the forward stays on the provisioner so the next supervised re-dial carries it")
}
