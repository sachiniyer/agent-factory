package doctor

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func webListenerReport(t *testing.T, l daemon.DaemonListenerStatus) *Report {
	t.Helper()
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)
	sockPath := filepath.Join(opts.ConfigDir, "daemon.sock")
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    sockPath,
			DaemonVersion: "dev",
			Phase:         daemon.DaemonPhaseReady,
			Listeners:     l,
			HTTPListening: daemon.Undetermined(errNoDaemon),
		}
	}
	report, err := Run(opts)
	require.NoError(t, err)
	return report
}

// #6386: a daemon that could not bind the configured web listener (port in
// use) used to leave every doctor row green while the README sent the user to
// an unserved URL. It is a WARN, not a FAIL — the daemon works without the web
// UI — that names the address, the bind error and the remedy.
func TestUnboundWebListenerWarns(t *testing.T) {
	report := webListenerReport(t, daemon.DaemonListenerStatus{
		TCPConfigured: true,
		TCPListenAddr: "127.0.0.1:8443",
		TCPBindError:  "listen tcp 127.0.0.1:8443: bind: address already in use",
		BindRetrying:  true,
	})
	rows := findCheckRows(report, "web listener")
	require.Len(t, rows, 1, "checks: %v", checkNames(report))
	require.Equal(t, StatusWarn, rows[0].Status)
	require.Contains(t, rows[0].Detail, "127.0.0.1:8443")
	require.Contains(t, rows[0].Detail, "address already in use")
	require.Contains(t, rows[0].Remediation, "network.listen_addr")
}

func TestBoundOrDisabledWebListenerHasNoWarning(t *testing.T) {
	for name, l := range map[string]daemon.DaemonListenerStatus{
		"bound":    {TCPConfigured: true, TCPBound: true, TCPListenAddr: "127.0.0.1:8443", TCPBoundAddr: "127.0.0.1:8443"},
		"disabled": {},
		// Still starting: no bind has failed yet, so there is nothing to report.
		"starting": {TCPConfigured: true, TCPListenAddr: "127.0.0.1:8443"},
	} {
		t.Run(name, func(t *testing.T) {
			report := webListenerReport(t, l)
			for _, c := range report.Checks {
				require.NotEqual(t, "web listener", c.Name)
			}
		})
	}
}
