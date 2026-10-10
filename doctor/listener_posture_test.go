package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestDaemonUnauthenticatedListenerRefused is the #5137 doctor contract, and it
// flips the #2168 Phase 0 assertion that stood here — which had itself flipped
// #2090's "the daemon cannot start" row.
//
// The owner reversed warn-and-serve: a non-loopback listen_addr with
// require_token off and no allow_unauthenticated_network opt-in is a posture
// the daemon REFUSES to bind. So the `daemon` row stays a Pass — the daemon
// does still start, and the unix socket still works — while the `listener` row
// is the FAIL: a configured, reachable-looking API surface that never binds is
// the thing the operator almost certainly meant to have up. The row carries
// ListenerBindRefusal verbatim so the three fixes read identically to the
// startup log and the `af config set` refusal.
func TestDaemonUnauthenticatedListenerRefused(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)

	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\n"), 0600))

	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  false, // simply not started yet
			PingErr:       errNoDaemon,
			HTTPListening: daemon.Undetermined(errNoDaemon),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	// The daemon row stays the plain truth: nothing is running, and it will
	// start when asked — the refusal is listener-scoped, never process-level.
	daemonRows := findCheckRows(report, "daemon")
	require.Len(t, daemonRows, 1)
	require.Equal(t, StatusPass, daemonRows[0].Status,
		"the daemon still starts under this config — the FAIL belongs to the listener it refuses to bind")

	// The refusal is reported, on its own row, as a FAIL with all three fixes.
	listenerRows := findCheckRows(report, "listener")
	require.Len(t, listenerRows, 1)
	require.Equal(t, StatusFail, listenerRows[0].Status,
		"a listener the daemon refuses to bind is a failure, not a warning")
	require.True(t, listenerRows[0].Problem,
		"the refused listener must drive a nonzero `af doctor` exit — the configured API surface is dead by policy")
	require.Contains(t, listenerRows[0].Detail, "0.0.0.0:8443")
	require.Contains(t, listenerRows[0].Detail, "DeliverPrompt",
		"say what an unauthenticated peer could reach, not just that auth is off")
	require.Contains(t, listenerRows[0].Detail, "refused",
		"the row must say the listener is refused — that is the fact an operator must act on")
	for _, fix := range []string{
		"network.require_token true", "af token show",
		"network.listen_addr 127.0.0.1:8443", "network.allow_unauthenticated_network true",
	} {
		require.Contains(t, listenerRows[0].Detail, fix,
			"the FAIL must name all three fixes, shared with the startup log and config-set refusal")
	}
}

// TestDaemonUnauthenticatedListenerOptInWarnsWithoutFailing is the other half
// of #5137: the opt-in is the operator's explicit acceptance of the risk, so
// `af doctor` must not fail on a posture the operator asked for — but the
// listener still serves the control API unauthenticated, so the exposure is
// still SAID, as a non-problem advisory.
func TestDaemonUnauthenticatedListenerOptInWarnsWithoutFailing(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)

	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\nallow_unauthenticated_network = true\n"), 0600))

	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  false,
			PingErr:       errNoDaemon,
			HTTPListening: daemon.Undetermined(errNoDaemon),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	listenerRows := findCheckRows(report, "listener")
	require.Len(t, listenerRows, 1)
	require.Equal(t, StatusWarn, listenerRows[0].Status,
		"an opted-in unauthenticated network listener is still worth saying — as a warning, not a failure")
	require.False(t, listenerRows[0].Problem,
		"the opt-in is the operator's explicit choice — it must not drive a nonzero `af doctor` exit")
	require.Contains(t, listenerRows[0].Detail, "0.0.0.0:8443")
	require.Contains(t, listenerRows[0].Detail, "DeliverPrompt",
		"say what an unauthenticated peer can actually do, not just that auth is off")
	require.Contains(t, listenerRows[0].Remediation, "network.require_token true")
	require.Contains(t, listenerRows[0].Remediation, "network.allow_unauthenticated_network false",
		"the undo for an advisory must be named, not only the tighten")
}

// TestDaemonNotRunningStillPassesOnASafeConfig guards the other direction: the
// exposure row must fire on the unsafe posture ONLY. An ordinary user who has just
// not started a daemon yet gets the on-demand pass and no listener row at all.
func TestDaemonNotRunningStillPassesOnASafeConfig(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)

	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = false\n"), 0600))

	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  false,
			PingErr:       errNoDaemon,
			HTTPListening: daemon.Undetermined(errNoDaemon),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	rows := findCheckRows(report, "daemon")
	require.Len(t, rows, 1)
	require.Equal(t, StatusPass, rows[0].Status,
		"the loopback default is safe — it must keep the on-demand pass")
	require.Empty(t, findCheckRows(report, "listener"),
		"nothing is exposed on the shipped default, so there is nothing to warn about")
}

// TestAuthenticatedNetworkListenerIsNotWarned pins that require_token = true is
// untouched by #2168 Phase 0. A network bind that authenticates its peers is the
// posture the docs recommend; warning about it would train users to ignore the
// row that matters.
func TestAuthenticatedNetworkListenerIsNotWarned(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)

	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = true\n"), 0600))

	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  false,
			PingErr:       errNoDaemon,
			HTTPListening: daemon.Undetermined(errNoDaemon),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Empty(t, findCheckRows(report, "listener"),
		"a network bind with the token on is authenticated — no exposure to report")
}
