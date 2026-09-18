package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
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
	// not a missing bus. A start that outlives its bound is most often queued
	// behind the unit's RestartSec holdoff, which adopt's restart skips, so
	// adopt leads; the session remedy follows for a manager that hangs adopt
	// too (#4475 review).
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
	if !strings.Contains(err.Error(), "unsupervised") {
		t.Fatalf("a manager timeout must name the supervision problem, got: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a manager timeout must keep its deadline identity, got: %v", err)
	}
	assertRemedyOrder(t, renderedRemedies(t, err), remedyAdoptLead, remedyAdoptHangsToo, uninstallRemedy)
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
	assertRemedyOrder(t, renderedRemedies(t, err), remedyAdoptLead, remedyAdoptFailsSame, uninstallRemedy)
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
	if !strings.Contains(err.Error(), "cannot be invoked") {
		t.Fatalf("refusal must name the invocation failure, got: %v", err)
	}
	// adopt runs the same binary from the same PATH, so it is never named;
	// the PATH repair leads, and the directory literal is pinned so the
	// (usually …) hint stays /usr/bin (systemctl ships there).
	assertRemedyOrder(t, renderedRemedies(t, err), remedyPathLead+"systemctl` (usually /usr/bin)", remedySessionLead, uninstallRemedy)
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
// refusal under a reachable manager keeps adopt first. Each row pins the full
// rendered order, not the mere presence of a phrase.
func TestEnsureDaemonUnitStartRefusalOrdersRemedyByBusClass(t *testing.T) {
	busOrder := []string{remedySessionLead, remedyAdoptWontHelp, uninstallRemedy}
	refusedOrder := []string{remedyAdoptLead, remedyAdoptFailsSame, uninstallRemedy}
	for _, tc := range []struct {
		name          string
		busConfigured bool
		managerStderr string
		privSock      bool // a live <runtime>/systemd/private listener exists
		want          []string
	}{
		{name: "no session bus (cron)", busConfigured: false, want: busOrder},
		{
			name:          "bus configured, connect failed",
			busConfigured: true,
			managerStderr: "Failed to connect to bus: No such file or directory",
			want:          busOrder,
		},
		// The shape measured on a lingering host: the user manager is live on
		// /run/user/<uid>/systemd/private, but a cron job's env names no
		// runtime dir, so systemctl never finds that socket and says so.
		{
			name:          "cron on a lingering host: live private socket, bus env empty, connect failed",
			busConfigured: false,
			privSock:      true,
			managerStderr: "Failed to connect to bus: No medium found",
			want:          busOrder,
		},
		{name: "refused under reachable manager", busConfigured: true, want: refusedOrder},
		// A foreign-init host legitimately has BOTH bus variables unset while
		// the manager answers on <runtime>/systemd/private — a refused start
		// there is a manager answer, not a missing bus, and adopt's
		// reset-failed + restart reaches it through the same socket (Codex
		// on #4475).
		{name: "private socket live, bus env empty", busConfigured: false, privSock: true, want: refusedOrder},
		// A masked unit is a durable admin override: adopt's reset-failed +
		// restart cannot lift it, so the remedy must name unmask (#4475
		// review).
		{
			name:          "masked unit",
			busConfigured: true,
			managerStderr: "Failed to start af-daemon.service: Unit af-daemon.service is masked.",
			want:          []string{remedyUnmaskLead, maskedUnmanagedRemedy},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _ = installEnsureTestUnitAndManager(t, false)
			if tc.busConfigured {
				t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			} else {
				t.Setenv("XDG_RUNTIME_DIR", "")
				t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
			}
			if tc.privSock {
				userDir := filepath.Join(systemdUserBusBase, strconv.Itoa(os.Getuid()))
				if err := os.MkdirAll(filepath.Join(userDir, "systemd"), 0o700); err != nil {
					t.Fatalf("mkdir private socket dir: %v", err)
				}
				ln, err := net.Listen("unix", filepath.Join(userDir, "systemd", "private"))
				if err != nil {
					t.Fatalf("fake private manager socket: %v", err)
				}
				t.Cleanup(func() { ln.Close() })
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
			assertRemedyOrder(t, renderedRemedies(t, err), tc.want...)
		})
	}
}

// TestEnsureDaemonActiveUnitUnreachableReclaimsThroughManager is the
// alive-but-unreachable case (#4475 review): the unit's daemon process is
// alive per the manager, but its control socket is dead — `systemctl --user
// start` is a no-op on an already-active unit, so without a reclaim every
// command burns the readiness budget and fails identically forever. Ensure
// must detect the active unit and restart it through the manager — the same
// teardown `af daemon adopt` performs explicitly.
func TestEnsureDaemonActiveUnitUnreachableReclaimsThroughManager(t *testing.T) {
	_, _ = installEnsureTestUnitAndManager(t, false)

	// Replace the accepting fake with one that models the wedge: `start`
	// exits 0 without serving (the unit is already active), `is-active
	// --quiet` reports the unit active, and only `restart` produces a
	// serving daemon.
	managerDir := t.TempDir()
	startMarker := filepath.Join(managerDir, "start-called")
	restartMarker := filepath.Join(managerDir, "restart-called")
	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"  start) printf 'called\\n' > " + shellQuote(startMarker) + "; exit 0;;\n" +
		"  is-active) exit 0;;\n" +
		"  restart) printf 'called\\n' > " + shellQuote(restartMarker) + "; exit 0;;\n" +
		"  reset-failed) exit 0;;\n" +
		"esac\n" +
		"exit 64\n"
	if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write wedged-unit systemctl: %v", err)
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	startServer, serverErr := ensureTestServerStarter(t)
	// The daemon only answers once the manager has restarted the unit.
	stopWatcher := startServerWhenMarked(restartMarker, startServer)
	defer stopWatcher()

	// The reclaim must run even when the readiness wait consumed the whole
	// admission window — callDaemon supplies a real deadline and the wait
	// legitimately spends all of it (RestartSec can outlast the budget), so a
	// reclaim sharing that deadline is dead code on the only path that reaches
	// it (Codex on #4475). This deadline is long enough for the fake `start`
	// to be accepted and then expires inside the readiness wait.
	adHocLaunched := false
	if err := ensureDaemonWithLauncherUntil(func() error {
		adHocLaunched = true
		return startServer()
	}, time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("active-but-unreachable unit must be reclaimed through the manager, got: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start reclaimed supervised daemon: %v", err)
	}
	if adHocLaunched {
		t.Fatal("a wedged unit daemon produced an ad-hoc spawn — the #4470 escape")
	}
	for _, m := range []string{startMarker, restartMarker} {
		if _, err := os.Stat(m); err != nil {
			t.Fatalf("expected manager call %s was not made: %v", filepath.Base(m), err)
		}
	}
}

// TestProbeUnitSupervisorOrdering pins the probe's three states and its check
// order: a systemd manager — either as PID 1 (the sd_booted marker) or as a
// user manager whose runtime marker exists (a `systemd --user` under a
// foreign init, #4475 review) — is consulted before PATH, so "no manager"
// reads absent even when the binary exists (a container shipping systemctl)
// or a FOREIGN bus is reachable (a standalone dbus-daemon owns no manager),
// while "manager but binary missing" or "manager but bus dead" is an
// invocation failure that fails closed — never absence. On darwin launchd is
// always PID 1, so a missing binary is always unreachable.
//
// Every row runs on every host: the probe takes GOOS as an argument, the
// ambient autostartGOOS is poisoned so a probe that fell back to reading it
// fails the linux and darwin rows alike, and the table must carry both arms —
// a linux CI runner exercises the darwin switch and a macOS runner the linux
// one, with every filesystem input sandboxed (#4475 review).
func TestProbeUnitSupervisorOrdering(t *testing.T) {
	prevGOOS := autostartGOOS
	t.Cleanup(func() { autostartGOOS = prevGOOS })
	autostartGOOS = "ambient-goos-must-not-be-read"

	binaryDir := t.TempDir()
	for _, name := range []string{"systemctl", "launchctl"} {
		if err := os.WriteFile(filepath.Join(binaryDir, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	cases := []struct {
		name      string
		goos      string
		booted    bool // linux only: the sd_booted marker exists
		userMgr   bool // linux only: a leftover <runtime>/systemd dir exists (may be a dead manager's residue)
		userBus   bool // linux only: a bus socket exists — a session broker, not manager evidence
		privSock  bool // linux only: the manager's <runtime>/systemd/private socket exists — the liveness marker
		binary    bool // the manager client binary is on PATH
		want      supervisorPresence
		bootedErr bool // linux only: the sd_booted stat fails with a non-ENOENT error
		deadPriv  bool // linux only: <runtime>/systemd/private is a stale inode with no listener
	}{
		{"linux booted + binary", "linux", true, false, false, false, true, supervisorPresent, false, false},
		{"linux booted, binary missing", "linux", true, false, false, false, false, supervisorUnreachable, false, false},
		{"linux not booted + binary", "linux", false, false, false, false, true, supervisorAbsent, false, false},
		{"linux not booted, no binary", "linux", false, false, false, false, false, supervisorAbsent, false, false},
		// A bus endpoint alone is NOT a manager — a foreign dbus-daemon or an
		// inherited DBUS_SESSION_BUS_ADDRESS must read absent (#4475 review).
		{"linux not booted + foreign bus + binary", "linux", false, false, true, false, true, supervisorAbsent, false, false},
		// The runtime dir is NOT liveness either: a failed `systemd --user`
		// leaves <runtime>/systemd behind after unlinking its private socket,
		// and dir + foreign bus still names no manager (Codex on #4475).
		{"linux not booted + leftover manager dir + bus + binary", "linux", false, true, true, false, true, supervisorAbsent, false, false},
		{"linux not booted + leftover manager dir only + binary", "linux", false, true, false, false, true, supervisorAbsent, false, false},
		// A socket INODE is not liveness either: SIGKILL leaves the file
		// behind, and only a refused connect proves no listener owns it
		// (Codex on #4475).
		{"linux not booted + stale private socket + binary", "linux", false, false, false, false, true, supervisorAbsent, false, true},
		// The manager's own private socket IS the live marker — and the path
		// `systemctl --user` connects to directly, so it reads present even
		// with no session broker at all (Codex on #4475).
		{"linux not booted + private socket, no bus + binary", "linux", false, false, false, true, true, supervisorPresent, false, false},
		{"linux not booted + private socket, binary missing", "linux", false, false, false, true, false, supervisorUnreachable, false, false},
		// A non-ENOENT boot-marker stat (a confined process's EACCES/EIO)
		// proves nothing about absence — fail closed, never spawn the
		// escapee (Codex on #4475).
		{"linux booted marker unreadable + binary", "linux", false, false, false, false, true, supervisorUnreachable, true, false},
		{"linux booted marker unreadable + live private socket", "linux", false, false, false, true, true, supervisorPresent, true, false},
		{"darwin binary", "darwin", false, false, false, false, true, supervisorPresent, false, false},
		{"darwin binary missing", "darwin", false, false, false, false, false, supervisorUnreachable, false, false},
		{"unsupported platform", "plan9", false, false, false, false, false, supervisorAbsent, false, false},
	}
	arms := map[string]bool{}
	for _, tc := range cases {
		arms[tc.goos] = true
	}
	for _, goos := range []string{"linux", "darwin"} {
		if !arms[goos] {
			t.Fatalf("the table has no %s rows; every host must exercise both arms of the switch", goos)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The host's real bus env must not leak a manager into a test that
			// wants none: clear the declared-bus variables and point the
			// well-known socket root at a sandbox.
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
			t.Setenv("XDG_RUNTIME_DIR", "")
			prevBase := systemdUserBusBase
			t.Cleanup(func() { systemdUserBusBase = prevBase })
			// SocketTempDir, not TempDir: the fake bus and private sockets are
			// real unix listeners, and macOS's 104-byte sun_path cannot hold a
			// /var/folders/.../T/<test-name>/... path — the bind fails as a bare
			// "invalid argument" there while linux's short /tmp base hides it.
			systemdUserBusBase = testguard.SocketTempDir(t)
			userDir := filepath.Join(systemdUserBusBase, strconv.Itoa(os.Getuid()))
			if tc.userMgr {
				if err := os.MkdirAll(filepath.Join(userDir, "systemd"), 0o700); err != nil {
					t.Fatalf("mkdir user manager marker: %v", err)
				}
			}
			if tc.userBus {
				if err := os.MkdirAll(userDir, 0o700); err != nil {
					t.Fatalf("mkdir user bus dir: %v", err)
				}
				ln, err := net.Listen("unix", filepath.Join(userDir, "bus"))
				if err != nil {
					t.Fatalf("fake user bus socket: %v", err)
				}
				t.Cleanup(func() { ln.Close() })
			}
			if tc.privSock {
				if err := os.MkdirAll(filepath.Join(userDir, "systemd"), 0o700); err != nil {
					t.Fatalf("mkdir user manager dir for private socket: %v", err)
				}
				ln, err := net.Listen("unix", filepath.Join(userDir, "systemd", "private"))
				if err != nil {
					t.Fatalf("fake private manager socket: %v", err)
				}
				t.Cleanup(func() { ln.Close() })
			}
			if tc.deadPriv {
				// A stale remnant: the file exists, no listener owns it.
				// Go unix listeners unlink their path on close, so disable
				// that first to leave the dead inode behind.
				if err := os.MkdirAll(filepath.Join(userDir, "systemd"), 0o700); err != nil {
					t.Fatalf("mkdir user manager dir for stale socket: %v", err)
				}
				ln, err := net.Listen("unix", filepath.Join(userDir, "systemd", "private"))
				if err != nil {
					t.Fatalf("fake stale private socket: %v", err)
				}
				if ul, ok := ln.(*net.UnixListener); ok {
					ul.SetUnlinkOnClose(false)
				}
				ln.Close()
			}
			prevBooted := systemdBootedDir
			t.Cleanup(func() { systemdBootedDir = prevBooted })
			switch {
			case tc.bootedErr:
				// A path longer than every platform's maximum stats as
				// ENAMETOOLONG — a non-ENOENT failure that must fail closed,
				// not read as "not booted".
				systemdBootedDir = strings.Repeat("x", 5000)
			case tc.booted:
				systemdBootedDir = t.TempDir()
			default:
				systemdBootedDir = filepath.Join(t.TempDir(), "no-such-dir")
			}
			if tc.binary {
				t.Setenv("PATH", binaryDir)
			} else {
				t.Setenv("PATH", t.TempDir())
			}
			got, err := probeUnitSupervisor(tc.goos)
			if got != tc.want {
				t.Fatalf("probeUnitSupervisor(%q) on a %s host = %v (err %v), want %v", tc.goos, runtime.GOOS, got, err, tc.want)
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

// TestCallDaemonReclaimPastDeadlineStillDials is the deadline finding's
// regression (Codex on #4475): the unit-path reclaim draws its own budgets,
// so a repair that succeeds legitimately returns after callDaemon's
// admission deadline — the request the repair made deliverable must still
// dial. On the stale deadline the first attempt failed as "daemon admission
// retry deadline elapsed" without ever reaching the now-healthy daemon.
func TestCallDaemonReclaimPastDeadlineStillDials(t *testing.T) {
	_, _ = installEnsureTestUnitAndManager(t, false)

	// Same wedged-unit fake as the ensure-level reclaim test: `start` is a
	// no-op on the already-active unit, `is-active` says active, and only
	// `restart` produces a serving daemon.
	managerDir := t.TempDir()
	restartMarker := filepath.Join(managerDir, "restart-called")
	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"  start) exit 0;;\n" +
		"  is-active) exit 0;;\n" +
		"  restart) printf 'called\\n' > " + shellQuote(restartMarker) + "; exit 0;;\n" +
		"  reset-failed) exit 0;;\n" +
		"esac\n" +
		"exit 64\n"
	if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write wedged-unit systemctl: %v", err)
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	startServer, serverErr := ensureTestServerStarter(t)
	stopWatcher := startServerWhenMarked(restartMarker, startServer)
	defer stopWatcher()

	prevLaunch := launchDaemonProcessFn
	adHocLaunched := false
	launchDaemonProcessFn = func() error { adHocLaunched = true; return startServer() }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	var resp PingResponse
	if err := callDaemon("Ping", PingRequest{}, &resp); err != nil {
		t.Fatalf("a successful reclaim past the admission deadline must still deliver the RPC: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start reclaimed supervised daemon: %v", err)
	}
	if adHocLaunched {
		t.Fatal("a wedged unit daemon produced an ad-hoc spawn — the #4470 escape")
	}
	if _, err := os.Stat(restartMarker); err != nil {
		t.Fatalf("the manager reclaim was not exercised: %v", err)
	}
}

// TestCallDaemonReEnsurePastDeadlineStillDials covers the retry-loop half of
// the deadline finding (Codex on #4475): a failed dial during a proven
// upgrade handoff enters the loop's re-ensure, whose unit-path reclaim can
// legitimately finish AFTER the admission deadline — the repaired daemon is
// still owed the RPC, so the renewed dial must carry it rather than the loop
// returning the pre-repair dial error.
func TestCallDaemonReEnsurePastDeadlineStillDials(t *testing.T) {
	_, _ = installEnsureTestUnitAndManager(t, false)

	// Wedged-unit fake: `start` is a no-op on the already-active unit,
	// `is-active` says active, only `restart` produces a serving daemon.
	managerDir := t.TempDir()
	restartMarker := filepath.Join(managerDir, "restart-called")
	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"  start) exit 0;;\n" +
		"  is-active) exit 0;;\n" +
		"  restart) printf 'called\\n' > " + shellQuote(restartMarker) + "; exit 0;;\n" +
		"  reset-failed) exit 0;;\n" +
		"esac\n" +
		"exit 64\n"
	if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write wedged-unit systemctl: %v", err)
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	startServer, serverErr := ensureTestServerStarter(t)
	stopWatcher := startServerWhenMarked(restartMarker, startServer)
	defer stopWatcher()

	// The first ensure sees a live upgrade handoff (typed gate error), which
	// is what admits the later reclaim into the retry loop at all.
	var gateMu sync.Mutex
	gateCalls := 0
	prevGate := runEntrypointGate
	runEntrypointGate = func(context.Context, string, bool) error {
		gateMu.Lock()
		defer gateMu.Unlock()
		gateCalls++
		if gateCalls == 1 {
			return &upgradetxn.UpgradeInProgressError{
				TransactionID: "txn-4475", ToVersion: "9.9.9",
				Phase:    upgradetxn.PhaseCandidateValidating,
				Deadline: time.Now().Add(time.Minute),
			}
		}
		return nil
	}
	t.Cleanup(func() { runEntrypointGate = prevGate })

	prevLaunch := launchDaemonProcessFn
	adHocLaunched := false
	launchDaemonProcessFn = func() error { adHocLaunched = true; return startServer() }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	var resp PingResponse
	if err := callDaemon("Ping", PingRequest{}, &resp); err != nil {
		t.Fatalf("a successful re-ensure past the admission deadline must still deliver the RPC: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start reclaimed supervised daemon: %v", err)
	}
	if adHocLaunched {
		t.Fatal("a wedged unit daemon produced an ad-hoc spawn — the #4470 escape")
	}
	if _, err := os.Stat(restartMarker); err != nil {
		t.Fatalf("the re-ensure reclaim was not exercised: %v", err)
	}
}

// TestEnsureDaemonActiveUnitReadyDaemonIsNotRestarted is the reclaim's edge
// race (Codex on #4475): is-active proves the unit's process lives but not
// that its daemon is still wedged — the last readiness poll can miss a
// socket the unit bound on the deadline's edge. The re-ping must see the
// answering daemon and leave it running rather than restarting it.
func TestEnsureDaemonActiveUnitReadyDaemonIsNotRestarted(t *testing.T) {
	_, _ = installEnsureTestUnitAndManager(t, false)

	// is-active marks then sleeps so the fake daemon can begin serving mid
	// check: the re-ping after it must find that daemon and skip restart.
	managerDir := t.TempDir()
	activeMarker := filepath.Join(managerDir, "is-active-called")
	restartMarker := filepath.Join(managerDir, "restart-called")
	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"  start) exit 0;;\n" +
		"  is-active) printf 'called\\n' > " + shellQuote(activeMarker) + "; sleep 1; exit 0;;\n" +
		"  restart) printf 'called\\n' > " + shellQuote(restartMarker) + "; exit 0;;\n" +
		"  reset-failed) exit 0;;\n" +
		"esac\n" +
		"exit 64\n"
	if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write edge-race systemctl: %v", err)
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	startServer, serverErr := ensureTestServerStarter(t)
	stopWatcher := startServerWhenMarked(activeMarker, startServer)
	defer stopWatcher()

	adHocLaunched := false
	// Short enough that the first readiness wait expires before is-active's
	// sleep ends — the daemon then answers in the re-ping window.
	if err := ensureDaemonWithLauncherUntil(func() error {
		adHocLaunched = true
		return startServer()
	}, time.Now().Add(1200*time.Millisecond)); err != nil {
		t.Fatalf("a daemon answering at the edge must count as ready, got: %v", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start edge-arriving daemon: %v", err)
	}
	if adHocLaunched {
		t.Fatal("an active unit produced an ad-hoc spawn — the #4470 escape")
	}
	if _, err := os.Stat(restartMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a newly-ready daemon was restarted anyway — the edge race: %v", err)
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
	// Pin the user-bus side of the presence probe to "absent" too: a host
	// DBUS_SESSION_BUS_ADDRESS/XDG_RUNTIME_DIR — or a real /run/user/<uid>/bus
	// — would otherwise smuggle a manager into the cases that repoint
	// systemdBootedDir to mean "absent" (#4475 review).
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	prevBusBase := systemdUserBusBase
	t.Cleanup(func() { systemdUserBusBase = prevBusBase })
	// SocketTempDir, not TempDir: rows that fake the manager's private
	// socket bind a real unix listener under this base, and macOS's
	// 104-byte sun_path cannot hold a /var/folders/.../T/... path.
	systemdUserBusBase = testguard.SocketTempDir(t)
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

// Leading phrases of each remedy a refusal can name. assertRemedyOrder
// matches them as prefixes, position by position, so a test pins the ORDER a
// reader acts in rather than whether a phrase appears somewhere (#4475
// review).
const (
	remedyAdoptLead      = "run `af daemon adopt` ("
	remedyAdoptFailsSame = "if adopt fails the same way, run this from a session with a service manager"
	remedyAdoptHangsToo  = "if adopt hangs too, the service manager itself is not answering: interrupt adopt, then run this from a session with a service manager"
	remedySessionLead    = "run this from a session with a service manager"
	remedyAdoptWontHelp  = "do not expect `af daemon adopt` to help"
	remedyUnmaskLead     = "lift the mask with `systemctl --user unmask " + autostartUnitName + "`, then restore the unit with `af daemon install`"
	remedyReinstallLead  = "re-bootstrap the unit with `af daemon install`"
	remedyPathLead       = "add the directory holding `"
)

var renderedRemedyRE = regexp.MustCompile(`(?:^|;) \((\d+)\) `)

// renderedRemedies parses a refusal's "try, in order: (1) …; (2) …" tail back
// into its ordered items, failing if the numbering is not 1..n.
func renderedRemedies(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatal("want a refusal, got nil")
	}
	_, list, ok := strings.Cut(err.Error(), "refusing to launch an unsupervised daemon — try, in order:")
	if !ok {
		t.Fatalf("refusal names no ordered remedies: %v", err)
	}
	locs := renderedRemedyRE.FindAllStringSubmatchIndex(list, -1)
	items := make([]string, 0, len(locs))
	for i, loc := range locs {
		if got, want := list[loc[2]:loc[3]], strconv.Itoa(i+1); got != want {
			t.Fatalf("remedy numbered %s where %s belongs: %v", got, want, err)
		}
		end := len(list)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		items = append(items, list[loc[1]:end])
	}
	return items
}

func assertRemedyOrder(t *testing.T, got []string, wantPrefixes ...string) {
	t.Helper()
	if len(got) != len(wantPrefixes) {
		t.Fatalf("got %d remedies %q, want %d led by %q", len(got), got, len(wantPrefixes), wantPrefixes)
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(got[i], prefix) {
			t.Fatalf("remedy %d = %q, want it to start with %q (full order %q)", i+1, got[i], prefix, got)
		}
	}
}

// TestClassifyUnitStartFailureBothArms pins which class each start failure
// lands in, for both platform arms on every host: GOOS is an argument, and
// the bus environment and manager socket root are sandboxed.
func TestClassifyUnitStartFailureBothArms(t *testing.T) {
	timedOut := fmt.Errorf("systemctl --user start unit timed out: %w", context.DeadlineExceeded)
	refused := errors.New("systemctl --user start unit failed: exit status 1\nJob failed.")
	for _, tc := range []struct {
		name     string
		goos     string
		busEnv   bool // XDG_RUNTIME_DIR names a directory
		privSock bool // a live <base>/<uid>/systemd/private listener exists
		err      error
		want     startFailureClass
	}{
		{"linux refused, bus configured", "linux", true, false, refused, startRefused},
		{"linux refused, no bus env, no manager socket", "linux", false, false, refused, startBusUnreachable},
		{"linux refused, no bus env, live manager socket", "linux", false, true, refused, startRefused},
		{"linux connect failure, bus configured", "linux", true, false, errors.New("Failed to connect to bus: No such file or directory"), startBusUnreachable},
		{"linux connect failure, live manager socket", "linux", false, true, errors.New("Failed to connect to bus: No medium found"), startBusUnreachable},
		{"linux masked", "linux", true, false, errors.New("Unit agent-factory-daemon.service is masked."), startMasked},
		{"linux timeout", "linux", true, false, timedOut, startHung},
		// The deadline outranks the environment inference.
		{"linux timeout, no bus env", "linux", false, false, timedOut, startHung},
		{"linux does not read launchd's not-loaded text", "linux", true, false, errors.New("Could not find service"), startRefused},
		{"darwin not loaded", "darwin", false, false, errors.New("Could not find service \"com.x\" in domain for port"), startNotLoaded},
		{"darwin refused, empty env is not a bus verdict", "darwin", false, false, refused, startRefused},
		{"darwin does not read systemd's masked text", "darwin", false, false, errors.New("is masked"), startRefused},
		{"darwin timeout", "darwin", false, false, timedOut, startHung},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
			if tc.busEnv {
				t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			} else {
				t.Setenv("XDG_RUNTIME_DIR", "")
			}
			prevBase := systemdUserBusBase
			t.Cleanup(func() { systemdUserBusBase = prevBase })
			systemdUserBusBase = testguard.SocketTempDir(t)
			if tc.privSock {
				dir := filepath.Join(systemdUserBusBase, strconv.Itoa(os.Getuid()), "systemd")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir private socket dir: %v", err)
				}
				ln, err := net.Listen("unix", filepath.Join(dir, "private"))
				if err != nil {
					t.Fatalf("fake private manager socket: %v", err)
				}
				t.Cleanup(func() { ln.Close() })
			}
			if got := classifyUnitStartFailure(tc.goos, tc.err); got != tc.want {
				t.Fatalf("classifyUnitStartFailure(%q, %q) on a %s host = %d, want %d", tc.goos, tc.err, runtime.GOOS, got, tc.want)
			}
		})
	}
}

// TestUnitRefusalRemediesOrderByClass pins the full remedy order of every
// refusal class on both platform arms, and that the rendered message keeps
// exactly that order. Bus-unreachable leads with the session remedy because
// adopt fails the same way there; refused and hung lead with adopt because
// the manager is reachable and the problem is the unit; every class ends with
// its unmanaged-home escape hatch (#4475 review).
func TestUnitRefusalRemediesOrderByClass(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			for _, tc := range []struct {
				class startFailureClass
				want  []string
			}{
				{startRefused, []string{remedyAdoptLead, remedyAdoptFailsSame, uninstallRemedy}},
				{startHung, []string{remedyAdoptLead, remedyAdoptHangsToo, uninstallRemedy}},
				{startBusUnreachable, []string{remedySessionLead, remedyAdoptWontHelp, uninstallRemedy}},
				// A masked unit's own file is the mask: unmask deletes it, so
				// install (not adopt) restores it, and stopping there is the
				// unmanaged exit `af daemon uninstall` cannot take.
				{startMasked, []string{remedyUnmaskLead, maskedUnmanagedRemedy}},
				{startNotLoaded, []string{remedyReinstallLead, uninstallRemedy}},
			} {
				got := unitStartRemedies(goos, tc.class)
				assertRemedyOrder(t, got, tc.want...)
				// The rendered refusal carries the same order the list does.
				startErr := errors.New("start failed")
				rendered := renderedRemedies(t, fmt.Errorf("x (%w); refusing to launch an unsupervised daemon — %s", startErr, formatRemedies(got)))
				if !reflect.DeepEqual(rendered, got) {
					t.Fatalf("class %d rendered %q, want %q", tc.class, rendered, got)
				}
			}

			// The bus lead is concrete: the diagnostic that proves a session
			// qualifies, and on linux the two settings that turn a cron job
			// into one.
			busLead := unitStartRemedies(goos, startBusUnreachable)[0]
			if !strings.Contains(busLead, unitStatusDiagnostic(goos)) {
				t.Fatalf("bus lead %q must name the diagnostic %q", busLead, unitStatusDiagnostic(goos))
			}
			hungLead := unitStartRemedies(goos, startHung)[0]
			if goos == "linux" {
				for _, want := range []string{"XDG_RUNTIME_DIR=/run/user/" + uid, "loginctl enable-linger " + uid} {
					if !strings.Contains(busLead, want) {
						t.Fatalf("linux bus lead %q must name %q", busLead, want)
					}
				}
				if !strings.Contains(hungLead, "RestartSec") {
					t.Fatalf("linux hung lead %q must say why adopt beats a queued start", hungLead)
				}
			} else {
				for _, notWant := range []string{"XDG_RUNTIME_DIR", "loginctl", "systemctl"} {
					if strings.Contains(busLead, notWant) {
						t.Fatalf("darwin bus lead %q must not name systemd's %q", busLead, notWant)
					}
				}
				if strings.Contains(hungLead, "RestartSec") {
					t.Fatalf("darwin hung lead %q must not cite systemd's RestartSec", hungLead)
				}
			}

			// A manager that exists but cannot be invoked: PATH first when the
			// binary is missing, never adopt (same binary, same PATH). The
			// prefix is asserted through the directory literal so the
			// (usually …) hint cannot silently regress to the wrong path
			// (#4484: launchctl ships in /usr/bin, not /bin).
			bin := map[string]string{"linux": "systemctl", "darwin": "launchctl"}[goos]
			pathMiss := fmt.Errorf("no binary: %w", &exec.Error{Name: bin, Err: exec.ErrNotFound})
			pathLead := remedyPathLead + bin + "` (usually /usr/bin)"
			assertRemedyOrder(t, unreachableSupervisorRemedies(goos, pathMiss),
				pathLead, remedySessionLead, uninstallRemedy)
			if got := unreachableSupervisorRemedies(goos, pathMiss)[0]; !strings.Contains(got, "(usually /usr/bin)") {
				t.Fatalf("PATH remedy %q must name /usr/bin, not /bin (launchctl and systemctl both ship there)", got)
			}
			unreadable := fmt.Errorf("boot marker unreadable: %w", os.ErrPermission)
			assertRemedyOrder(t, unreachableSupervisorRemedies(goos, unreadable),
				remedySessionLead, uninstallRemedy)

			// Accepted but silent: waiting and adopt stay the working verbs.
			assertRemedyOrder(t, unitReadinessRemedies(goos),
				"retry shortly", "check `"+unitStatusDiagnostic(goos)+"`", "reclaim the unit's daemon with `af daemon adopt`", uninstallRemedy)
		})
	}
}
