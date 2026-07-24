package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestCollectDaemonStatusNoDaemon runs the read-only probe against a fresh
// temp home where no daemon is running: it must report not-running and resolve
// both socket paths under that home without dialing or spawning anything.
func TestCollectDaemonStatusNoDaemon(t *testing.T) {
	// SocketTempDir, not t.TempDir: this resolves the daemon socket paths, and on
	// macOS a t.TempDir() home is ~107 bytes — past sun_path, so resolution now
	// fails with the #1940 guard. The real home (~/.agent-factory) is short.
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	info := collectDaemonStatus()
	if info.Running {
		t.Fatal("expected Running=false with no daemon in a fresh home")
	}
	if !strings.HasPrefix(info.ControlSocket, home) {
		t.Fatalf("control socket %q not under temp home %q", info.ControlSocket, home)
	}
	if !strings.HasPrefix(info.HTTPSocket, home) {
		t.Fatalf("http socket %q not under temp home %q", info.HTTPSocket, home)
	}
	if info.ControlSocketFile || info.HTTPSocketFile {
		t.Fatal("no socket files should exist in a fresh home")
	}
}

func TestPrintDaemonStatusHumanRunning(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running:       true,
		Version:       "1.2.3",
		BootID:        "boot-123",
		TransactionID: "transaction-123",
		Phase:         daemon.DaemonPhaseUpgradeProbation,
		Listeners: &daemon.DaemonListenerStatus{
			HTTPUnixBound: true,
			TCPConfigured: true,
			TCPBound:      true,
			TCPBoundAddr:  "127.0.0.1:8443",
		},
		ControlSocket:     "/h/daemon.sock",
		ControlSocketFile: true,
		HTTPSocket:        "/h/daemon-http.sock",
		HTTPSocketFile:    true,
		PID:               42,
		PIDVerified:       true,
		AutostartUnit:     true,
		BinaryStale:       true,
	})
	got := out.String()
	for _, want := range []string{
		"daemon: running",
		"phase:          upgrade_probation",
		"version:        1.2.3",
		"boot id:        boot-123",
		"transaction:    transaction-123",
		"http listener:  bound",
		"tcp listener:   127.0.0.1:8443 (bound)",
		"control socket: /h/daemon.sock (present)",
		"http socket:    /h/daemon-http.sock (present)",
		"pid:            42 (verified)",
		"autostart:      installed",
		"warning:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("human output missing %q\n%s", want, got)
		}
	}
}

// A unit file on disk is not evidence that it owns the daemon which answered
// Ping. Before #2168 Phase 4 status printed only "autostart: installed", the
// exact reassurance shown during the incident while an ad-hoc daemon served.
func TestPrintDaemonStatusHumanInstalledUnitDoesNotImplySupervision(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running:       true,
		PID:           42,
		PIDVerified:   true,
		AutostartUnit: true,
	})

	got := out.String()
	require.Contains(t, got, "supervision:",
		"status must distinguish an installed unit from a unit proven to own the responder")
	require.Contains(t, got, "unknown",
		"an absent service-manager answer is unknown, never an implied supervised yes")
}

func TestCollectDaemonStatusCorrelatesResponderUnitAndConfig(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = true\n"), 0600))

	previousHealth := daemonHealthFn
	previousScope := autostartUnitServesHomeFn
	previousSupervision := daemonStatusSupervisionFn
	t.Cleanup(func() {
		daemonHealthFn = previousHealth
		autostartUnitServesHomeFn = previousScope
		daemonStatusSupervisionFn = previousSupervision
	})
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			ServingPID:    42,
			BootConfig:    &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: false},
			AutostartUnit: true,
		}
	}
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return true, true, nil }
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(), Active: daemon.AnswerYes(),
			MainPID: 42, MainPIDPresent: daemon.AnswerYes(),
		}
	}

	info := collectDaemonStatus()
	require.True(t, info.Running)
	require.Equal(t, "yes", info.Supervised)
	require.Equal(t, "no", info.ConfigMatches)
	require.Contains(t, info.ConfigDetail, "listen_addr")
	require.Contains(t, info.ConfigDetail, "require_token")
}

func TestCollectDaemonStatusDoesNotAttributeForeignHomeUnit(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	previousHealth := daemonHealthFn
	previousScope := autostartUnitServesHomeFn
	previousSupervision := daemonStatusSupervisionFn
	t.Cleanup(func() {
		daemonHealthFn = previousHealth
		autostartUnitServesHomeFn = previousScope
		daemonStatusSupervisionFn = previousSupervision
	})
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{ServingPID: 42, AutostartUnit: true}
	}
	autostartUnitServesHomeFn = func(got string) (bool, bool, error) {
		require.Equal(t, home, got)
		return false, true, nil
	}
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		t.Fatal("status must not query or attribute another home's service-manager unit")
		return daemon.SupervisionInfo{}
	}

	info := collectDaemonStatus()
	require.False(t, info.AutostartUnit)
	require.Empty(t, info.AutostartEnabled)
	require.Empty(t, info.AutostartActive)
	require.Zero(t, info.UnitPID)
	require.Equal(t, "no", info.Supervised, "this home has no unit supervising its responder")
}

func TestCollectDaemonStatusNoUnitOmitsInapplicableManagerState(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	previousHealth := daemonHealthFn
	previousScope := autostartUnitServesHomeFn
	previousSupervision := daemonStatusSupervisionFn
	t.Cleanup(func() {
		daemonHealthFn = previousHealth
		autostartUnitServesHomeFn = previousScope
		daemonStatusSupervisionFn = previousSupervision
	})
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{PingErr: errors.New("no daemon")}
	}
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return false, false, nil }
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		t.Fatal("no installed unit means there is no service-manager state to query")
		return daemon.SupervisionInfo{}
	}

	data, err := json.Marshal(collectDaemonStatus())
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Equal(t, false, wire["autostart_unit"])
	require.NotContains(t, wire, "autostart_enabled")
	require.NotContains(t, wire, "autostart_active")
	require.NotContains(t, wire, "unit_pid")
}

func TestCollectDaemonStatusUnitScopeFailureStaysUnknown(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	previousHealth := daemonHealthFn
	previousScope := autostartUnitServesHomeFn
	previousSupervision := daemonStatusSupervisionFn
	t.Cleanup(func() {
		daemonHealthFn = previousHealth
		autostartUnitServesHomeFn = previousScope
		daemonStatusSupervisionFn = previousSupervision
	})
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{ServingPID: 42, AutostartUnit: true}
	}
	autostartUnitServesHomeFn = func(string) (bool, bool, error) {
		return false, true, errors.New("unit file is unreadable")
	}
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		t.Fatal("an unscoped unit must not be attributed through the service manager")
		return daemon.SupervisionInfo{}
	}

	info := collectDaemonStatus()
	require.True(t, info.AutostartUnit, "the file is known to exist even though its home is unknown")
	require.Equal(t, "unknown", info.Supervised)
	require.Contains(t, info.SupervisionDetail, "unit file is unreadable")
	require.Empty(t, info.AutostartEnabled)
	require.Empty(t, info.AutostartActive)
}

func TestPrintDaemonStatusHumanNamesPIDMismatchAndStaleConfig(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running:          true,
		ServingPID:       42,
		AutostartUnit:    true,
		AutostartEnabled: "yes",
		AutostartActive:  "yes",
		UnitPID:          99,
		Supervised:       "no",
		ConfigMatches:    "no",
		ConfigDetail:     `listen_addr: running "0.0.0.0:8443", file "127.0.0.1:8443"`,
	})

	got := out.String()
	require.Contains(t, got, "responding daemon pid 42 is not supervised")
	require.Contains(t, got, "installed unit, which owns pid 99")
	require.Contains(t, got, "af daemon adopt",
		"a responder the unit does not own is exactly what adopt fixes")
	require.Contains(t, got, "config on disk differs from the running daemon")
	require.Contains(t, got, "restart the daemon to apply it")
}

func TestPrintDaemonStatusHumanNotRunning(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running:           false,
		ControlSocket:     "/h/daemon.sock",
		ControlSocketFile: false,
	})
	got := out.String()
	if !strings.Contains(got, "not running") {
		t.Errorf("expected not-running line, got:\n%s", got)
	}
	if !strings.Contains(got, "(absent)") {
		t.Errorf("expected absent socket label, got:\n%s", got)
	}
	if !strings.Contains(got, "no daemon.pid on disk") {
		t.Errorf("expected empty-pid line, got:\n%s", got)
	}
	if !strings.Contains(got, "af daemon install") {
		t.Errorf("expected autostart install hint, got:\n%s", got)
	}
}

// TestCollectDaemonStatusReportsExposureWithoutClaimingItCannotStart is the
// #2168 Phase 0 status surface, and it flips what #2090 asserted here.
//
// #2090 printed "not running and cannot start" on this config and suppressed the
// on-demand line. Both are now false: the daemon starts and serves. Keeping them
// would be a status command lying about a live capability — the same fabricated
// negative in the other direction. What survives is the exposure, reported as a
// warning alongside the ordinary on-demand wording.
func TestCollectDaemonStatusReportsExposureWithoutClaimingItCannotStart(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\n"), 0600))

	info := collectDaemonStatus()
	require.False(t, info.Running)
	require.NotEmpty(t, info.ExposureWarning, "an exposed listener must be reported, not implied")
	require.Contains(t, info.ExposureWarning, "require_token")
	require.Contains(t, info.ExposureWarning, "0.0.0.0:8443")

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printDaemonStatusHuman(cmd, info)
	got := out.String()
	require.Contains(t, got, "starts on demand",
		"the on-demand promise is true again — this config starts fine")
	require.NotContains(t, got, "cannot start",
		"there is no config the daemon refuses to start under any more")
	require.Contains(t, got, "warning:", "the exposure still has to reach the operator")
	require.Contains(t, got, "DeliverPrompt")
}

// TestCollectDaemonStatusSafeConfigIsUnwarned is the other direction: a user who
// simply has no daemon yet, on the shipped loopback default, sees no warning.
func TestCollectDaemonStatusSafeConfigIsUnwarned(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = false\n"), 0600))

	info := collectDaemonStatus()
	require.Empty(t, info.ExposureWarning)

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printDaemonStatusHuman(cmd, info)
	require.Contains(t, out.String(), "starts on demand")
	require.NotContains(t, out.String(), "warning:")
}

// TestCollectDaemonStatusAuthenticatedNetworkBindIsUnwarned pins that
// require_token = true is untouched: the recommended remote posture draws no
// warning, so the warning keeps meaning something when it does appear.
func TestCollectDaemonStatusAuthenticatedNetworkBindIsUnwarned(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = true\n"), 0600))

	require.Empty(t, collectDaemonStatus().ExposureWarning)
}
