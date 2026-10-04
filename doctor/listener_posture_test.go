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

// TestDaemonRefusedListenerOnDiskFixedButRunningStale covers the drift case: the
// file already carries a fix, but the daemon still reports its boot-time refusal
// (a hand-edit is not an apply — nothing has reconciled the running listener).
// That is a WARN about the running daemon being behind the file, not a second
// FAIL — the disk posture already carries the verdict.
func TestDaemonRefusedListenerOnDiskFixedButRunningStale(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)

	// Fixed on disk: loopback + no token is the safe posture.
	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = false\n"), 0600))

	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners: daemon.DaemonListenerStatus{
				HTTPUnixBound:     true,
				TCPConfigured:     true,
				TCPListenAddr:     "0.0.0.0:8443",
				TCPBound:          false,
				TCPRefusalReason:  "network.listen_addr \"0.0.0.0:8443\" is reachable from the network … (refused)",
				PreviewConfigured: false,
			},
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	listenerRows := findCheckRows(report, "listener")
	require.Len(t, listenerRows, 1)
	require.Equal(t, StatusWarn, listenerRows[0].Status,
		"a running daemon behind a fixed file is drift, not a fresh failure")
	require.Contains(t, listenerRows[0].Detail, "0.0.0.0:8443")
	require.Contains(t, listenerRows[0].Detail, "refused")
	require.Contains(t, listenerRows[0].Remediation, "af daemon restart",
		"the fix for running-config drift is the restart that applies the fixed file")
}

// TestDaemonLegacyBoundListenerUnderRefusedDiskPosture is the version-skew
// mirror of TestDaemonRefusedListenerOnDiskFixedButRunningStale: the file
// carries a refused posture but the ANSWERING daemon still reports the socket
// bound. A daemon that enforces #5137 retires the socket on apply, so bound
// here means a pre-refusal build — or a hand-edit no apply has reconciled —
// and reporting the refusal would be a false safe conclusion against a live
// socket. What that socket serves comes from BootConfig: nil means a responder
// that predates the field (and the refusal), which is therefore serving the
// refused-looking posture unauthenticated — a FAIL naming the live exposure.
// A live posture that is itself safe is restart-pending drift, a Warn.
func TestDaemonLegacyBoundListenerUnderRefusedDiskPosture(t *testing.T) {
	testguard.IsolateTmux(t)

	writeRefused := func(opts *Options) {
		require.NoError(t, os.WriteFile(
			filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
			[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\n"), 0600))
	}
	bound := daemon.DaemonListenerStatus{
		HTTPUnixBound: true,
		TCPConfigured: true, TCPListenAddr: "0.0.0.0:8443",
		TCPBound: true, TCPBoundAddr: "0.0.0.0:8443",
	}

	// Legacy daemon: answers Ping but carries no BootConfig, i.e. it predates
	// #2168 Phase 4 — certainly the refusal — so the bound socket is the live
	// exposure the disk posture now refuses.
	opts := testOptions(t, false)
	writeRefused(&opts)
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners:     bound,
		}
	}
	report, err := Run(opts)
	require.NoError(t, err)
	listenerRows := findCheckRows(report, "listener")
	require.Len(t, listenerRows, 1)
	require.Equal(t, StatusFail, listenerRows[0].Status,
		"a still-bound socket under a refused disk posture is a live exposure, not a refused one")
	require.Contains(t, listenerRows[0].Detail, "still serving 0.0.0.0:8443 unauthenticated")
	require.Contains(t, listenerRows[0].Detail, "DeliverPrompt")
	require.NotContains(t, listenerRows[0].Detail, "the TCP listener is refused",
		"the refusal text is a false safe claim while a socket still serves")
	require.Contains(t, listenerRows[0].Remediation, "af daemon restart")

	// Same bound socket but the daemon reports a live posture that keeps it
	// safe — a current daemon mid-drift, not a legacy exposure: Warn.
	opts2 := testOptions(t, false)
	writeRefused(&opts2)
	opts2.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts2.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners:     bound,
			BootConfig:    &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: true},
		}
	}
	report2, err := Run(opts2)
	require.NoError(t, err)
	rows2 := findCheckRows(report2, "listener")
	require.Len(t, rows2, 1)
	require.Equal(t, StatusWarn, rows2[0].Status,
		"a bound socket the live config still authenticates is drift, not an unauthenticated exposure")
	require.Contains(t, rows2[0].Detail, "socket is safe")
	require.Contains(t, rows2[0].Remediation, "af daemon restart")

	// And a BootConfig that reports the live posture unauthenticated confirms
	// the exposure rather than diluting it.
	opts3 := testOptions(t, false)
	writeRefused(&opts3)
	opts3.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts3.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners:     bound,
			BootConfig:    &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: false},
		}
	}
	report3, err := Run(opts3)
	require.NoError(t, err)
	rows3 := findCheckRows(report3, "listener")
	require.Len(t, rows3, 1)
	require.Equal(t, StatusFail, rows3[0].Status)
	require.Contains(t, rows3[0].Detail, "still serving 0.0.0.0:8443 unauthenticated")

	// The retained-socket divergence (#5137): a failed rebind deliberately
	// leaves the OLD network socket answering while the live config already
	// names loopback — testing BootConfig.ListenAddr would read that exposed
	// socket as safe. The bound address is what the exposure is.
	opts4 := testOptions(t, false)
	writeRefused(&opts4)
	opts4.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts4.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners:     bound,
			BootConfig:    &daemon.DaemonBootConfig{ListenAddr: "127.0.0.1:9999", RequireToken: false},
		}
	}
	report4, err := Run(opts4)
	require.NoError(t, err)
	rows4 := findCheckRows(report4, "listener")
	require.Len(t, rows4, 1)
	require.Equal(t, StatusFail, rows4[0].Status,
		"a socket still bound on the network address is exposed no matter what the live config names")
	require.Contains(t, rows4[0].Detail, "still serving 0.0.0.0:8443 unauthenticated")
}

// TestDaemonOptInOnDiskWhileRunningRefuses is the reverse drift of the
// bound-under-refused case (#5137 review): the disk now opts in to the
// unauthenticated listener, but the RUNNING daemon still refuses it — a
// hand-edit is not an apply, so nothing serves. The opted-in "serves the
// control API" Warn would claim a live exposure that does not exist and
// contradict the refusal row; the single Warn here is the pending restart —
// still naming what the restart will expose.
func TestDaemonOptInOnDiskWhileRunningRefuses(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)

	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\nallow_unauthenticated_network = true\n"), 0600))

	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners: daemon.DaemonListenerStatus{
				HTTPUnixBound:    true,
				TCPConfigured:    true,
				TCPListenAddr:    "0.0.0.0:8443",
				TCPBound:         false,
				TCPRefusalReason: "network.listen_addr \"0.0.0.0:8443\" is reachable from the network … (refused)",
			},
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	listenerRows := findCheckRows(report, "listener")
	require.Len(t, listenerRows, 1,
		"one row owns the drift — the exposure claim must not print beside the live refusal")
	require.Equal(t, StatusWarn, listenerRows[0].Status)
	require.Contains(t, listenerRows[0].Detail, "refused to bind")
	require.Contains(t, listenerRows[0].Detail, "no authentication",
		"the restart's consequence — the opt-in WILL expose — must stay in the warning")
	require.NotContains(t, listenerRows[0].Detail, "is reachable from the network",
		"the opted-in row's present-tense reachability claim is false while the daemon refuses")
	require.Contains(t, listenerRows[0].Remediation, "af daemon restart")
}

// TestDaemonOptInOnDiskUnderBoundSocket is the bound counterpart of
// TestDaemonOptInOnDiskWhileRunningRefuses (#5137 review): the disk opts in
// while a daemon still running its last-applied config answers. Whether the
// socket is an exposure right now is the LIVE token gate's answer, not the
// file's — a tokened responder under an opted-in file is restart-pending
// drift, and only a socket genuinely serving unauthenticated earns the
// present-tense claim.
func TestDaemonOptInOnDiskUnderBoundSocket(t *testing.T) {
	testguard.IsolateTmux(t)

	bound := daemon.DaemonListenerStatus{
		HTTPUnixBound: true,
		TCPConfigured: true, TCPListenAddr: "0.0.0.0:8443",
		TCPBound: true, TCPBoundAddr: "0.0.0.0:8443",
	}
	writeOpted := func(opts *Options) {
		require.NoError(t, os.WriteFile(
			filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
			[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\nallow_unauthenticated_network = true\n"), 0600))
	}

	// The tokened responder: the opt-in is written but nothing is exposed yet —
	// restart-pending drift, not the live-exposure claim.
	opts := testOptions(t, false)
	writeOpted(&opts)
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners:     bound,
			BootConfig:    &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: true},
		}
	}
	report, err := Run(opts)
	require.NoError(t, err)
	listenerRows := findCheckRows(report, "listener")
	require.Len(t, listenerRows, 1)
	require.Equal(t, StatusWarn, listenerRows[0].Status)
	require.Contains(t, listenerRows[0].Detail, "nothing is exposed yet",
		"a tokened live socket under an opted-in file is pending drift, not an exposure")
	require.NotContains(t, listenerRows[0].Detail, "serves 0.0.0.0:8443 from the network with no authentication",
		"the present-tense serving claim is false while the responder enforces the token")

	// The tokenless responder IS the opt-in's exposure — the same Warn the
	// daemon-down case draws, now true of the answering socket.
	opts2 := testOptions(t, false)
	writeOpted(&opts2)
	opts2.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:    filepath.Join(opts2.ConfigDir, "daemon.sock"),
			SocketExists:  true,
			PingErr:       nil,
			HTTPListening: daemon.AnswerYes(),
			Listeners:     bound,
			BootConfig:    &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: false},
		}
	}
	report2, err := Run(opts2)
	require.NoError(t, err)
	rows2 := findCheckRows(report2, "listener")
	require.Len(t, rows2, 1)
	require.Equal(t, StatusWarn, rows2[0].Status)
	require.Contains(t, rows2[0].Detail, "serves 0.0.0.0:8443 from the network with no authentication")
	require.False(t, rows2[0].Problem,
		"an opted-in live exposure is still the operator's choice — Warn, not Problem")
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
