package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// systemdUserBusBase roots the user-manager probe: while `systemd --user`
// runs, its bus socket lives at <base>/<uid>/bus — present on hosts where a
// user manager exists WITHOUT systemd as PID 1 (a container or foreign root
// running `systemd --user` independently), where the boot marker alone would
// misread a live manager as absent (#4475 review). A var so tests can point
// it at a sandbox.
var systemdUserBusBase = "/run/user"

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
// is started through can act from THIS environment. Manager existence is
// checked FIRST and on its own — a property of the OS, independent of this
// process's PATH: the sd_booted marker for systemd-as-PID-1, OR the user
// manager's own runtime marker for a `systemd --user` running under a foreign
// init (#4475 review), so a container that merely carries the client binary —
// or a session bus with no manager behind it — still reads absent. Only once
// the manager is known to exist does a missing binary become meaningful, and
// there it is an inability to invoke, never proof of absence (#4470).
func probeUnitSupervisor() (supervisorPresence, error) {
	switch autostartGOOS {
	case "linux":
		// Presence is "a manager this unit can be started through exists": the
		// sd_booted marker proves systemd-as-PID-1, and the user manager's
		// runtime state dir covers `systemd --user` running under a
		// non-systemd init — the boot marker cannot prove THAT manager absent
		// because `systemctl --user` drives the user manager, not PID 1. The
		// bus endpoint deliberately does NOT prove presence: a foreign
		// dbus-daemon (an OpenRC session, a container inheriting
		// DBUS_SESSION_BUS_ADDRESS) owns a bus with no manager behind it
		// (#4475 review). Neither marker existing is the only proven-absent
		// case.
		_, bootErr := os.Stat(systemdBootedDir)
		booted := bootErr == nil
		if !booted && !systemdUserManagerPresent() {
			return supervisorAbsent, fmt.Errorf("systemd runs neither this system nor a user manager in this session: %w", bootErr)
		}
		if _, err := exec.LookPath("systemctl"); err != nil {
			return supervisorUnreachable, fmt.Errorf("systemd is present but no systemctl binary is reachable in PATH: %w", err)
		}
		// A user manager under a foreign init can be invoked ONLY through its
		// own endpoints — unlike the booted case, there is no PID-1 activation
		// path. Its marker proves the manager ran, so a dead endpoint is an
		// invocation failure (fail closed), not absence.
		if !booted && !systemdUserManagerInvocable() {
			return supervisorUnreachable, fmt.Errorf("a systemd user manager runs in this session but it is unreachable from this environment (no session bus and no private manager socket)")
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
	// user manager) — take the manager's own words. launchd's "Could not find
	// service" is deliberately NOT here: the gui domain ANSWERED that the job
	// is not loaded, which is a bootstrap repair, not a reachability failure
	// (#4475 review).
	return strings.Contains(startErr.Error(), "connect to bus")
}

// unitStartRemedy orders the remedies named in a start refusal by failure
// class: a bus-unreachable start cannot be fixed by adopt, so the session
// remedy leads; a refused or hung start leaves adopt the working verb; and a
// darwin not-loaded job is repaired by re-bootstrap, which neither session
// nor adopt performs.
func unitStartRemedy(startErr error) string {
	// On darwin "Could not find service" means the gui domain answered but
	// the plist is not loaded — a booted-out job or a reset interrupted
	// between pause and resume (#4475 review). `af daemon install`
	// re-bootstraps it; adopting or switching sessions cannot register the
	// plist from a domain that already answered.
	if autostartGOOS == "darwin" && strings.Contains(startErr.Error(), "Could not find service") {
		return "re-bootstrap the unit with `af daemon install` — the gui domain answered but the service is not loaded"
	}
	// A masked unit is a durable admin override that neither reset-failed nor
	// restart can reverse — `af daemon adopt` deterministically fails on it
	// (#4475 review). Only `systemctl unmask` lifts the mask, so it leads.
	if autostartGOOS == "linux" && strings.Contains(startErr.Error(), "is masked") {
		return fmt.Sprintf("unmask the unit (`systemctl --user unmask %s`), then run `af daemon adopt`", autostartUnitName)
	}
	if unitStartBusUnreachable(startErr) {
		return "start the unit from a session with a service manager — `af daemon adopt` cannot reach the manager from this environment either"
	}
	return "run `af daemon adopt`, or start the unit from a session with a service manager"
}

// waitForUnitDaemonReady waits out the caller's readiness budget for the
// unit-started daemon to answer, then tries ONE manager-owned reclaim before
// giving up: on linux an ALREADY-ACTIVE unit makes `start` a no-op, so when
// the daemon's process is alive but its control socket is dead or its RPC
// loop is wedged, every command fails identically and the home stays wedged
// until manual recovery (#4475 review). The reclaim is what `af daemon adopt`
// does — a restart owned by the manager — followed by a fresh readiness wait.
// launchd needs no such branch: `kickstart -k` already kills and restarts a
// running job.
//
// The reclaim gets its OWN bounded budgets rather than sharing the outer
// admission deadline: the readiness wait above can legitimately consume that
// whole window (RestartSec=5 alone outlasts the 5s budget), and an expired
// deadline handed to is-active reads an active unit as dead — so a reclaim
// drawn from `deadline` could never run for an ordinary callDaemon RPC, the
// only path that reaches it (Codex on #4475).
//
// When the wait failed because the admission budget expired, the returned
// error keeps the context.DeadlineExceeded identity so callDaemon does not
// let this refusal replace the live-upgrade lifecycle fallback it explicitly
// preserves (Codex on #4475).
func waitForUnitDaemonReady(deadline time.Time) error {
	err := waitForDaemonReady(admissionBoundedDeadline(deadline, daemonReadyTimeout))
	if err == nil {
		return nil
	}
	if autostartGOOS == "linux" && systemdUnitActive(time.Now().Add(ensureUnitStartTimeout)) {
		restartDeadline := time.Now().Add(ensureUnitStartTimeout)
		if rerr := runEnsureManagerCommand(restartDeadline, "systemctl", "--user", "restart", autostartUnitName); rerr == nil {
			if werr := waitForDaemonReady(time.Now().Add(daemonReadyTimeout)); werr == nil {
				return nil
			}
		}
	}
	if admissionDeadlineExpired(deadline) {
		err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
	}
	return err
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

// systemdUserManagerPresent reports whether a `systemd --user` manager runs
// in this session, proven by its runtime state dir <runtime>/systemd — created
// by the manager itself at startup, under the same runtime dir chain
// `systemctl --user` resolves ($XDG_RUNTIME_DIR, else /run/user/<uid>). This —
// not the session bus — is the evidence a manager exists: a standalone
// dbus-daemon or an inherited DBUS_SESSION_BUS_ADDRESS owns a bus endpoint
// with nothing systemd behind it, and taking it as presence read that foreign
// session as supervised and failed every command closed (#4475 review).
func systemdUserManagerPresent() bool {
	// Check both plausible locations: a marker under EITHER proves a manager
	// ran here — a session whose XDG_RUNTIME_DIR points elsewhere can then
	// only fail to invoke it, which reads as unreachable, not absent.
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" && isDir(filepath.Join(dir, "systemd")) {
		return true
	}
	return isDir(filepath.Join(systemdUserBusBase, strconv.Itoa(os.Getuid()), "systemd"))
}

// systemdUserManagerInvocable reports whether this process can invoke the
// systemd USER manager — resolved the way `systemctl --user` reaches it:
// DBUS_SESSION_BUS_ADDRESS, else $XDG_RUNTIME_DIR/bus, else
// /run/user/<uid>/bus, and finally the manager's own private socket at
// <runtime>/systemd/private, which systemctl connects to directly when no
// session D-Bus broker runs at all — the exact shape of a `systemd --user`
// under a foreign init (Codex on #4475). Endpoint presence is what the boot
// marker cannot show: a manager that can start the user unit even under a
// non-systemd init (#4475 review). A declared-but-dead address still counts —
// the start attempt, not the probe, is what surfaces that connect failure.
func systemdUserManagerInvocable() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	for _, dir := range []string{
		os.Getenv("XDG_RUNTIME_DIR"),
		filepath.Join(systemdUserBusBase, strconv.Itoa(os.Getuid())),
	} {
		if dir == "" {
			continue
		}
		// The private socket is the manager's own endpoint, not the session
		// broker's — systemctl --user uses it directly when no bus exists.
		if isSocket(filepath.Join(dir, "bus")) ||
			isSocket(filepath.Join(dir, "systemd", "private")) {
			return true
		}
	}
	return false
}

func isSocket(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// systemdUnitActive reports whether the manager considers the unit's process
// alive — `is-active --quiet` exits zero only for the active state, so a dead
// bus, a missing unit, or a wedged manager all read false and never trigger
// the readiness-failure reclaim (#4475 review).
func systemdUnitActive(deadline time.Time) bool {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", autostartUnitName).Run() == nil
}
