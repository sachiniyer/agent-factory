package session

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
