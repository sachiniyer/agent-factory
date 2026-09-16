package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// SupervisionOwner is the derived owner of one AF home's daemon lifecycle.
// It is never persisted: the installed unit and its baked AF home remain the
// source of truth.
type SupervisionOwner uint8

const (
	OwnerUnknown SupervisionOwner = iota
	OwnerUnit
	OwnerAdHoc
)

// ResolveSupervisionOwner derives whether the installed service-manager unit
// belongs to configDir. An absent or foreign unit leaves this home ad-hoc; a
// read/parse/canonicalization failure is explicitly unknown rather than being
// inverted into either owner.
func ResolveSupervisionOwner(configDir string) (SupervisionOwner, error) {
	serves, _, err := AutostartUnitServesHome(configDir)
	if err != nil {
		return OwnerUnknown, err
	}
	if serves {
		return OwnerUnit, nil
	}
	return OwnerAdHoc, nil
}

// The unit gets a bounded share of EnsureDaemon's existing five-second ready
// budget. A wedged manager must leave time for the compatibility fallback to
// bind and answer rather than consuming the whole launch window itself.
const (
	ensureUnitStartTimeout   = 2 * time.Second
	ensureUnitStartWaitDelay = 250 * time.Millisecond
)

// systemdBootedDir is the marker sd_booted(3) uses for "systemd is PID 1":
// present only while systemd actually runs the system, so a container or
// foreign root that merely carries a systemctl binary still reads absent.
// A var so tests can point it at a sandbox.
var systemdBootedDir = "/run/systemd/system"

// checkUnitSupervisorAbsent returns a non-nil error when the service manager
// an installed unit is started through provably cannot exist in this
// environment — no manager binary, no systemd as init, or a platform with no
// autostart support. That is the sole condition under which an ad-hoc daemon
// remains permitted on a unit-claimed home (#4470): a refusal, a hang, or a
// missing session bus all leave a manager that could run the unit later, and
// an ad-hoc spawn there becomes a permanent unsupervised escapee.
func checkUnitSupervisorAbsent() error {
	switch autostartGOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return fmt.Errorf("no systemctl binary in PATH: %w", err)
		}
		if _, err := os.Stat(systemdBootedDir); err != nil {
			return fmt.Errorf("systemd is not running this system: %w", err)
		}
		return nil
	case "darwin":
		// launchd is always PID 1 on macOS; only a missing binary counts as
		// "no supervisor could exist" — a gui-domain or bus failure is a
		// refusal case, not absence.
		if _, err := exec.LookPath("launchctl"); err != nil {
			return fmt.Errorf("no launchctl binary in PATH: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

func runEnsureUnitStartCommand(deadline time.Time) error {
	switch autostartGOOS {
	case "linux":
		// Do not reset-failed here. EnsureDaemon is implicit in ordinary client
		// calls, so clearing the retained state on every call would turn Phase
		// 1's bounded crash loop into another restart burst per TUI/RPC action.
		// Explicit install/restart/resume operations remain the recovery paths
		// that deliberately clear a repaired unit's start-limit state.
		return runEnsureManagerCommand(
			deadline, "systemctl", "--user", "start", autostartUnitName,
		)
	case "darwin":
		return runEnsureManagerCommand(
			deadline, "launchctl", "kickstart", "-k", launchdServiceTarget(),
		)
	default:
		return fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

func runEnsureManagerCommand(deadline time.Time, name string, args ...string) error {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = ensureUnitStartWaitDelay

	out, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		// The manager exited zero. A deadline that fires while WaitDelay is
		// cleaning inherited pipes cannot retroactively make the command time out.
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s timed out: %w", name, strings.Join(args, " "), ctx.Err())
	}
	return fmt.Errorf("%s %s failed: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}
