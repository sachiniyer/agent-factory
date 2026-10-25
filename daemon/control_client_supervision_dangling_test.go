package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestEnsureDaemonDanglingUnitLinkRefusesAdHoc closes the supervision-escape
// path the Codex review on #4470/#4475 named for a DANGLING symlink: when the
// autostart unit path (or an ancestor of it) is a symlink whose target no
// longer exists, os.ReadFile follows the link, gets ENOENT, and
// AutostartUnitServesHome returns (false, false, nil) — the same shape as a
// truly absent unit. ResolveSupervisionOwner handed back OwnerAdHoc, so the
// ownerErr gate in ensureDaemonWithPolicyUntil was never reached, and a host
// whose service manager still held the previously loaded home-claiming unit
// could cold-start an unsupervised daemon that won the socket before
// probeUnitSupervisor was ever asked. A broken link is a read failure (the path
// exists as a link whose target cannot be resolved), not a proven-absent unit,
// so it must classify as OwnerUnknown and fail closed under a live supervisor
// — the same shape the unreadable-unit and unresolvable-directory rows above
// pin — while the #2373 ad-hoc fallback survives only when the manager is
// provably absent.
func TestEnsureDaemonDanglingUnitLinkRefusesAdHoc(t *testing.T) {
	t.Run("dangling unit symlink, supervisor present", func(t *testing.T) {
		marker, home := installEnsureTestUnitAndManager(t, false) // booted + systemctl, home-claiming unit
		// Sanity: the unit claims this home when it can be read, so the
		// dangling link below is a real "claimed home" escape, not the benign
		// no-claim case the old `unknown` subtest pinned.
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
		// A dangling symlink at the unit path: os.ReadFile follows the link
		// and returns ENOENT (os.IsNotExist), so AutostartUnitServesHome
		// reports the unit as absent — but Lstat sees the link and Stat cannot
		// resolve it, the production shape a still-loaded unit hides behind.
		missingTarget := filepath.Join(t.TempDir(), "no-such-target")
		if err := os.Symlink(missingTarget, unitPath); err != nil {
			t.Fatalf("stage dangling symlink at unit path: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(unitPath) })

		// The dangling link must read as OwnerUnknown, not the OwnerAdHoc the
		// old IsNotExist-as-absence conversion returned.
		owner, ownerErr = ResolveSupervisionOwner(home)
		if owner != OwnerUnknown || ownerErr == nil {
			t.Fatalf("dangling unit symlink must resolve to OwnerUnknown, got owner=%v err=%v", owner, ownerErr)
		}

		warnBuf := captureWarnings(t)
		startServer, _ := ensureTestServerStarter(t)
		adHocLaunched := false
		err = ensureDaemonWithLauncher(func() error {
			adHocLaunched = true
			return startServer()
		})
		if err == nil {
			t.Fatal("a dangling home-claiming unit under a live supervisor produced an ad-hoc daemon — the #4470 escape")
		}
		if !strings.Contains(err.Error(), "ownership of this home could not be determined") {
			t.Fatalf("refusal must name the ownership failure, got: %v", err)
		}
		assertRemedyOrder(t, renderedRemedies(t, err), remedyOwnerResolveLead, remedyAdoptWontHelp, uninstallRemedy)
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

	t.Run("dangling unit symlink, supervisor absent", func(t *testing.T) {
		// #2373 preserved for the dangling-link case too: when the manager
		// provably cannot exist, a broken link hides no live unit, so the
		// ad-hoc fallback remains the only working path — mirroring the
		// unresolvable-unit-directory, supervisor-absent row in
		// TestEnsureDaemonUnknownOwnerRefusesAdHoc.
		home := testguard.SocketTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", home)
		unitDir := withAutostartTestEnv(t, "linux")
		unitPath := filepath.Join(unitDir, autostartUnitName)
		missingTarget := filepath.Join(t.TempDir(), "no-such-target")
		if err := os.Symlink(missingTarget, unitPath); err != nil {
			t.Fatalf("stage dangling symlink at unit path: %v", err)
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
		t.Setenv("PATH", t.TempDir()) // no systemctl anywhere

		owner, ownerErr := ResolveSupervisionOwner(home)
		if owner != OwnerUnknown || ownerErr == nil {
			t.Fatalf("prereq: dangling unit symlink must resolve to OwnerUnknown even with no manager, got owner=%v err=%v", owner, ownerErr)
		}

		warnBuf := captureWarnings(t)
		startServer, serverErr := ensureTestServerStarter(t)
		adHocLaunched := false
		err := ensureDaemonWithLauncher(func() error {
			adHocLaunched = true
			return startServer()
		})
		if err != nil {
			t.Fatalf("supervisor-absent dangling link must return a reachable daemon: %v", err)
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
			t.Fatalf("supervisor-absent dangling link must warn about the ad-hoc fallback; log = %q", warnBuf.String())
		}
	})

	t.Run("dangling ancestor symlink, supervisor present", func(t *testing.T) {
		// An ancestor directory of the unit path can be the broken link: a
		// symlink at the unit directory whose target was removed makes
		// os.ReadFile return ENOENT for the unit under it, indistinguishable
		// from a truly absent unit without an Lstat walk up the path. The
		// manager may still hold the previously loaded home-claiming unit, so
		// this is the same escape as a dangling link at the path itself.
		marker, home := installEnsureTestUnitAndManager(t, false)
		_, _ = ResolveSupervisionOwner(home) // sanity: the install wrote a readable unit
		unitPath, err := autostartUnitFilePath()
		if err != nil {
			t.Fatalf("resolve unit path: %v", err)
		}
		unitDir := filepath.Dir(unitPath)
		// Replace the unit directory with a dangling symlink: every path under
		// it reads as ENOENT through os.ReadFile, while the loaded unit still
		// claims this home.
		if err := os.RemoveAll(unitDir); err != nil {
			t.Fatalf("remove unit dir: %v", err)
		}
		missingTarget := filepath.Join(t.TempDir(), "no-such-dir")
		if err := os.Symlink(missingTarget, unitDir); err != nil {
			t.Fatalf("stage dangling symlink at unit dir: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(unitDir) })

		owner, ownerErr := ResolveSupervisionOwner(home)
		if owner != OwnerUnknown || ownerErr == nil {
			t.Fatalf("dangling ancestor symlink must resolve to OwnerUnknown, got owner=%v err=%v", owner, ownerErr)
		}

		warnBuf := captureWarnings(t)
		startServer, _ := ensureTestServerStarter(t)
		adHocLaunched := false
		err = ensureDaemonWithLauncher(func() error {
			adHocLaunched = true
			return startServer()
		})
		if err == nil {
			t.Fatal("a dangling ancestor under a live supervisor produced an ad-hoc daemon — the #4470 escape")
		}
		if !strings.Contains(err.Error(), "ownership of this home could not be determined") {
			t.Fatalf("refusal must name the ownership failure, got: %v", err)
		}
		if adHocLaunched {
			t.Fatal("uncertain ownership spawned an unsupervised daemon")
		}
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("an uncertain-ownership cold start invoked the service manager; marker stat = %v", statErr)
		}
		_ = warnBuf // the cold start refused before any fallback warning
	})
}
