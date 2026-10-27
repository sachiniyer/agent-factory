package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// A responder built before boot-config reporting cannot be compared with the
// file on disk. Doctor must expose that evidence gap instead of omitting the
// check and leaving the user to assume the running daemon has applied edits.
func TestDaemonConfig_OlderResponderIsExplicitlyUnknown(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = false\n"), 0600))
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon config")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "unknown")
	require.Contains(t, c.Detail, "predates")
	require.False(t, c.Problem, "an older responder is missing evidence, not proven stale config")
}

func TestDaemonConfig_MismatchNamesRunningAndFileValues(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = true\nrequire_loopback_token = true\n"), 0600))
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			DaemonVersion: "1.0.220",
			ServingPID:    42,
			BootConfig: &daemon.DaemonBootConfig{
				ListenAddr: "0.0.0.0:8443", RequireToken: false,
			},
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon config")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, `network.listen_addr: running "0.0.0.0:8443", file "127.0.0.1:8443"`)
	require.Contains(t, c.Detail, "network.require_token: running false, file true")
	require.Contains(t, c.Detail, "network.require_loopback_token: running false, file true")
	require.Contains(t, c.Remediation, "af daemon restart")
	require.True(t, c.Problem)
}

func TestDaemonConfig_MatchPasses(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = true\n"), 0600))
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			DaemonVersion: "1.0.220",
			ServingPID:    42,
			BootConfig: &daemon.DaemonBootConfig{
				ListenAddr: "127.0.0.1:8443", RequireToken: true,
			},
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "daemon config").Status)
}

// TestDaemonPID_UnverifiableGetsAdvisoryNotStaleRow pins the tri-state on the
// doctor surface (#5188): a daemon.pid naming a live af daemon whose home
// could not be bound is inconclusive — possibly this home's own daemon with
// an unreadable frame (the normal macOS shape). The stale-file row must NOT
// fire on it (its remediation suggests deleting the only handle naming a live
// daemon), and the advisory that does fire is not a problem-flagged row.
func TestDaemonPID_UnverifiableGetsAdvisoryNotStaleRow(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			PingErr:         errors.New("no daemon"),
			PIDFilePID:      4242,
			PIDUnverifiable: true,
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	rows := findCheckRows(report, "daemon.pid")
	require.Len(t, rows, 1, "exactly one daemon.pid row: the advisory, not the stale-file verdict")
	require.Contains(t, rows[0].Detail, "could not be verified")
	require.NotContains(t, rows[0].Remediation, "remove the stale",
		"an unproven live pid must never be offered for file removal")
	require.False(t, rows[0].Problem,
		"an inconclusive binding is advisory — it may be this home's own daemon")
}
