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
		"if [ \"$1\" != \"kickstart\" ] || [ \"$2\" != \"" + launchdServiceTarget() + "\" ]; then\n" +
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

func TestEnsureDaemonManagerHangFallsBackWithDegradation(t *testing.T) {
	marker, _ := installEnsureTestUnitAndManager(t, true)
	startServer, serverErr := ensureTestServerStarter(t)

	adHocLaunched := false
	started := time.Now()
	err := ensureDaemonWithLauncher(func() error {
		adHocLaunched = true
		return startServer()
	})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("manager degradation was silent; want an explicit non-nil result")
	}
	var degraded *SupervisionDegradedError
	if !errors.As(err, &degraded) {
		t.Fatalf("degradation error type = %T, want *SupervisionDegradedError", err)
	}
	if !strings.Contains(err.Error(), "unsupervised daemon") || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("degradation error = %q, want unsupervised fallback plus timeout cause", err)
	}
	if err := serverErr(); err != nil {
		t.Fatalf("start fake ad-hoc daemon: %v", err)
	}
	if !adHocLaunched {
		t.Fatal("hung manager left no daemon instead of falling back")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the manager hang was not actually exercised: %v", err)
	}
	if elapsed >= daemonReadyTimeout {
		t.Fatalf("manager fallback took %s, want less than the existing %s readiness budget", elapsed, daemonReadyTimeout)
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
	unit := systemdAutostartUnit("/opt/agent-factory/bin/af", "", "", home)
	if err := os.WriteFile(filepath.Join(unitDir, autostartUnitName), []byte(unit), 0o600); err != nil {
		t.Fatalf("write home-serving unit: %v", err)
	}

	managerDir := t.TempDir()
	marker := filepath.Join(managerDir, "start-called")
	script := "#!/bin/sh\n" +
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
