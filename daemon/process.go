package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// Everything that treats the daemon as an EXTERNAL OS process lives here:
// spawning it, its PID file, stopping it gracefully, and recognizing it by
// argv. The in-process view — runDaemon's startup ordering, restore, and
// refresh — is daemon.go; the shutdown scan is sigterm_fallback.go. The
// argv-identity primitives (daemonArgs / argsHaveDaemonFlag /
// argsAreDaemonBinary / isAgentFactoryDaemon) are the shared classifier the
// PID-file path, the host-wide pgrep scan, health, and stopall must all agree
// on (#937/#1004), so they sit beside the PID-file logic they validate rather
// than beside the loop that merely uses them.

// launchDaemonProcessFn is the spawn entry point EnsureDaemon uses.
// Package-level so tests can record or suppress real daemon spawns and prove
// a bound-but-warming daemon is treated as running, never respawned (#829).
var launchDaemonProcessFn = launchDaemonProcess

func launchDaemonProcess() error {
	// Find the agent-factory binary.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	return launchDaemonProcessAt(execPath)
}

func launchDaemonProcessAt(execPath string) error {
	pid, err := startDaemonChild(execPath)
	if err != nil {
		return err
	}

	log.InfoLog.Printf("started daemon child process with PID: %d", pid)

	// The child writes its own PID file from RunDaemon (#504).
	return nil
}

// startDaemonChild starts execPath --daemon (plus any extraArgs) detached from
// the parent and returns its PID. Split from launchDaemonProcess so tests can
// spawn a short-lived stub instead of re-executing the real binary with --daemon.
// extraArgs carries the upgrade-candidate probation flag (#2212 R2); ordinary
// spawns pass none.
func startDaemonChild(execPath string, extraArgs ...string) (int, error) {
	cmd := exec.Command(execPath, append([]string{"--daemon"}, extraArgs...)...)

	// Detach the process from the parent
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Set process group to prevent signals from propagating
	cmd.SysProcAttr = getSysProcAttr()

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to start child process: %w", err)
	}

	// Setsid detaches the child's session but the kernel still parents it
	// here, so it must be reaped or each exited daemon lingers as a zombie
	// for the life of the TUI — one per upgrade/respawn cycle (#816). Same
	// pattern as session/tmux/pty.go.
	go func() {
		_ = cmd.Wait()
	}()

	return cmd.Process.Pid, nil
}

// daemonPIDFilePath returns the path to the daemon PID file, or "" if the
// config dir cannot be resolved.
func daemonPIDFilePath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.pid"), nil
}

// writeDaemonPIDFile atomically writes the current process's PID to the daemon
// PID file with mode 0600. Used by RunDaemon so callers (StopDaemon, the
// SIGTERM fallback in RequestShutdown) can locate and signal this daemon.
//
// It REFUSES a symlinked path (#3672). The PID file is af's own liveness
// bookkeeping at a path af chose, written on start and deleted on teardown, so
// a link there is neither af's to write through nor af's to replace — the same
// answer the bearer token and the autostart unit take. It takes the PID-file
// lock (withDaemonPIDLock) so a stop's read-compare-unlink can't interleave.
func writeDaemonPIDFile() error {
	path, err := daemonPIDFilePath()
	if err != nil {
		return err
	}
	return withDaemonPIDLock(path, time.Now().Add(daemonPIDLockStartupBudget), func() error {
		return config.AtomicWriteFileRefusingLink(path, []byte(strconv.Itoa(os.Getpid())), 0600)
	})
}

// removeDaemonPIDFile deletes the daemon PID file. Best-effort: an ENOENT is
// already harmless (a stale file is fine — readers verify cmdline) and
// permission errors only occur in pathological setups. Logs at warning level
// rather than failing the daemon teardown.
//
// It refuses a symlinked path because the write above refuses one: af cannot
// have written this file through a link, so unlinking one on teardown would
// delete an arrangement af never touched (#3672).
func removeDaemonPIDFile() {
	path, err := daemonPIDFilePath()
	if err != nil {
		return
	}
	if err := config.RemoveFileRefusingLink(path); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to remove daemon PID file: %v", err)
	}
}

// removeStaleDaemonPIDFile removes a stale daemon PID file from an af-managed
// path. Used by stopDaemonUntil's stale-PID branches — the entry-time janitor
// that fires when the PID file's contents look stale, NOT the teardown of a
// PID the daemon wrote — so it is the stop-side sibling to writeDaemonPIDFile
// and the reach asymmetric with removeDaemonPIDFile (which fires on the
// daemon's own SIGTERM teardown of a PID file af WROTE).
//
// It uses config.RemoveFileRefusingLink for the same reason writeDaemonPIDFile
// and removeDaemonPIDFile do (#3672): writeDaemonPIDFile refuses to write
// through a link, so af cannot have authored this file — unlinking one here
// would delete an arrangement af never touched. The four stale-PID branches
// used to bypass this with a bare os.Remove, unlinks the link while its target
// kept whatever the user planted — the asymmetry RemoveFileRefusingLink exists
// to prevent on the autostart teardown and the daemon teardown, and the one
// place the PID file's stop-side cleanup leaked through os.Remove.
//
// Logs the symlink refusal and unexpected removal errors; a successful removal
// or an already-absent file are silent (the caller has already logged why the
// PID looked stale). The caller's "removing stale file"-style prior log line
// was dropped because on the refused-symlink case that line claimed an action
// that did not happen (#3672): the refusal log here is what stays truthful.
func removeStaleDaemonPIDFile(pidFile string, pid int) {
	if err := config.RemoveFileRefusingLink(pidFile); err != nil {
		if errors.Is(err, config.ErrManagedFileSymlink) {
			log.InfoLog.Printf("daemon PID file (PID: %d) is a symlink af did not write through; leaving it in place", pid)
		} else if !os.IsNotExist(err) {
			log.WarningLog.Printf("failed to remove stale PID file: %v", err)
		}
	}
}

// stopDaemonGrace bounds how long StopDaemon waits for a SIGTERM'd daemon to
// exit before escalating to SIGKILL. stopDaemonPoll is the polling cadence.
// Package vars rather than constants so tests can shorten them. Production
// defaults mirror sigtermFallbackGrace / sigtermFallbackPoll — the same
// timings already used by signalAndWait on the upgrade fallback path.
var (
	stopDaemonGrace = sigtermFallbackGrace
	stopDaemonPoll  = sigtermFallbackPoll
)

// stopDaemonPIDLockBudget bounds how long the foreign/unverifiable PID-file
// cleanup waits on the sidecar daemon.pid.lock when the caller carries no
// deadline — public StopDaemon passes a zero deadline, so without a floor the
// cleanup's withDaemonPIDLock would block forever in LOCK_EX on a writer
// suspended or stalled on daemon.pid.lock, hanging StopDaemon (and with it
// upgrade recovery and autostart handoff). A finite budget abandons the
// best-effort cleanup instead, mirroring the bounded startup write
// (daemonPIDLockStartupBudget). Package var so tests can shorten it; the
// cleanup is best-effort, so a lock this budget cannot acquire is dropped (the
// stale PID file is left; a later caller re-reads and re-checks it).
var stopDaemonPIDLockBudget = daemonPIDLockStartupBudget

// pidLockCleanupDeadline returns the deadline to pass to the PID-file lock the
// foreign/unverifiable cleanup paths take. A deadline-bounded stopDaemonUntil
// caller threads its own deadline through; a zero deadline (public StopDaemon,
// which carries none) is floored at stopDaemonPIDLockBudget so the lock
// acquisition cannot block indefinitely on a writer suspended or stalled on
// daemon.pid.lock (withDaemonPIDLock blocks forever on a zero deadline).
func pidLockCleanupDeadline(deadline time.Time) time.Time {
	if !deadline.IsZero() {
		return deadline
	}
	return time.Now().Add(stopDaemonPIDLockBudget)
}

// StopDaemon attempts to stop a running daemon process if it exists. The bool
// return reports whether a live agent-factory daemon was actually signaled: it
// is false (with a nil error) when there was nothing to stop — no PID file, an
// invalid/stale PID, a dead process, or a PID that doesn't look like an
// agent-factory daemon. Callers that surface a user-facing "stopped" message
// must gate on it (#937): a daemon predating the PID file (pre-1.0.69) leaves
// no daemon.pid, so a true success line here would be a lie. It verifies the
// PID actually belongs to an agent-factory daemon before signaling it, so a
// stale or reused PID in the PID file can't take down an unrelated process.
// That cmdline check is paired with a home binding (pidBelongsToThisHome): a
// stale daemon.pid whose PID was recycled by ANOTHER AGENT_FACTORY_HOME's
// `af --daemon` passes the cmdline check while serving a different control
// socket, and signaling it would kill an unrelated daemon — possibly another
// user's on a shared host. The same binding gates locateDaemonPID so the two
// PID-validation paths agree (#1004), mirroring the cross-home gate the unit
// operations already carry (#1919).
//
// Shutdown is graceful by default: SIGTERM gives the daemon's signal handler a
// chance to run SaveInstances() and clean up the PID file (see RunDaemon). We
// only escalate to SIGKILL if the daemon does not exit within stopDaemonGrace,
// matching the SIGTERM-first pattern in signalAndWait (#571).
func StopDaemon() (bool, error) {
	return stopDaemonUntil(time.Time{})
}

// stopDaemonUntil applies an optional caller deadline to the graceful-exit
// poll. When that earlier deadline expires after SIGTERM, it returns without
// escalating to SIGKILL; a deadline-bounded EnsureDaemon caller will stop the
// launch path rather than start a replacement while the old process may still
// be releasing its singleton lock.
func stopDaemonUntil(deadline time.Time) (bool, error) {
	if admissionDeadlineExpired(deadline) {
		return false, daemonAdmissionDeadlineError()
	}
	pidDir, err := config.GetConfigDir()
	if err != nil {
		return false, fmt.Errorf("failed to get config directory: %w", err)
	}

	pidFile := filepath.Join(pidDir, "daemon.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read PID file: %w", err)
	}

	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return false, fmt.Errorf("invalid PID file format: %w", err)
	}

	// Defensively refuse to kill our own process or obviously invalid PIDs.
	if pid <= 1 || pid == os.Getpid() {
		log.InfoLog.Printf("daemon PID file contained invalid PID %d", pid)
		removeStaleDaemonPIDFile(pidFile, pid)
		return false, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		// On unix, FindProcess never returns an error, but handle it defensively anyway.
		log.InfoLog.Printf("daemon process (PID: %d) not found", pid)
		removeStaleDaemonPIDFile(pidFile, pid)
		return false, nil
	}

	// Check the process exists at all. Signal 0 is a no-op that just validates permissions/existence.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		log.InfoLog.Printf("daemon process (PID: %d) is not running (%v)", pid, err)
		removeStaleDaemonPIDFile(pidFile, pid)
		return false, nil
	}

	// Verify the process is actually an agent-factory daemon before signaling it. If we can't verify,
	// err on the side of caution and treat the PID file as stale rather than signaling a random process.
	if !isAgentFactoryDaemon(pid) {
		log.InfoLog.Printf("PID %d does not look like an agent-factory daemon", pid)
		removeStaleDaemonPIDFile(pidFile, pid)
		return false, nil
	}

	// Bind the PID to THIS home before signaling (#4793, #1919, #1004). The
	// cmdline check above cannot tell this home's `af --daemon` from another
	// home's. A PROVEN-foreign PID is a stale PID file; an INCONCLUSIVE binding
	// (daemonUnverifiable) is neither safe to signal nor safe to orphan by
	// deleting the PID file over it — leave the file and surface why.
	switch scope := classifyDaemonHome(pid); scope {
	case daemonOurs:
		// Proven to serve this home: signal it below.
	case daemonForeign:
		log.InfoLog.Printf("PID %d is not this home's agent-factory daemon; removing stale PID file", pid)
		removePIDFileIfStillNames(pidFile, pid, pidLockCleanupDeadline(deadline))
		return false, nil
	default: // daemonUnverifiable — inconclusive; neither signal nor orphan a live daemon.
		if reclaimDeadUnverifiablePIDFile(pidFile, pid, pidLockCleanupDeadline(deadline)) {
			return false, nil
		}
		return false, fmt.Errorf("PID %d could not be bound to this home (uid, AGENT_FACTORY_HOME, or path unresolved); not signaling and leaving the PID file in place", pid)
	}

	// Send SIGTERM so the daemon's signal handler can SaveInstances() before
	// exit (#571). A race where the daemon exits between the signal-0 probe
	// above and this call is benign: errIsProcessGone covers both ESRCH and
	// the os.ErrProcessDone surface.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errIsProcessGone(err) {
			log.InfoLog.Printf("daemon process (PID: %d) exited before SIGTERM landed; cleaning up", pid)
			cleanupDaemonRuntimeFiles(pidFile, deadline)
			return true, nil
		}
		return false, fmt.Errorf("failed to signal daemon process: %w", err)
	}

	// Poll for graceful exit.
	gracefulDeadline := admissionBoundedDeadline(deadline, stopDaemonGrace)
	exited := false
	for time.Now().Before(gracefulDeadline) {
		if !pidLooksAlive(pid) {
			exited = true
			break
		}
		if !waitUntilAdmissionDeadline(gracefulDeadline, stopDaemonPoll) {
			break
		}
	}

	if exited {
		log.InfoLog.Printf("daemon process (PID: %d) exited gracefully after SIGTERM", pid)
	} else if admissionDeadlineExpired(deadline) {
		return true, daemonAdmissionDeadlineError()
	} else {
		log.WarningLog.Printf("daemon process (PID: %d) did not exit within %s of SIGTERM; escalating to SIGKILL", pid, stopDaemonGrace)
		if err := proc.Signal(syscall.SIGKILL); err != nil && !errIsProcessGone(err) {
			return false, fmt.Errorf("failed to stop daemon process: %w", err)
		}
	}

	cleanupDaemonRuntimeFiles(pidFile, deadline)
	log.InfoLog.Printf("daemon process (PID: %d) stopped successfully", pid)
	return true, nil
}

// isAgentFactoryDaemon checks whether the process at pid looks like an agent-factory daemon:
// its argv must carry the --daemon flag as a discrete argument AND its executable must be an
// agent-factory binary ("af" or "agent-factory"). It reads the process argv with argument
// boundaries preserved (see daemonArgs); if no readable argv is available, returns false so
// callers treat the PID as unverified.
//
// Both checks are required so that a stale PID file whose PID has been reused by an unrelated
// process carrying a "--daemon" token (e.g. "sleep --daemon af-test") is not mistaken for our
// daemon and signaled by StopDaemon/locateDaemonPID. This mirrors the host-wide pgrep scan in
// sigterm_fallback.go, which also requires both argsHaveDaemonFlag and argsAreDaemonBinary;
// the two PID-validation paths must agree (#1004).
//
// Detection operates on real argv elements (not a space-joined string), so a binary installed
// under a path containing spaces — e.g. "/home/John Smith/.local/bin/af" — is classified
// correctly instead of having its path shredded across argv boundaries (#1214). We still require
// an exact "--daemon" token (or the "--daemon=..." form), so flags like --daemonize never match.
func isAgentFactoryDaemon(pid int) bool {
	args := daemonArgs(pid)
	if len(args) == 0 {
		return false
	}
	return argsHaveDaemonFlag(args) && argsAreDaemonBinary(args)
}

// argsHaveDaemonFlag reports whether argv contains "--daemon" as a discrete argument (either bare
// or in the "--daemon=value" form). It deliberately rejects substring matches like "--daemonize"
// or "--daemon-mode". Because it scans real argv elements, spaces inside another argument (such as
// a spaced binary path in argv[0]) can never fabricate or hide a "--daemon" token (#1214).
func argsHaveDaemonFlag(args []string) bool {
	for _, a := range args {
		if a == "--daemon" {
			return true
		}
		value, ok := strings.CutPrefix(a, "--daemon=")
		if !ok {
			continue
		}
		// `--daemon=false` is a client saying, explicitly, that it is NOT a
		// daemon. Matching the prefix and calling it one made every such client
		// a daemon to every caller here: doctor counted it as a duplicate, the
		// host scan offered it for a kill, and the #1004 pid guard would have
		// accepted it as ours. The flag's VALUE is the answer; its name is only
		// where the answer lives.
		//
		// Only an explicitly FALSE value flips the answer. An unparseable one
		// ("--daemon=foo") keeps the long-standing "the form is present, so treat
		// it as a daemon flag" reading that TestArgsHaveDaemonFlag has pinned
		// since #342: that case is about recognizing the `--daemon=` FORM (as
		// against `--daemonize`), and its value is a placeholder, not a boolean.
		// It is also unobservable in practice — cobra rejects a non-boolean here,
		// so no such process is ever live to classify — and narrowing a seam that
		// gates signals on a hypothetical is not worth the blast radius.
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return true
		}
		return enabled
	}
	return false
}

// argsAreDaemonBinary reports whether argv[0] is an agent-factory daemon binary: installed as "af"
// or built from source (`go build .`) as "agent-factory". The host-wide pgrep scan in
// sigterm_fallback.go matches any process carrying a "--daemon" token, so this restores the
// binary-name specificity that the old "af --daemon" substring pattern provided — while still
// catching source-built `agent-factory --daemon` daemons that the old pattern missed (#937).
//
// argv[0] is a single argv element, so filepath.Base sees the whole executable path even when it
// contains spaces (e.g. "/home/John Smith/.local/bin/af" → base "af"). The previous
// implementation space-joined the argv and re-split on whitespace, which turned that same path
// into base "John" and made every spaced-install daemon undetectable (#1214).
func argsAreDaemonBinary(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch filepath.Base(args[0]) {
	case "af", "agent-factory":
		return true
	default:
		return false
	}
}

// daemonArgs returns the argv of pid with argument BOUNDARIES preserved, or nil when no argv is
// readable (a foreign user's process, a zombie, a kernel thread).
//
// The boundaries are the whole contract. This classifies a process by its binary name
// (argsAreDaemonBinary), so an install path containing a space must arrive as ONE element:
// "/Users/John Smith/.local/bin/af" has base "af", while the same path re-split on whitespace has
// base "John" and no longer looks like a daemon at all (#1214).
//
// It used to prefer /proc and fall back to `ps -p <pid> -o args=`, whose output is already
// space-joined — so the fallback could not recover the boundaries it needed and the code said so
// in a comment: spaced-install detection was "only fully reliable where /proc exists". That
// caveat was a live bug wearing a disclaimer, and it was worst exactly where it was untested:
// spaces in paths are ordinary on macOS (/Users/First Last, /Volumes/Macintosh HD, ~/Library/
// Application Support) and rare on Linux. proctree.Argv now reads real argv on both platforms
// (/proc/<pid>/cmdline on Linux, KERN_PROCARGS2 on darwin), so there is no lossy path left to
// fall back to and the caveat is retired (#1942).
func daemonArgs(pid int) []string {
	return proctree.Argv(pid)
}
