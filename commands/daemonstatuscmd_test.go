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

// TestCollectDaemonStatusReadFailureShapeKeepsUnit pins the read-failure shape
// the real AutostartUnitServesHome produces when the unit file path is present
// per stat but not readable as a file: a directory at the path (EISDIR), a
// chmod 0000 file on a box where af is not root, or a broken mount that answers
// stat but not read. In that shape os.Stat succeeds so h.AutostartUnit is true,
// while os.ReadFile fails with a non-IsNotExist error, so AutostartUnitServesHome
// returns (false, false, err) — installed=false on a present file.
//
// collectDaemonStatus's scope-unknown fallback must keep AutostartUnit=true on
// the stat-based h.AutostartUnit, not downgrade it to false on the read-based
// installed return. Without that, status prints "autostart: no unit for this
// home" next to "supervision: unknown (cannot tell whether the installed unit
// serves this home)" — a self-contradiction, and a JSON autostart_unit=false
// that scripts/lifecycle.sh (lines 484, 653) gates upgrade assertions on.
//
// Contrast TestCollectDaemonStatusUnitScopeFailureStaysUnknown, which stubs the
// parse-failure shape (installed=true) that AutostartUnitServesHome returns only
// AFTER a successful read; this test stubs the pre-read shape that masks the bug.
func TestCollectDaemonStatusReadFailureShapeKeepsUnit(t *testing.T) {
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
	// Stat-based existence is true: the file is present, just unreadable.
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{ServingPID: 42, AutostartUnit: true}
	}
	// The real read-failure shape, proven by the daemon-package reproduction: a
	// present file that cannot be read returns (false, false, err) — installed
	// is false BEFORE the parser ever runs, unlike the post-read parse-failure
	// shape that returns installed=true.
	autostartUnitServesHomeFn = func(string) (bool, bool, error) {
		return false, false, errors.New("failed to read the autostart unit /h/agent-factory-daemon.service: read /h/agent-factory-daemon.service: is a directory")
	}
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		t.Fatal("an unscoped unit must not be attributed through the service manager")
		return daemon.SupervisionInfo{}
	}

	info := collectDaemonStatus()
	require.True(t, info.AutostartUnit,
		"the unit file is stat-present even though its home scope is unreadable; AutostartUnit must stay true")
	require.Equal(t, "unknown", info.Supervised,
		"an unreadable unit cannot be proven to own the responder, so supervision is unknown")
	require.Contains(t, info.SupervisionDetail, "is a directory",
		"the read-failure cause must reach the operator as the supervision detail")
	require.Empty(t, info.AutostartEnabled, "no service-manager state is queried for an unscoped unit")
	require.Empty(t, info.AutostartActive)

	// The human report must not contradict itself: it must not claim "no unit
	// for this home" while the supervision line says it cannot tell whether the
	// INSTALLED unit serves this home.
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printDaemonStatusHuman(cmd, info)
	got := out.String()
	require.Contains(t, got, "autostart:      installed",
		"a stat-present unit must be reported as installed, not absent")
	require.NotContains(t, got, "no unit for this home",
		"the unit file exists by stat; status must not claim it is absent")
	require.Contains(t, got, "supervision:    unknown")
	require.Contains(t, got, "is a directory")
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

// TestPrintDaemonStatusHumanMarksUnverifiablePID pins the third pid-file
// verdict on the status surface: a pid naming a live af daemon whose home
// could not be bound is inconclusive — not verified (a kill hint could name
// another home's daemon), and not "unverified" stale (the file may name this
// home's own live daemon on platforms that cannot read a peer's frame).
func TestPrintDaemonStatusHumanMarksUnverifiablePID(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running:           false,
		ControlSocket:     "/h/daemon.sock",
		ControlSocketFile: false,
		PID:               4242,
		PIDUnverifiable:   true,
	})

	got := out.String()
	require.Contains(t, got, "pid:            4242 (live af daemon, home unproven)")
	require.NotContains(t, got, "(unverified)",
		"a live af daemon with an unproven home is inconclusive, not stale-unverified")
	require.NotContains(t, got, "(verified)")
}
