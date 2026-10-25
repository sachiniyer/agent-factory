package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestEnsureDaemonLoadedUnitFileAbsentRefusesAdHoc closes the supervision-
// escape path the Codex review on #5528 named for a unit whose on-disk file
// was removed after it was loaded: AutostartUnitServesHome reads the missing
// file as "not installed" (os.ReadFile returns ENOENT), the dangling-link gate
// does not fire (the path is simply absent, not a broken symlink), and without
// a loaded-job probe ResolveSupervisionOwner would hand back OwnerAdHoc — so a
// cold start would spawn an unsupervised daemon even though the manager still
// has the home-claiming job loaded and can restart it at any moment, the same
// #4470 escape the dangling-link case closes. Querying the manager by name
// (systemctl --user show LoadState) detects a loaded job the missing file no
// longer describes, so the home reads OwnerUnknown and the ensure path fails
// closed under a live supervisor (the ownerErr gate's probeUnitSupervisor
// refuses unless the manager is provably absent). The #2373 ad-hoc fallback
// survives only when the manager is provably absent (no LoadState answer).
func TestEnsureDaemonLoadedUnitFileAbsentRefusesAdHoc(t *testing.T) {
	t.Run("loaded unit, file absent, supervisor present", func(t *testing.T) {
		// A unit the service manager still has loaded after its on-disk file was
		// removed (without a disable or daemon-reload) is the on-disk-absent
		// counterpart of the dangling-link case: AutostartUnitServesHome reads
		// the missing file as "not installed", the dangling-link gate does not
		// fire (the path is simply absent, not a broken symlink), and without a
		// loaded-job probe ResolveSupervisionOwner would hand back OwnerAdHoc —
		// letting EnsureDaemon spawn an unsupervised daemon that races the
		// loaded job the manager can still start (#4470/#4475). The manager is
		// queried by name (systemctl --user show LoadState), not by file, so a
		// loaded-but-file-deleted unit reads OwnerUnknown and fails closed under
		// a live supervisor; the standard #2373 fallback survives only when the
		// manager is provably absent (not loaded, no LoadState answer).
		marker, home := installEnsureTestUnitAndManager(t, false)
		owner, ownerErr := ResolveSupervisionOwner(home)
		if owner != OwnerUnit || ownerErr != nil {
			t.Fatalf("prereq: readable unit must resolve to OwnerUnit, got owner=%v err=%v", owner, ownerErr)
		}
		unitPath, err := autostartUnitFilePath()
		if err != nil {
			t.Fatalf("resolve unit path: %v", err)
		}
		if err := os.Remove(unitPath); err != nil {
			t.Fatalf("remove unit file: %v", err)
		}
		// The manager still reports the unit as loaded by name even though its
		// on-disk file is gone (LoadState "loaded"). The ensure path must refuse
		// rather than start the unit through the manager, so any start call is an
		// unexpected escape the marker catches.
		managerDir := filepath.Dir(marker)
		loadedScript := "#!/bin/sh\n" +
			"if [ \"$1\" = \"--user\" ] && [ \"$2\" = \"show\" ] && [ \"$3\" = \"" + autostartUnitName + "\" ] && [ \"$4\" = \"--property=LoadState\" ] && [ \"$5\" = \"--value\" ]; then\n" +
			"  printf 'loaded\\n'\n" +
			"  exit 0\n" +
			"fi\n" +
			"printf 'called\\n' > " + shellQuote(marker) + "\n" +
			"exit 64\n"
		if err := os.WriteFile(filepath.Join(managerDir, "systemctl"), []byte(loadedScript), 0o700); err != nil {
			t.Fatalf("write LoadState-loaded fake systemctl: %v", err)
		}

		owner, ownerErr = ResolveSupervisionOwner(home)
		if owner != OwnerUnknown || ownerErr == nil {
			t.Fatalf("a loaded-but-file-deleted unit must resolve to OwnerUnknown, got owner=%v err=%v", owner, ownerErr)
		}

		warnBuf := captureWarnings(t)
		startServer, _ := ensureTestServerStarter(t)
		adHocLaunched := false
		err = ensureDaemonWithLauncher(func() error {
			adHocLaunched = true
			return startServer()
		})
		if err == nil {
			t.Fatal("a loaded-but-file-deleted unit under a live supervisor produced an ad-hoc daemon — the #4470 escape")
		}
		if !strings.Contains(err.Error(), "ownership of this home could not be determined") {
			t.Fatalf("refusal must name the ownership failure, got: %v", err)
		}
		if !strings.Contains(err.Error(), "loaded by the service manager but its on-disk file is absent") {
			t.Fatalf("refusal must name the loaded-but-absent cause, got: %v", err)
		}
		// The loaded-but-file-absent case carries the same manual-unload escape
		// hatch as the dangling-link case: af daemon uninstall follows the
		// missing file to ENOENT with os.Stat and reports the home uninstalled
		// without unloading the loaded job, so the standard uninstallRemedy
		// would mislead the operator (Codex on #5528). The remedy stops at the
		// unload — reinstalling would re-create the unit file the manager still
		// has loaded and undo the unmanaged exit.
		assertRemedyOrder(t, renderedRemedies(t, err), remedyOwnerResolveLead, remedyAdoptWontHelp, loadedAbsentUnloadRemedy(autostartGOOS))
		if strings.Contains(renderedRemedies(t, err)[2], uninstallRemedy) {
			t.Fatalf("loaded-but-absent escape must not name the no-op `af daemon uninstall`: %q", renderedRemedies(t, err)[2])
		}
		if adHocLaunched {
			t.Fatal("uncertain ownership spawned an unsupervised daemon")
		}
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("an uncertain-ownership cold start invoked the service manager; marker stat = %v", statErr)
		}
		if !strings.Contains(warnBuf.String(), "could not determine daemon supervision owner") {
			t.Fatalf("fallback must warn about the ownership failure; log = %q", warnBuf.String())
		}
		if strings.Contains(warnBuf.String(), "using ad-hoc launch") {
			t.Fatalf("the old fail-open warning survived; the uncertain-owner branch must not announce an ad-hoc launch it did not take: %q", warnBuf.String())
		}
	})

	t.Run("loaded unit, file absent, supervisor absent", func(t *testing.T) {
		// #2373 preserved for the loaded-but-file-deleted case: when the
		// manager provably cannot exist, a loaded job cannot be running, so the
		// ad-hoc fallback is the only working path — the LoadState probe cannot
		// confirm a loaded job, so the home reads OwnerAdHoc and the #2373
		// fallback spawns the ad-hoc daemon.
		home := testguard.SocketTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", home)
		unitDir := withAutostartTestEnv(t, "linux")
		unitPath := filepath.Join(unitDir, autostartUnitName)
		if err := os.WriteFile(unitPath, []byte(systemdAutostartUnit("/opt/agent-factory/bin/af", "", "", home)), 0o600); err != nil {
			t.Fatalf("write home-serving unit: %v", err)
		}
		if err := os.Remove(unitPath); err != nil {
			t.Fatalf("remove unit file: %v", err)
		}
		// A booted marker that does not exist is the only proven-absent case;
		// the user-bus side is sandboxed so a real /run/user/<uid>/systemd/private
		// cannot smuggle a manager in.
		prevBooted := systemdBootedDir
		t.Cleanup(func() { systemdBootedDir = prevBooted })
		systemdBootedDir = filepath.Join(t.TempDir(), "no-such-dir")
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		t.Setenv("XDG_RUNTIME_DIR", "")
		prevBusBase := systemdUserBusBase
		t.Cleanup(func() { systemdUserBusBase = prevBusBase })
		systemdUserBusBase = testguard.SocketTempDir(t)
		// No systemctl in PATH: the LoadState probe cannot reach a manager, so
		// the unit cannot be confirmed loaded and the home stays OwnerAdHoc.
		t.Setenv("PATH", t.TempDir())

		owner, ownerErr := ResolveSupervisionOwner(home)
		if owner != OwnerAdHoc || ownerErr != nil {
			t.Fatalf("a file-deleted unit with no manager must resolve to OwnerAdHoc, got owner=%v err=%v", owner, ownerErr)
		}

		startServer, serverErr := ensureTestServerStarter(t)
		adHocLaunched := false
		err := ensureDaemonWithLauncher(func() error {
			adHocLaunched = true
			return startServer()
		})
		if err != nil {
			t.Fatalf("supervisor-absent loaded-but-file-deleted must return a reachable daemon: %v", err)
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
	})
}

// TestLoadedAbsentUnloadRemedyOrder pins the unmanaged-home escape hatch the
// loaded-but-file-absent case carries: af daemon uninstall follows the missing
// file to ENOENT with os.Stat and reports the home uninstalled without
// unloading the loaded job (the same shape a dangling link takes), and
// `systemctl --user disable --now` fails before stopping on a unit file it
// cannot read, so the standard uninstallRemedy would mislead the operator
// (Codex on #5528). The escape names the manual stop by unit name instead,
// never the no-op uninstall, and never `af daemon install` (reinstalling would
// re-create the unit file and re-enable the job, undoing the unmanaged exit the
// operator followed this remedy to reach). Both platform arms are pinned so a
// linux/darwin regression cannot silently swap them.
func TestLoadedAbsentUnloadRemedyOrder(t *testing.T) {
	loadedAbsentUnloadLead := map[string]string{
		"linux":  "if this home should be unmanaged, stop the loaded service by name with `systemctl --user stop " + autostartUnitName + "`",
		"darwin": "if this home should be unmanaged, unload the loaded service by name with `launchctl bootout " + launchdServiceTarget() + "`",
	}
	loadedAbsentStopCmd := map[string]string{
		"linux":  "systemctl --user stop " + autostartUnitName,
		"darwin": "launchctl bootout " + launchdServiceTarget(),
	}
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			order := unknownOwnerRemedies(goos, fmt.Errorf("loaded but absent: %w", errLoadedUnitAbsent))
			assertRemedyOrder(t, order, remedyOwnerResolveLead, remedyAdoptWontHelp, loadedAbsentUnloadLead[goos])
			if !strings.Contains(order[2], loadedAbsentStopCmd[goos]) {
				t.Fatalf("%s loaded-but-absent escape %q must name the manual stop command %q", goos, order[2], loadedAbsentStopCmd[goos])
			}
			if strings.Contains(order[2], uninstallRemedy) {
				t.Fatalf("%s loaded-but-absent escape must not name the no-op `af daemon uninstall`: %q", goos, order[2])
			}
			if strings.Contains(order[2], "af daemon install") {
				t.Fatalf("%s loaded-but-absent escape must not name `af daemon install` (it would undo the unload): %q", goos, order[2])
			}
			if strings.Contains(order[2], "dangling") {
				t.Fatalf("%s loaded-but-absent escape must not name the dangling link: %q", goos, order[2])
			}
		})
	}
}
