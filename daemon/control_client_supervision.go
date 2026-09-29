package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
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
//
// ensureUnitStartTimeout is a var so tests can shrink the window a hung fake
// manager burns (#4464); production never assigns it. Same seam as
// upgradeGateTimeout and autostartProbeTimeout.
var (
	ensureUnitStartTimeout = 2 * time.Second
)

const (
	ensureUnitStartWaitDelay = 250 * time.Millisecond
)

// systemdBootedDir is the marker sd_booted(3) uses for "systemd is PID 1":
// present only while systemd actually runs the system, so a container or
// foreign root that merely carries a systemctl binary still reads absent.
// A var so tests can point it at a sandbox.
var systemdBootedDir = "/run/systemd/system"

// systemdUserBusBase roots the user-manager probe: while `systemd --user`
// runs, its private socket lives at <base>/<uid>/systemd/private — present
// on hosts where a user manager exists WITHOUT systemd as PID 1 (a container
// or foreign root running `systemd --user` independently), where the boot
// marker alone would misread a live manager as absent (#4475 review). A var
// so tests can point it at a sandbox.
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
//
// goos is an explicit input rather than a read of autostartGOOS: a table test
// that can only reach the host's arm is not testing the switch (#4475
// review). Production passes autostartGOOS.
func probeUnitSupervisor(goos string) (supervisorPresence, error) {
	switch goos {
	case "linux":
		// Presence is "a manager this unit can be started through exists": the
		// sd_booted marker proves systemd-as-PID-1, and the user manager's own
		// private socket proves a live `systemd --user` under a non-systemd
		// init — the boot marker cannot prove THAT manager absent because
		// `systemctl --user` drives the user manager, not PID 1. The runtime
		// dir and any bus endpoint deliberately prove NOTHING: the dir
		// outlives a dead manager, and a foreign dbus-daemon (an OpenRC
		// session, a container inheriting DBUS_SESSION_BUS_ADDRESS) owns a
		// bus with no manager behind it (#4475 review). Neither marker
		// existing is the only proven-absent case.
		_, bootErr := os.Stat(systemdBootedDir)
		booted := bootErr == nil
		if !booted && !systemdUserManagerPresent() {
			// Only nonexistence proves the marker absent. A permission or
			// I/O error from a confined process proves nothing about whether
			// systemd is init — reading it as absence would spawn the very
			// escapee this gate exists to refuse, so it fails closed
			// (Codex on #4475).
			if !errors.Is(bootErr, os.ErrNotExist) {
				return supervisorUnreachable, fmt.Errorf("cannot determine whether systemd is init (boot marker unreadable): %w", bootErr)
			}
			return supervisorAbsent, fmt.Errorf("systemd runs neither this system nor a user manager in this session: %w", bootErr)
		}
		if _, err := exec.LookPath("systemctl"); err != nil {
			return supervisorUnreachable, fmt.Errorf("systemd is present but no systemctl binary is reachable in PATH: %w", err)
		}
		// Presence already implies invocable: the booted arm reaches the
		// system manager's activation path, and the user-manager arm was
		// proven by the private socket `systemctl --user` itself connects to.
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
		return supervisorAbsent, fmt.Errorf("daemon autostart is not supported on %s", goos)
	}
}

// unitStartBusUnreachable reports whether a failed unit start could not reach
// the manager at all — no session bus to drive it through (a user cron job, a
// system service, `su` without -l, a minimal container shell). In that class
// `af daemon adopt` cannot work either: it drives the same manager through the
// same bus and fails identically, so the refusal must lead with the remedy
// that can work.
//
// The predicate is sufficient, not exhaustive. A bus failure worded in a way
// it does not recognize (an older systemd's "Failed to get D-Bus connection",
// for example) falls through to startRefused, which is why that class still
// names the session remedy second (#4475 review).
func unitStartBusUnreachable(goos string, startErr error) bool {
	// Deterministic on linux: with neither variable set AND no live private
	// manager socket, systemctl has no address through which to reach the
	// user manager at all — no stderr parsing. The socket matters: under a
	// foreign init both variables are legitimately empty while
	// `systemctl --user` still reaches the manager through
	// <runtime>/systemd/private, so an empty env cannot classify a refusal
	// as missing-bus while that endpoint answers (Codex on #4475).
	if goos == "linux" &&
		os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" &&
		os.Getenv("XDG_RUNTIME_DIR") == "" &&
		!systemdUserManagerPresent() {
		return true
	}
	// A bus address existed but the connect still failed (stale socket, dead
	// user manager), or the env is empty while a live manager socket exists (a
	// cron job on a lingering host: systemctl does not look for that socket
	// without XDG_RUNTIME_DIR) — take the manager's own words. launchd's
	// "Could not find service" is deliberately NOT here: the gui domain
	// ANSWERED that the job is not loaded, which is a bootstrap repair, not a
	// reachability failure (#4475 review).
	return strings.Contains(startErr.Error(), "connect to bus")
}

// startFailureClass is why the bounded unit start failed. The class, not the
// raw error, decides which remedies can work and in what order (#4475
// review).
type startFailureClass uint8

const (
	// startRefused: the manager answered and refused — a start-limit hit, a
	// failing ExecStart, or any error the other classes do not recognize.
	// The manager is reachable and the problem is the unit, so adopt's
	// reset-failed + restart leads.
	startRefused startFailureClass = iota
	// startHung: the bounded start hit its deadline. That is most often a
	// manager that is busy with the unit rather than dead: through at least
	// systemd v256, service_start returns -EAGAIN while the unit waits out
	// RestartSec — "if a user does not want to wait for the holdoff time to
	// elapse, the service should be manually restarted, not started" — and
	// while it is still stopping, so the start job stays queued and the
	// unit's own RestartSec=5 alone outlasts the 2s bound. adopt's restart
	// skips the holdoff and is not held to that bound, so adopt leads. A
	// manager that hangs adopt too is genuinely wedged; the second remedy
	// covers it.
	startHung
	// startBusUnreachable: this process has no path to the manager at all.
	// adopt fails the same way, so the session remedy leads.
	startBusUnreachable
	// startMasked: a durable admin mask that neither reset-failed nor
	// restart lifts, so adopt alone deterministically fails (#4475 review).
	// The mask is af's own unit file: `systemctl --user mask` places its
	// /dev/null link at ~/.config/systemd/user/<unit> and refuses while a
	// regular file is there, and a --runtime mask is shadowed by that
	// higher-priority file (systemd v255 install.c, unit-file.c). `unmask`
	// therefore deletes the unit, so the follow-up is install, not adopt; and
	// `af daemon uninstall` refuses the link, so the unmanaged exit is the
	// unmask alone.
	startMasked
	// startNotLoaded: darwin's gui domain answered that the job is not loaded
	// — a booted-out job, or a reset interrupted between pause and resume
	// (#4475 review). Only a re-bootstrap registers the plist again.
	startNotLoaded
)

// classifyUnitStartFailure maps a failed bounded start onto its class. goos is
// an input so both platform arms are testable on every host.
func classifyUnitStartFailure(goos string, startErr error) startFailureClass {
	msg := startErr.Error()
	switch {
	case goos == "darwin" && strings.Contains(msg, "Could not find service"):
		return startNotLoaded
	case goos == "linux" && strings.Contains(msg, "is masked"):
		return startMasked
	case errors.Is(startErr, context.DeadlineExceeded):
		// Ahead of the bus predicate: a start that ran out its bound got as
		// far as waiting on the manager, while the bus predicate is an
		// inference from this process's environment.
		return startHung
	case unitStartBusUnreachable(goos, startErr):
		return startBusUnreachable
	default:
		return startRefused
	}
}

// uninstallRemedy is the escape hatch every refusal names last: an operator
// who deliberately wants this home unmanaged needs a supported exit.
const uninstallRemedy = "if this home should be unmanaged, uninstall the autostart unit with `af daemon uninstall`"

// maskedUnmanagedRemedy is the same exit for a masked unit, where
// `af daemon uninstall` refuses the mask's /dev/null link and the unmask
// already removes the only unit claiming this home.
const maskedUnmanagedRemedy = "if this home should be unmanaged, stop after the unmask, which leaves no unit claiming this home (`af daemon uninstall` refuses to remove the mask's /dev/null link)"

// managerSessionRemedy names the session that can drive the manager, and the
// command that proves a candidate session qualifies.
func managerSessionRemedy(goos string) string {
	if goos == "darwin" {
		return fmt.Sprintf("run this from a session with a service manager, such as this user's GUI login session, where `%s` answers", unitStatusDiagnostic(goos))
	}
	return fmt.Sprintf("run this from a session with a service manager, such as a login shell where `%s` answers", unitStatusDiagnostic(goos))
}

// busSessionRemedy extends managerSessionRemedy with how an unattended job
// becomes such a session. On linux the user manager listens at
// /run/user/<uid>/systemd/private, which systemctl only looks for when
// XDG_RUNTIME_DIR names that directory; lingering keeps the manager running
// with nobody logged in. The uid is rendered because a cron job has no login
// session for a bare `loginctl enable-linger` to act on.
func busSessionRemedy(goos string) string {
	if goos != "linux" {
		return managerSessionRemedy(goos)
	}
	uid := os.Getuid()
	return managerSessionRemedy(goos) + fmt.Sprintf(" (for a cron job or system service, set `XDG_RUNTIME_DIR=/run/user/%d` in its environment, and run `loginctl enable-linger %d` once so the user manager runs without a login)", uid, uid)
}

// unitStartRemedies orders the remedies a start refusal names by failure
// class: what the caller can act on first, the unmanaged-home escape hatch
// always last (#4475 review).
func unitStartRemedies(goos string, class startFailureClass) []string {
	switch class {
	case startNotLoaded:
		return []string{
			"re-bootstrap the unit with `af daemon install` (the gui domain answered that the service is not loaded, which neither adopt nor another session repairs)",
			uninstallRemedy,
		}
	case startMasked:
		return []string{
			fmt.Sprintf("lift the mask with `systemctl --user unmask %s`, then restore the unit with `af daemon install` (the mask is the unit's own file, so unmask deletes it and adopt would find no unit to hand the daemon to)", autostartUnitName),
			maskedUnmanagedRemedy,
		}
	case startBusUnreachable:
		return []string{
			busSessionRemedy(goos),
			"do not expect `af daemon adopt` to help from here (it drives the same service manager through the same missing bus and fails the same way)",
			uninstallRemedy,
		}
	case startHung:
		adopt := fmt.Sprintf("run `af daemon adopt` (its restart is not held to this command's %s start bound)", ensureUnitStartTimeout)
		if goos == "linux" {
			adopt = fmt.Sprintf("run `af daemon adopt` (its restart skips the RestartSec holdoff a queued start waits out, and is not held to this command's %s start bound)", ensureUnitStartTimeout)
		}
		return []string{
			adopt,
			"if adopt hangs too, the service manager itself is not answering: interrupt adopt, then " + managerSessionRemedy(goos),
			uninstallRemedy,
		}
	default:
		adopt := "run `af daemon adopt` (it restarts the unit through the service manager)"
		if goos == "linux" {
			adopt = "run `af daemon adopt` (it clears the unit's failed state and start-limit counter, then restarts it through the service manager)"
		}
		return []string{
			adopt,
			"if adopt fails the same way, " + busSessionRemedy(goos),
			uninstallRemedy,
		}
	}
}

// unreachableSupervisorRemedies orders the remedies for a manager that exists
// but cannot be invoked from here. adopt is never named: it runs the same
// binary from the same environment.
func unreachableSupervisorRemedies(goos string, probeErr error) []string {
	var remedies []string
	if errors.Is(probeErr, exec.ErrNotFound) {
		bin, dir := "systemctl", "/usr/bin"
		if goos == "darwin" {
			bin, dir = "launchctl", "/bin"
		}
		remedies = append(remedies, fmt.Sprintf("add the directory holding `%s` (usually %s) to this environment's PATH (`af daemon adopt` runs the same binary and fails the same way)", bin, dir))
	}
	return append(remedies, busSessionRemedy(goos), uninstallRemedy)
}

// unitReadinessRemedies is the accepted-but-silent class: the manager took
// the start, so waiting and adopt stay the working verbs.
func unitReadinessRemedies(goos string) []string {
	retry := "retry shortly, since the unit may still be starting"
	if goos == "linux" {
		retry += " (systemd holds a crashed unit's restart for RestartSec)"
	}
	return []string{
		retry,
		fmt.Sprintf("check `%s`", unitStatusDiagnostic(goos)),
		"reclaim the unit's daemon with `af daemon adopt`",
		uninstallRemedy,
	}
}

// formatRemedies renders an ordered remedy list with its order visible, so a
// reader follows the numbers rather than guessing which clause comes first.
func formatRemedies(remedies []string) string {
	var b strings.Builder
	b.WriteString("try, in order:")
	for i, r := range remedies {
		if i > 0 {
			b.WriteString(";")
		}
		fmt.Fprintf(&b, " (%d) %s", i+1, r)
	}
	return b.String()
}

// The three refusals ensureDaemonThroughUnitUntil returns instead of an
// ad-hoc spawn on a unit-claimed home (#4470). The start and readiness
// refusals wrap their cause, since callDaemon's lifecycle fallback reads
// context.DeadlineExceeded through them; the probe error stays formatted
// only, as it always was, so its stat errno can never be read as a daemon
// that is merely absent (isDaemonAbsentErr matches fs.ErrNotExist).

func unreachableSupervisorRefusal(goos string, probeErr error) error {
	return fmt.Errorf("the installed daemon service supervises this home but its service manager cannot be invoked from this environment (%v); refusing to launch an unsupervised daemon — %s", probeErr, formatRemedies(unreachableSupervisorRemedies(goos, probeErr)))
}

func unitStartRefusal(goos string, startErr error) error {
	return fmt.Errorf("the installed daemon service supervises this home but could not be started (%w); refusing to launch an unsupervised daemon — %s", startErr, formatRemedies(unitStartRemedies(goos, classifyUnitStartFailure(goos, startErr))))
}

func unitReadinessRefusal(goos string, readyErr error) error {
	return fmt.Errorf("the installed daemon service accepted the start but no daemon answered (%w); refusing to launch an unsupervised daemon — %s", readyErr, formatRemedies(unitReadinessRemedies(goos)))
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
// only path that reaches it (Codex on #4475). The matching half lives in
// callDaemon: a successful ensure that outlived the window renews the
// follow-up dial via postEnsureDialDeadline, so a repair that worked does
// not surface as a deadline error.
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
		// is-active proves the unit's process lives — not that its daemon is
		// still wedged: the last readiness poll may have just missed a socket
		// the unit bound on the deadline's edge (RestartSec=5 matches the 5s
		// wait, so a healthy daemon landing at expiry is the common shape).
		// Re-ping before letting a manager-owned restart kill a daemon that
		// is actually answering (Codex on #4475).
		if werr := waitForDaemonReady(time.Now().Add(daemonDialTimeout)); werr == nil {
			return nil
		}
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
func unitStatusDiagnostic(goos string) string {
	if goos == "darwin" {
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

// systemdUserManagerPresent reports whether a `systemd --user` manager is
// LIVE for this uid under an init that is not systemd — a container or
// foreign root running `systemd --user` independently. Liveness is a working
// connect to the manager's own `systemd/private` socket, not the `systemd/`
// runtime directory (which a failed manager leaves behind) and not the
// socket's inode (which survives an unclean exit — SIGKILL does not unlink
// it) — either remnant would otherwise read as a live manager and fail every
// command closed (Codex on #4475). The private socket is also the invocation
// path `systemctl --user` uses directly when no session broker runs, so
// presence implies invocable and no separate "present but unreachable"
// user-manager state exists. A bus socket or DBUS_SESSION_BUS_ADDRESS
// deliberately proves NOTHING: a foreign dbus-daemon owns a bus with no
// manager behind it (#4475 review).
func systemdUserManagerPresent() bool {
	for _, dir := range []string{
		os.Getenv("XDG_RUNTIME_DIR"),
		filepath.Join(systemdUserBusBase, strconv.Itoa(os.Getuid())),
	} {
		if dir == "" {
			continue
		}
		if systemdPrivateSocketLive(filepath.Join(dir, "systemd", "private")) {
			return true
		}
	}
	return false
}

// systemdPrivateSocketLive verifies a live listener owns the manager's
// private socket rather than trusting the inode type. A refused connect
// proves the remnant dead — no listener owns it; any other dial failure
// (a timeout on a saturated backlog) cannot prove death, so it still counts
// present — reading dead-while-alive would reintroduce the ad-hoc escape
// this probe guards (Codex on #4475).
func systemdPrivateSocketLive(path string) bool {
	if !isSocket(path) {
		return false
	}
	conn, err := net.DialTimeout("unix", path, daemonDialTimeout)
	if err == nil {
		_ = conn.Close()
		return true
	}
	return !errors.Is(err, syscall.ECONNREFUSED)
}

func isSocket(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
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
