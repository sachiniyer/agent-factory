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
	require.Contains(t, info.ConfigDetail, "network.listen_addr")
	require.Contains(t, info.ConfigDetail, "network.require_token")
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
		ConfigDetail:     `network.listen_addr: running "0.0.0.0:8443", file "127.0.0.1:8443"`,
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

// TestCollectDaemonStatusReportsRefusalWithoutClaimingItCannotStart is the
// #5137 status surface, and it flips what #2168 Phase 0 asserted here.
//
// The disk posture is a non-loopback listen_addr with the token off and no
// opt-in: the daemon STILL starts (the refusal is listener-scoped, so the
// on-demand line stays true), but ExposureWarning now carries the refusal
// reason — ListenerBindRefusal verbatim — not a warn-and-serve notice.
func TestCollectDaemonStatusReportsRefusalWithoutClaimingItCannotStart(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\n"), 0600))

	info := collectDaemonStatus()
	require.False(t, info.Running)
	require.NotEmpty(t, info.ExposureWarning, "a refused listener must be reported, not implied")
	require.Contains(t, info.ExposureWarning, "refused")
	require.Contains(t, info.ExposureWarning, "0.0.0.0:8443")
	for _, fix := range []string{
		"network.require_token true", "af token show",
		"network.listen_addr 127.0.0.1:8443", "network.allow_unauthenticated_network true",
	} {
		require.Contains(t, info.ExposureWarning, fix,
			"the status surface must carry the same three fixes as every other refusal surface")
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printDaemonStatusHuman(cmd, info)
	got := out.String()
	require.Contains(t, got, "starts on demand",
		"the on-demand promise is still true — the refusal is scoped to the TCP listener")
	require.NotContains(t, got, "cannot start",
		"there is still no config the daemon refuses to start under")
	require.Contains(t, got, "warning:", "the refusal still has to reach the operator")
	require.Contains(t, got, "DeliverPrompt")
}

// TestCollectDaemonStatusOptedInExposureWarns covers the opted-in half: with
// allow_unauthenticated_network = true the listener DOES bind and serve
// unauthenticated, so ExposureWarning carries the serving-exposure notice —
// "af serves" — and not the refusal.
func TestCollectDaemonStatusOptedInExposureWarns(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\nallow_unauthenticated_network = true\n"), 0600))

	info := collectDaemonStatus()
	require.NotEmpty(t, info.ExposureWarning, "a serving unauthenticated exposure is still reported")
	require.Contains(t, info.ExposureWarning, "0.0.0.0:8443")
	require.Contains(t, info.ExposureWarning, "af serves")
	require.NotContains(t, info.ExposureWarning, "refused",
		"an opted-in listener is serving, not refused — the notice must not conflate them")
}

// TestPrintDaemonStatusHumanRefusedListener pins the lifecycle rendering: a
// configured-but-refused TCP listener prints "(refused)" with the shared reason,
// not "(not bound)" — the refused state is a decision, and a bare not-bound
// would send the operator debugging a port that was never attempted.
func TestPrintDaemonStatusHumanRefusedListener(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running: true,
		Listeners: &daemon.DaemonListenerStatus{
			HTTPUnixBound:     true,
			TCPConfigured:     true,
			TCPListenAddr:     "0.0.0.0:8443",
			TCPBound:          false,
			TCPRefusalReason:  "network.listen_addr \"0.0.0.0:8443\" is reachable from the network … refused",
			PreviewConfigured: false,
		},
		ControlSocket:     "/h/daemon.sock",
		ControlSocketFile: true,
	})

	got := out.String()
	require.Contains(t, got, "tcp listener:   0.0.0.0:8443 (refused)",
		"a refused listener is a decision — the line must say so")
	require.Contains(t, got, "refused:      network.listen_addr",
		"the refusal reason the lifecycle recorded must reach the operator")
	require.NotContains(t, got, "(not bound)",
		"a refused listener is not 'not bound' — that phrase belongs to a bind that was attempted")
}

// TestListenerStatusWarningBoundUnderRefusedPosture pins the version-skew
// surface: a daemon still BOUND while the disk posture is refused is running
// code from before the #5137 refusal (a current daemon retires the socket on
// apply), so it is still serving the API unauthenticated. The refusal text
// would be a false "safe" claim — the warning must name the serving socket
// and the restart that enforces the policy instead.
func TestListenerStatusWarningBoundUnderRefusedPosture(t *testing.T) {
	refused := config.DefaultConfig()
	refused.ListenAddr = "0.0.0.0:8443"
	refused.RequireToken = false

	// The still-serving case: Ping reports the socket bound, so the disk
	// refusal is describing a policy this daemon build does not have. A nil
	// BootConfig stands in for a responder that predates the field entirely —
	// which also predates the refusal, so the bound socket IS the exposure.
	bound := &daemon.DaemonListenerStatus{
		TCPConfigured: true, TCPListenAddr: "0.0.0.0:8443",
		TCPBound: true, TCPBoundAddr: "0.0.0.0:8443",
	}
	warn := listenerStatusWarning(refused, nil, bound)
	for _, want := range []string{"still serving", "0.0.0.0:8443", "af daemon restart", "DeliverPrompt"} {
		require.Contains(t, warn, want,
			"a bound socket under a refused posture must be reported as the live exposure it is")
	}
	require.NotContains(t, warn, "the TCP listener is refused",
		"the refusal text is a false safe claim while the socket is still bound")

	// A daemon that DOES carry BootConfig gets its live posture consulted: a
	// bound socket the live config still serves unauthenticated is the same
	// exposure; a bound socket the live config keeps safe (token on, or a
	// loopback addr the disk edit abandoned) is restart-pending drift, not an
	// unauthenticated claim.
	liveExposed := &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: false}
	require.Contains(t, listenerStatusWarning(refused, liveExposed, bound), "still serving 0.0.0.0:8443 unauthenticated",
		"a live posture that is itself unauthenticated confirms the exposure")
	liveSafe := &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: true}
	drift := listenerStatusWarning(refused, liveSafe, bound)
	require.Contains(t, drift, "socket is safe",
		"a bound socket the live config still authenticates is drift, not an unauthenticated exposure")
	require.NotContains(t, drift, "unauthenticated",
		"claiming unauthenticated against a tokened socket would be a false exposure claim")
	require.Contains(t, drift, "af daemon restart")

	// The retained-socket divergence (#5137): a failed rebind leaves the OLD
	// network socket answering while the live config already names loopback —
	// classifying boot.ListenAddr would read that live network socket as safe.
	// The bound address, not the configured one, is what the exposure is.
	liveConfigMoved := &daemon.DaemonBootConfig{ListenAddr: "127.0.0.1:9999", RequireToken: false}
	require.Contains(t, listenerStatusWarning(refused, liveConfigMoved, bound), "still serving 0.0.0.0:8443 unauthenticated",
		"a socket still bound on the network address is exposed no matter what the live config names")

	// And the converse: the retained socket moved to loopback while the live
	// config still names the network address — the loopback socket is safe, so
	// drift, not exposure.
	loopbackBound := &daemon.DaemonListenerStatus{
		TCPConfigured: true, TCPListenAddr: "0.0.0.0:8443",
		TCPBound: true, TCPBoundAddr: "127.0.0.1:8443",
	}
	liveNetwork := &daemon.DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: false}
	require.Contains(t, listenerStatusWarning(refused, liveNetwork, loopbackBound), "socket is safe",
		"a socket bound on loopback is not network-reachable even if the config names a network addr")

	// The refused case: nothing bound — the refusal itself is the report.
	notBound := &daemon.DaemonListenerStatus{
		TCPConfigured: true, TCPListenAddr: "0.0.0.0:8443",
		TCPBound: false, TCPRefusalReason: "…",
	}
	require.Equal(t, config.ListenerBindRefusal(refused), listenerStatusWarning(refused, nil, notBound))
	require.Equal(t, config.ListenerBindRefusal(refused), listenerStatusWarning(refused, nil, nil),
		"no running daemon reports no bound socket — the disk refusal stands")

	// The opted-in case stays the serving notice (unchanged behavior).
	opted := *refused
	opted.AllowUnauthenticatedNetwork = true
	require.Equal(t, config.ListenerExposureNotice(&opted), listenerStatusWarning(&opted, nil, bound))
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
