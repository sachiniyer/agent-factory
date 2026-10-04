package daemon

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// withVersion sets the recorded build version for one test.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := Version()
	t.Cleanup(func() { SetVersion(prev) })
	SetVersion(v)
}

// Ping is the probe every client uses to learn the daemon's version, so it must
// carry it — an OK with no version is what a pre-#1044 daemon returns, and a
// client reads that as skew.
func TestPing_ReportsRecordedVersion(t *testing.T) {
	withVersion(t, "1.0.192")

	var resp PingResponse
	s := &controlServer{}
	require.NoError(t, s.Ping(PingRequest{}, &resp))
	require.True(t, resp.OK)
	require.True(t, resp.AccountHandoff)
	require.Equal(t, "1.0.192", resp.Version)
}

// An unset version must stay empty rather than inventing a value: "" is the
// wire signal for "older than version reporting", and a placeholder here would
// make a genuinely stale daemon look current.
func TestPing_UnsetVersionStaysEmpty(t *testing.T) {
	withVersion(t, "")

	var resp PingResponse
	s := &controlServer{}
	require.NoError(t, s.Ping(PingRequest{}, &resp))
	require.True(t, resp.OK)
	require.Empty(t, resp.Version)
}

// Phase 4 of #2168 needs status and doctor to compare the daemon that actually
// answered Ping with the installed unit and the config currently on disk. Pin
// those facts on Ping itself: a PID file can be stale, and rereading config in
// the client cannot reveal what the already-running daemon is enforcing.
func TestPing_ReportsProcessAndBootConfig(t *testing.T) {
	m := &Manager{cfg: &config.Config{
		ListenAddr:                  "0.0.0.0:8443",
		RequireToken:                true,
		RequireLoopbackToken:        true,
		AllowUnauthenticatedNetwork: true,
	}}
	s := &controlServer{manager: m}

	var resp PingResponse
	require.NoError(t, s.Ping(PingRequest{}, &resp))

	data, err := json.Marshal(resp)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Equal(t, float64(os.Getpid()), wire["pid"],
		"the responding process, not daemon.pid, is the supervision identity")
	require.Equal(t, map[string]any{
		"listen_addr":                   "0.0.0.0:8443",
		"require_token":                 true,
		"require_loopback_token":        true,
		"allow_unauthenticated_network": true,
	}, wire["boot_config"], "with no live swap the boot config is the answer")

	// The name is historical — the fields report the LIVE posture
	// (Manager.Config()), because the listener/auth keys apply live (#2480) and
	// every BootConfig consumer asks what this daemon enforces NOW: a
	// live-applied write must change the answer, not read as drift.
	live := *m.cfg
	live.RequireToken = false
	live.AllowUnauthenticatedNetwork = false
	m.live.Store(&live)

	var after PingResponse
	require.NoError(t, s.Ping(PingRequest{}, &after))
	require.NotNil(t, after.BootConfig)
	require.False(t, after.BootConfig.RequireToken,
		"BootConfig must follow the live-applied posture, not the frozen startup config")
	require.False(t, after.BootConfig.AllowUnauthenticatedNetwork)
	require.True(t, after.BootConfig.RequireLoopbackToken, "untouched fields still report")
}

func TestSetVersion_RoundTrips(t *testing.T) {
	withVersion(t, "1.2.3")
	require.Equal(t, "1.2.3", Version())
}
