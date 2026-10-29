package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// `af daemon status` against a remote target reports the TARGET's health, and
// never consults this machine's sockets, pid file or autostart unit.
func TestDaemonStatusFollowsTheRemoteTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/health", r.URL.Path)
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{OK: true, Version: "9.9.9", BootID: "boot-1", Phase: daemon.DaemonPhase("ready")}))
	}))
	defer srv.Close()
	t.Setenv("AF_DAEMON_URL", srv.URL)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	prev := daemonHealthFn
	daemonHealthFn = func() daemon.HealthStatus {
		t.Error("remote status consulted this machine's daemon health")
		return daemon.HealthStatus{}
	}
	t.Cleanup(func() { daemonHealthFn = prev })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	require.NoError(t, daemonStatusCmd.RunE(cmd, nil))
	require.Contains(t, out.String(), "daemon at "+srv.URL+": running")
	require.Contains(t, out.String(), "9.9.9")

	out.Reset()
	daemonStatusJSON = true
	t.Cleanup(func() { daemonStatusJSON = false })
	require.NoError(t, daemonStatusCmd.RunE(cmd, nil))
	var env struct {
		Data remoteDaemonStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	require.True(t, env.Data.Running)
	require.Equal(t, srv.URL, env.Data.Target)
	require.Equal(t, "9.9.9", env.Data.Version)

	// An unreachable target reports "not responding", not this machine's state.
	srv.Close()
	out.Reset()
	daemonStatusJSON = false
	require.NoError(t, daemonStatusCmd.RunE(cmd, nil))
	require.Contains(t, out.String(), "not responding")
}
