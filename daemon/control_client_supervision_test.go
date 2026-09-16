package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These tests exercise the ordinary EnsureDaemon production gate with a real
// fake systemctl binary. The AF home, unit file, socket, and manager process are
// all private to the testbox; no host daemon or service manager is touched.

func TestRunEnsureManagerCommand_DeadlineDuringWaitDelayStillSucceeds(t *testing.T) {
	manager := filepath.Join(t.TempDir(), "manager")
	if err := os.WriteFile(manager, []byte("#!/bin/sh\nsleep 30 >&1 2>&1 &\n"), 0o700); err != nil {
		t.Fatalf("write fake manager: %v", err)
	}

	// The shell exits immediately; the child explicitly retains the capture
	// pipes. This gives Wait ample time to observe the completed command before
	// the deadline lands inside the 250ms WaitDelay cleanup window.
	if err := runEnsureManagerCommand(time.Now().Add(200*time.Millisecond), manager, "start"); err != nil {
		t.Fatalf("the manager exited zero before the deadline; only pipe cleanup crossed it: %v", err)
	}
}

func TestEnsureDaemonPrefersHomeServingUnit(t *testing.T) {
	marker, home := installEnsureTestUnitAndManager(t, false)
	pidPath := filepath.Join(home, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte("0"), 0o600); err != nil {
		t.Fatalf("write harmless stale PID marker: %v", err)
	}
	startServer, serverErr := ensureTestServerStarter(t)
	stopWatcher := startServerWhenMarked(marker, startServer)
	defer stopWatcher()

	adHocLaunched := false
	err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	})
	if err != nil {
		t.Fatalf("ensureDaemonWithLauncher: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start fake supervised daemon: %v", err)
	}
	if adHocLaunched {
		t.Fatal("home-serving unit was ignored and an ad-hoc daemon was launched")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("unit-owned cold start ran the ad-hoc StopDaemon path: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("bounded systemctl start was not invoked: %v", err)
	}
	if _, err := os.Stat(marker + ".unexpected-reset"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("implicit cold start reset the crash-loop backstop; marker stat = %v", err)
	}
}

func TestEnsureDaemonPrefersHomeServingLaunchdUnit(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	unitDir := withAutostartTestEnv(t, "darwin")
	plist := launchdAutostartPlist("/opt/agent-factory/bin/af", "", "", home, filepath.Join(home, "daemon.log"))
	if err := os.WriteFile(filepath.Join(unitDir, autostartLaunchdLabel+".plist"), []byte(plist), 0o600); err != nil {
		t.Fatalf("write home-serving launchd unit: %v", err)
	}

	managerDir := t.TempDir()
	marker := filepath.Join(managerDir, "kickstart-called")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"kickstart\" ] || [ \"$2\" != \"-k\" ] || [ \"$3\" != \"" + launchdServiceTarget() + "\" ]; then\n" +
		"  exit 64\n" +
		"fi\n" +
		"printf 'called\\n' > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(managerDir, "launchctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake launchctl: %v", err)
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	startServer, serverErr := ensureTestServerStarter(t)
	stopWatcher := startServerWhenMarked(marker, startServer)
	defer stopWatcher()
	adHocLaunched := false
	if err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	}); err != nil {
		t.Fatalf("ensureDaemonWithLauncher: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start fake launchd daemon: %v", err)
	}
	if adHocLaunched {
		t.Fatal("home-serving launchd unit was ignored and an ad-hoc daemon was launched")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("bounded launchctl kickstart was not invoked: %v", err)
	}
}

// TestEnsureDaemonManagerHangRefusesAdHocSpawn: a wedged service manager on a
// unit-claimed home must NOT fall back to an ad-hoc daemon (#4470). A timed-out
// `systemctl --user start` is ambiguous — the start may already be queued with
// ExecStart pending — and any ad-hoc spawn in that state becomes the permanent
// unsupervised escapee that keeps the unit inactive. EnsureDaemon refuses with
// an actionable error within the bounded manager slice instead.
func TestEnsureDaemonManagerHangRefusesAdHocSpawn(t *testing.T) {
	marker, _ := installEnsureTestUnitAndManager(t, true)
	// A session-bus address is configured, so the wedge is a manager hang,
	// not a missing bus — the adopt remedy must lead.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	startServer, _ := ensureTestServerStarter(t)

	adHocLaunched := false
	started := time.Now()
	err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a wedged manager on a unit-claimed home produced an ad-hoc daemon — the #4470 escape")
	}
	if !strings.Contains(err.Error(), "unsupervised") || !strings.Contains(err.Error(), "af daemon adopt") {
		t.Fatalf("refusal must name the supervision problem and the remedy, got: %v", err)
	}
	if adHocLaunched {
		t.Fatal("wedged manager spawned an unsupervised daemon")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the manager hang was not actually exercised: %v", err)
	}
	if elapsed >= daemonReadyTimeout {
		t.Fatalf("refusal took %s, want the bounded %s manager slice", elapsed, ensureUnitStartTimeout)
	}
}

// TestEnsureDaemonPendingUnitStartNeverSpawnsAdHoc is the #4470 incident shape:
// the manager ACCEPTS `systemctl --user start` while the unit's ExecStart is
// still pending (RestartSec after an on-failure kill), so no socket serves
// inside the bounded start slice. The fallback used to fire there and win the
// socket race against the pending ExecStart — the unsupervised daemon that
// left the unit inactive for hours. EnsureDaemon must wait out the whole
// readiness budget and let the supervised daemon answer instead.
func TestEnsureDaemonPendingUnitStartNeverSpawnsAdHoc(t *testing.T) {
	marker, _ := installEnsureTestUnitAndManager(t, false)
	startServer, serverErr := ensureTestServerStarter(t)

	// The unit's daemon answers only after the old bounded start slice would
	// have expired — the ExecStart-pending window the fallback used to lose.
	stopWatcher := startServerWhenMarked(marker, func() error {
		time.Sleep(ensureUnitStartTimeout + 500*time.Millisecond)
		return startServer()
	})
	defer stopWatcher()

	adHocLaunched := false
	if err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	}); err != nil {
		t.Fatalf("a slow supervised start must return nil once the unit daemon answers, got: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start delayed supervised daemon: %v", err)
	}
	if adHocLaunched {
		t.Fatal("a pending ExecStart was raced by an ad-hoc spawn — the #4470 escape")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("bounded systemctl start was not invoked: %v", err)
	}
}

// TestEnsureDaemonUnitStartRefusedDoesNotSpawn: a manager that answers but
// refuses the start — masked unit, start-limit hit — must not produce an
// ad-hoc daemon either: the unit still owns this home, and the escapee would
// outlive the transient refusal (#4470). (A caller with no session bus at all
// is the bus-unreachable class — covered below.)
func TestEnsureDaemonUnitStartRefusedDoesNotSpawn(t *testing.T) {
	_, _ = installEnsureTestUnitAndManager(t, false)
	// A session-bus address is configured, so the refusal is a manager answer
	// (masked unit, start-limit), not a bus reachability failure — the adopt
	// remedy must lead.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	// Replace the accepting fake with one that refuses the start outright.
	managerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write refusing systemctl: %v", err)
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	startServer, _ := ensureTestServerStarter(t)
	adHocLaunched := false
	err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	})
	if err == nil {
		t.Fatal("a refused start produced an ad-hoc daemon — the #4470 escape")
	}
	if !strings.Contains(err.Error(), "af daemon adopt") {
		t.Fatalf("refusal must name the adopt remedy, got: %v", err)
	}
	if adHocLaunched {
		t.Fatal("refused start spawned an unsupervised daemon")
	}
}

// TestEnsureDaemonSupervisorAbsentFallsBackToAdHoc: where the unit's manager
// provably cannot exist in this environment — no systemd as init — the ad-hoc
// fallback remains the only way af runs, and there is no supervision for it
// to escape (#2373's constraint, preserved under #4470). The fallback daemon
// is reachable and the manager binary is never invoked.
func TestEnsureDaemonSupervisorAbsentFallsBackToAdHoc(t *testing.T) {
	marker, _ := installEnsureTestUnitAndManager(t, false)
	systemdBootedDir = filepath.Join(t.TempDir(), "no-such-dir")
	startServer, serverErr := ensureTestServerStarter(t)

	warnBuf := captureWarnings(t)

	adHocLaunched := false
	err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	})
	if err != nil {
		t.Fatalf("supervisor-absent fallback must return a reachable daemon: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start fake ad-hoc daemon: %v", err)
	}
	if !adHocLaunched {
		t.Fatal("a host with no supervisor skipped the only launch path that can work")
	}
	if err := pingDaemon(); err != nil {
		t.Fatalf("fallback returned nil but the daemon is not reachable: %v", err)
	}
	if !strings.Contains(warnBuf.String(), "falling back to an ad-hoc daemon") {
		t.Fatalf("fallback did not warn about the supervision downgrade; log = %q", warnBuf.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a provably-absent supervisor was still invoked; marker stat = %v", err)
	}
}

// TestEnsureDaemonNoManagerBinaryFallsBackToAdHoc covers the second absence
// arm: the unit file claims the home but the host has neither systemd as
// init nor a manager binary on PATH — a minimal image where the unit can
// never run. The ad-hoc fallback is again the only working path.
func TestEnsureDaemonNoManagerBinaryFallsBackToAdHoc(t *testing.T) {
	_, _ = installEnsureTestUnitAndManager(t, false)
	// An empty PATH makes the manager binary unresolvable without affecting
	// the in-process fake daemon, which needs no external tools. The booted
	// marker must ALSO be missing: on a systemd-booted host a PATH miss is an
	// invocation failure that fails closed, not absence (the next test).
	systemdBootedDir = filepath.Join(t.TempDir(), "no-such-dir")
	t.Setenv("PATH", t.TempDir())
	startServer, serverErr := ensureTestServerStarter(t)

	adHocLaunched := false
	err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	})
	if err != nil {
		t.Fatalf("no-manager fallback must return a reachable daemon: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start fake ad-hoc daemon: %v", err)
	}
	if !adHocLaunched {
		t.Fatal("a host with no manager binary skipped the only launch path that can work")
	}
}

// TestEnsureDaemonUnreachableSupervisorRefusesAdHoc is the Codex-finding
// shape: on a systemd-BOOTED host, an `af` invoked by absolute path from an
// env whose PATH omits systemctl cannot invoke the manager — but the
// supervisor provably exists. Reading that PATH miss as absence would ad-hoc
// spawn the exact unsupervised escapee this gate exists to refuse (#4470), so
// the probe must fail closed instead.
func TestEnsureDaemonUnreachableSupervisorRefusesAdHoc(t *testing.T) {
	_, _ = installEnsureTestUnitAndManager(t, false) // booted marker present
	t.Setenv("PATH", t.TempDir())                    // no systemctl anywhere
	startServer, _ := ensureTestServerStarter(t)

	adHocLaunched := false
	err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	})
	if err == nil {
		t.Fatal("a PATH-omitted manager binary on a booted host produced an ad-hoc daemon — the #4470 escape")
	}
	if !strings.Contains(err.Error(), "cannot be invoked") || !strings.Contains(err.Error(), "session with a service manager") {
		t.Fatalf("refusal must name the invocation failure and the session remedy, got: %v", err)
	}
	if adHocLaunched {
		t.Fatal("unreachable supervisor spawned an unsupervised daemon")
	}
}

// TestEnsureDaemonUnitStartRefusalOrdersRemedyByBusClass: the refusal's remedy
// order depends on WHY the start failed. Where the manager could not be
// reached at all — no session bus configured (a user cron job, a system
// service), or a configured bus that refused the connect — `af daemon adopt`
// would drive the same manager through the same bus and fail identically, so
// the session remedy leads and adopt is only named to say it cannot help. A
// refusal under a reachable manager keeps adopt first.
func TestEnsureDaemonUnitStartRefusalOrdersRemedyByBusClass(t *testing.T) {
	for _, tc := range []struct {
		name          string
		busConfigured bool
		managerStderr string
		wantAdoptLead bool
	}{
		{name: "no session bus (cron)", busConfigured: false, wantAdoptLead: false},
		{
			name:          "bus configured, connect failed",
			busConfigured: true,
			managerStderr: "Failed to connect to bus: No such file or directory",
			wantAdoptLead: false,
		},
		{name: "refused under reachable manager", busConfigured: true, wantAdoptLead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _ = installEnsureTestUnitAndManager(t, false)
			if tc.busConfigured {
				t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			} else {
				t.Setenv("XDG_RUNTIME_DIR", "")
				t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
			}
			managerDir := t.TempDir()
			script := "#!/bin/sh\n"
			if tc.managerStderr != "" {
				script += "echo " + shellQuote(tc.managerStderr) + " >&2\n"
			}
			script += "exit 1\n"
			if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(script), 0o700); err != nil {
				t.Fatalf("write fake systemctl: %v", err)
			}
			t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			startServer, _ := ensureTestServerStarter(t)
			adHocLaunched := false
			err := ensureDaemonWithLauncher(func() error {
				adHocLaunched = true
				return startServer()
			})
			if err == nil {
				t.Fatal("a refused start produced an ad-hoc daemon — the #4470 escape")
			}
			if adHocLaunched {
				t.Fatal("refused start spawned an unsupervised daemon")
			}
			if tc.wantAdoptLead {
				if !strings.Contains(err.Error(), "run `af daemon adopt`") {
					t.Fatalf("reachable-manager refusal must lead with adopt, got: %v", err)
				}
			} else {
				if !strings.Contains(err.Error(), "start the unit from a session with a service manager") {
					t.Fatalf("bus-unreachable refusal must lead with the session remedy, got: %v", err)
				}
				if strings.Contains(err.Error(), "run `af daemon adopt`,") {
					t.Fatalf("adopt cannot help in the bus-unreachable class and must not lead, got: %v", err)
				}
			}
		})
	}
}

// TestProbeUnitSupervisorOrdering pins the probe's three states and its check
// order: the boot marker is consulted before PATH, so "not booted" reads
// absent even when the binary exists (a container shipping systemctl), while
// "booted but binary missing" is an invocation failure that fails closed —
// never absence. On darwin launchd is always PID 1, so a missing binary is
// always unreachable.
func TestProbeUnitSupervisorOrdering(t *testing.T) {
	binaryDir := t.TempDir()
	for _, name := range []string{"systemctl", "launchctl"} {
		if err := os.WriteFile(filepath.Join(binaryDir, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	for _, tc := range []struct {
		name   string
		goos   string
		booted bool // linux only: the sd_booted marker exists
		binary bool // the manager client binary is on PATH
		want   supervisorPresence
	}{
		{"linux booted + binary", "linux", true, true, supervisorPresent},
		{"linux booted, binary missing", "linux", true, false, supervisorUnreachable},
		{"linux not booted + binary", "linux", false, true, supervisorAbsent},
		{"linux not booted, no binary", "linux", false, false, supervisorAbsent},
		{"darwin binary", "darwin", false, true, supervisorPresent},
		{"darwin binary missing", "darwin", false, false, supervisorUnreachable},
		{"unsupported platform", "plan9", false, false, supervisorAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAutostartTestEnv(t, tc.goos)
			prevBooted := systemdBootedDir
			t.Cleanup(func() { systemdBootedDir = prevBooted })
			if tc.booted {
				systemdBootedDir = t.TempDir()
			} else {
				systemdBootedDir = filepath.Join(t.TempDir(), "no-such-dir")
			}
			if tc.binary {
				t.Setenv("PATH", binaryDir)
			} else {
				t.Setenv("PATH", t.TempDir())
			}
			got, err := probeUnitSupervisor()
			if got != tc.want {
				t.Fatalf("probeUnitSupervisor = %v (err %v), want %v", got, err, tc.want)
			}
		})
	}
}

// TestCallDaemonSurfacesUnitStartRefusal drives a real caller (callDaemon)
// through a wedged manager on a unit-claimed home: the RPC layer must surface
// the refusal instead of silently producing the unsupervised daemon #4470
// removed. The ad-hoc launcher is never invoked.
func TestCallDaemonSurfacesUnitStartRefusal(t *testing.T) {
	marker, _ := installEnsureTestUnitAndManager(t, true)
	startServer, _ := ensureTestServerStarter(t)

	prevLaunch := launchDaemonProcessFn
	adHocLaunched := false
	launchDaemonProcessFn = func() error { adHocLaunched = true; return startServer() }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	var resp PingResponse
	err := callDaemon("Ping", PingRequest{}, &resp)
	if err == nil {
		t.Fatal("a wedged manager on a unit-claimed home surfaced a silent ad-hoc daemon — the #4470 escape")
	}
	if !strings.Contains(err.Error(), "unsupervised") {
		t.Fatalf("refusal must name the supervision problem, got: %v", err)
	}
	if adHocLaunched {
		t.Fatal("wedged manager spawned an unsupervised daemon")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the manager hang was not exercised: %v", err)
	}
}

func TestEnsureDaemonFromPathBypassesUnitPreference(t *testing.T) {
	marker, _ := installEnsureTestUnitAndManager(t, false)
	startServer, serverErr := ensureTestServerStarter(t)

	const upgradedPath = "/opt/agent-factory/new/af"
	prev := launchDaemonProcessAtFn
	t.Cleanup(func() { launchDaemonProcessAtFn = prev })
	launchedPath := ""
	launchDaemonProcessAtFn = func(path string) error {
		launchedPath = path
		return startServer()
	}

	if err := EnsureDaemonFromPath(upgradedPath); err != nil {
		t.Fatalf("EnsureDaemonFromPath: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start explicit upgraded daemon: %v", err)
	}
	if launchedPath != upgradedPath {
		t.Fatalf("explicit launcher path = %q, want %q", launchedPath, upgradedPath)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EnsureDaemonFromPath consulted the installed unit; marker stat = %v", err)
	}
}

func TestEnsureDaemonForeignAbsentOrUnknownUnitKeepsAdHocPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(t *testing.T, unitDir, home string)
	}{
		{name: "absent"},
		{
			name: "foreign home",
			configure: func(t *testing.T, unitDir, _ string) {
				t.Helper()
				unit := systemdAutostartUnit("/opt/agent-factory/bin/af", "", "", t.TempDir())
				if err := os.WriteFile(filepath.Join(unitDir, autostartUnitName), []byte(unit), 0o600); err != nil {
					t.Fatalf("write foreign unit: %v", err)
				}
			},
		},
		{
			name: "unknown",
			configure: func(t *testing.T, _ string, _ string) {
				t.Helper()
				autostartSystemdUserDir = func() (string, error) {
					return "", errors.New("unit directory unavailable")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := testguard.SocketTempDir(t)
			t.Setenv("AGENT_FACTORY_HOME", home)
			unitDir := withAutostartTestEnv(t, "linux")
			if tc.configure != nil {
				tc.configure(t, unitDir, home)
			}

			managerDir := t.TempDir()
			marker := filepath.Join(managerDir, "unexpected-manager-call")
			script := "#!/bin/sh\nprintf 'called\\n' > " + shellQuote(marker) + "\n"
			if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(script), 0o700); err != nil {
				t.Fatalf("write fake systemctl: %v", err)
			}
			t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			startServer, serverErr := ensureTestServerStarter(t)
			adHocLaunched := false
			if err := ensureDaemonWithLauncher(func() error {
				adHocLaunched = true
				return startServer()
			}); err != nil {
				t.Fatalf("ensureDaemonWithLauncher: %v", err)
			}
			if err := serverErr(); err != nil {
				t.Fatalf("start fake ad-hoc daemon: %v", err)
			}
			if !adHocLaunched {
				t.Fatal("non-owning unit displaced the existing ad-hoc launch policy")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("non-owning unit invoked the service manager; marker stat = %v", err)
			}
		})
	}
}

func TestEnsureDaemonHealthySkipsSupervisionOwnership(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	withAutostartTestEnv(t, "linux")
	autostartSystemdUserDir = func() (string, error) {
		return "", errors.New("ownership probe must stay off the healthy path")
	}
	startTestControlServer(t)

	launched := false
	if err := ensureDaemonWithLauncher(func() error {
		launched = true
		return nil
	}); err != nil {
		t.Fatalf("healthy ensure consulted broken ownership state: %v", err)
	}
	if launched {
		t.Fatal("healthy ensure launched another daemon")
	}
}

func installEnsureTestUnitAndManager(t *testing.T, block bool) (string, string) {
	t.Helper()
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	unitDir := withAutostartTestEnv(t, "linux")
	// A testbox/container has no real /run/systemd/system, so point the
	// supervisor-presence marker at a directory that exists: the fake
	// environment claims a live supervisor, and tests that want "absent"
	// repoint it at a missing path.
	prevBooted := systemdBootedDir
	t.Cleanup(func() { systemdBootedDir = prevBooted })
	systemdBootedDir = t.TempDir()
	unit := systemdAutostartUnit("/opt/agent-factory/bin/af", "", "", home)
	if err := os.WriteFile(filepath.Join(unitDir, autostartUnitName), []byte(unit), 0o600); err != nil {
		t.Fatalf("write home-serving unit: %v", err)
	}

	managerDir := t.TempDir()
	marker := filepath.Join(managerDir, "start-called")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--user\" ] && [ \"$2\" = \"reset-failed\" ] && [ \"$3\" = \"" + autostartUnitName + "\" ]; then\n" +
		"  printf 'reset\\n' > " + shellQuote(marker+".unexpected-reset") + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" != \"--user\" ] || [ \"$2\" != \"start\" ] || [ \"$3\" != \"" + autostartUnitName + "\" ]; then\n" +
		"  exit 64\n" +
		"fi\n" +
		"printf 'called\\n' > " + shellQuote(marker) + "\n"
	if block {
		// The child keeps the output pipe open too. A direct-child-only timeout
		// therefore hangs unless the production runner owns and kills the group.
		script += "sleep 300 &\nwait\n"
	}
	if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker, home
}

func ensureTestServerStarter(t *testing.T) (func() error, func() error) {
	t.Helper()
	var mu sync.Mutex
	var closeServer func() error
	var startErr error
	start := func() error {
		mu.Lock()
		defer mu.Unlock()
		if closeServer == nil && startErr == nil {
			closeServer, startErr = startControlServer(nil, nil, nil, nil)
		}
		return startErr
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if closeServer != nil {
			_ = closeServer()
		}
	})
	return start, func() error {
		mu.Lock()
		defer mu.Unlock()
		return startErr
	}
}

func startServerWhenMarked(marker string, startServer func() error) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(marker); err == nil {
				_ = startServer()
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				return
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
