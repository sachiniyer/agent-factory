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

// supervisorPresence is the three-way answer the unit-start gate needs. The
// states are not a boolean because "absent" must be proven, never inferred
// from a failed lookup: a manager that EXISTS but cannot be invoked from this
// environment is a fail-closed case, not an ad-hoc one.
type supervisorPresence uint8

const (
	// supervisorPresent: the manager runs this system and its client binary is
	// reachable, so the unit path can start the daemon.
	supervisorPresent supervisorPresence = iota
	// supervisorAbsent: the manager provably cannot exist in this environment —
	// the ONLY state permitting an ad-hoc daemon on a unit-claimed home (#2373).
	supervisorAbsent
	// supervisorUnreachable: the manager runs this system but this process
	// cannot invoke it — e.g. an `af` invoked by absolute path from an env
	// whose PATH omits the client binary. Fail closed: an ad-hoc spawn here
	// escapes a supervisor that is real (#4470).
	supervisorUnreachable
)

// probeUnitSupervisor determines whether the service manager an installed unit
// is started through can act from THIS environment. The boot marker is checked
// FIRST and on its own: whether systemd/launchd runs the system is a property
// of the OS, independent of this process's PATH — so a container that merely
// carries the client binary still reads absent. Only once the manager is known
// to exist does a missing binary become meaningful, and there it is an
// inability to invoke, never proof of absence (#4470).
func probeUnitSupervisor() (supervisorPresence, error) {
	switch autostartGOOS {
	case "linux":
		if _, err := os.Stat(systemdBootedDir); err != nil {
			return supervisorAbsent, fmt.Errorf("systemd is not running this system: %w", err)
		}
		if _, err := exec.LookPath("systemctl"); err != nil {
			return supervisorUnreachable, fmt.Errorf("systemd runs this system but no systemctl binary is reachable in PATH: %w", err)
		}
		return supervisorPresent, nil
	case "darwin":
		// launchd is always PID 1 on macOS — there is no "not booted under
		// launchd" state to detect, so the manager provably exists on every
		// darwin host. A missing binary can therefore only be an invocation
		// failure, never absence.
		if _, err := exec.LookPath("launchctl"); err != nil {
			return supervisorUnreachable, fmt.Errorf("launchd runs this system but no launchctl binary is reachable in PATH: %w", err)
		}
		return supervisorPresent, nil
	default:
		return supervisorAbsent, fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

// unitStartBusUnreachable reports whether a failed unit start could not reach
// the manager at all — no session bus to drive it through (a user cron job, a
// system service, an SSH-less environment). In that class `af daemon adopt`
// cannot work either: it drives the same manager through the same bus and
// fails identically, so the refusal must lead with the remedy that can work.
func unitStartBusUnreachable(startErr error) bool {
	// Deterministic on linux: with neither variable set, systemctl has no
	// address through which to reach the user bus at all — no stderr parsing.
	if autostartGOOS == "linux" &&
		os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" &&
		os.Getenv("XDG_RUNTIME_DIR") == "" {
		return true
	}
	// A bus address existed but the connect still failed (stale socket, dead
	// user manager), or launchd has no gui domain for this uid — take the
	// manager's own words.
	return strings.Contains(startErr.Error(), "connect to bus") ||
		strings.Contains(startErr.Error(), "Could not find service")
}

// unitStartRemedy orders the remedies named in a start refusal by failure
// class: a bus-unreachable start cannot be fixed by adopt, so the session
// remedy leads; a refused or hung start leaves adopt the working verb.
func unitStartRemedy(startErr error) string {
	if unitStartBusUnreachable(startErr) {
		return "start the unit from a session with a service manager — `af daemon adopt` cannot reach the manager from this environment either"
	}
	return "run `af daemon adopt`, or start the unit from a session with a service manager"
}

// unitStatusDiagnostic names the manager-specific inspection command for the
// readiness-failure hint — systemd and launchd share no spelling.
func unitStatusDiagnostic() string {
	if autostartGOOS == "darwin" {
		return fmt.Sprintf("launchctl print %s", launchdServiceTarget())
	}
	return fmt.Sprintf("systemctl --user status %s", autostartUnitName)
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
