package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// sigtermFallback locates a running pre-#501 daemon and sends it SIGTERM,
// escalating to SIGKILL after sigtermFallbackGrace. Used when the Shutdown
// RPC returned method-not-found (the daemon is listening on the control
// socket but the binary predates #501).
//
// Strategy:
//  1. If ~/.config/agent-factory/daemon.pid exists, parse it. Verify the PID
//     is alive AND its command line contains "--daemon" as a discrete token
//     (defensive against PID reuse — the file may be stale).
//  2. Otherwise (or if the PID file is missing), scan with `pgrep -f
//     "af --daemon"`, filter out /tmp/Test* paths (Go test binaries) and the
//     current process, and require exactly one candidate.
//
// Returns ShutdownViaSIGTERM when a signal was delivered, ShutdownNoDaemon
// when nothing eligible was found, or an error for ambiguous cases (multiple
// pgrep matches) so the caller can surface a clear actionable message.
func sigtermFallback() (ShutdownResult, error) {
	pid, source, err := locateDaemonPID()
	if err != nil {
		return ShutdownNoDaemon, err
	}
	if pid == 0 {
		// Nothing to signal. Treat as ShutdownNoDaemon: the upgrade prints
		// bare success and the next RPC EnsureDaemon-respawns from the new
		// binary anyway.
		return ShutdownNoDaemon, nil
	}

	log.InfoLog.Printf("sigterm fallback: signaling pre-#501 daemon (pid=%d source=%s)", pid, source)
	if err := signalAndWait(pid); err != nil {
		return ShutdownNoDaemon, fmt.Errorf("sigterm fallback for daemon pid %d: %w", pid, err)
	}

	// Best-effort PID file cleanup so the next `af` invocation does not see
	// a stale file. StopDaemon does this on its happy path too; doing it
	// here keeps state tidy when the daemon binary never wrote one itself.
	removeDaemonPIDFile()
	return ShutdownViaSIGTERM, nil
}

// locateDaemonPID returns the PID of the running daemon to signal, the source
// it was found in ("pid-file" or "pgrep") for diagnostics, or (0, "", nil)
// when no eligible target exists. An error is returned only for ambiguous
// pgrep results where signaling would be unsafe.
func locateDaemonPID() (int, string, error) {
	if pid, ok := readPIDFromFile(); ok {
		if pidLooksAlive(pid) && isAgentFactoryDaemon(pid) {
			return pid, "pid-file", nil
		}
		log.InfoLog.Printf("sigterm fallback: PID file pid=%d is dead or non-daemon; falling back to pgrep", pid)
	}

	pids, err := pgrepDaemonCandidates()
	if err != nil {
		return 0, "", err
	}
	switch len(pids) {
	case 0:
		return 0, "", nil
	case 1:
		return pids[0], "pgrep", nil
	default:
		return 0, "", fmt.Errorf(
			"sigterm fallback: ambiguous, found %d `af --daemon` processes (%s) — "+
				"kill the right one manually then re-run `af upgrade`",
			len(pids), formatPIDList(pids),
		)
	}
}

// readPIDFromFile parses the daemon PID file. Returns (0, false) when the
// file is missing, malformed, or points at an obviously bogus PID. A stale
// or reused PID is not filtered here — callers re-verify with cmdline.
func readPIDFromFile() (int, bool) {
	path, err := daemonPIDFilePath()
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return 0, false
	}
	return pid, true
}

// pidLooksAlive returns true when signal 0 to pid succeeds AND the kernel
// still has user-space state for the process (cmdline is non-empty). The
// cmdline check is the cheap Linux-side way to filter out zombies: once a
// process has exited but not yet been reaped, /proc/<pid>/cmdline is empty,
// but kill(pid, 0) still succeeds because the process entry exists. Without
// the second check, signalAndWait would wait the full sigtermFallbackGrace
// for any zombie before escalating to SIGKILL — visible as a 5s pause in
// `af upgrade` when the dying daemon's parent isn't waiting.
//
// On platforms without /proc (macOS), readProcCmdline returns "" and we
// can't distinguish zombie from "kernel doesn't expose the cmdline"; we
// fall back to the signal-0 result. The cost there is the 5s grace, which
// is correct but slow.
func pidLooksAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	if _, err := os.Stat("/proc"); err == nil {
		// /proc is mounted (Linux). An empty cmdline means the task is a
		// zombie or kernel thread; for our purposes either is "dead".
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err == nil && len(strings.TrimRight(string(data), "\x00")) == 0 {
			return false
		}
	}
	return true
}

// pgrepDaemonCandidates scans for processes whose full command line contains
// "af --daemon", excluding Go test binaries living under /tmp/Test* and the
// current process. We rely on pgrep -f rather than parsing /proc directly so
// the path works on both Linux and macOS.
func pgrepDaemonCandidates() ([]int, error) {
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		// No pgrep is unusual but recoverable: just report no candidates so
		// the upgrade falls back to "bare success". The daemon will keep
		// running but the next RPC respawns the right one anyway, and we
		// surface this in the log for debugging.
		log.WarningLog.Printf("sigterm fallback: pgrep not found in PATH; cannot scan for daemons: %v", err)
		return nil, nil
	}
	out, err := exec.Command(pgrep, "-f", "af --daemon").Output()
	if err != nil {
		// pgrep exits 1 when there are no matches. Treat that as zero
		// candidates rather than an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep failed: %w", err)
	}

	self := os.Getpid()
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, parseErr := strconv.Atoi(line)
		if parseErr != nil {
			continue
		}
		if pid == self {
			continue
		}
		// Defensive: re-read the cmdline and require it to (a) contain
		// "--daemon" as a discrete token (pgrep -f does substring matching,
		// not token matching) and (b) not be a Go test binary (these live
		// under /tmp/Test... when invoked from `go test`).
		cmdline := readProcCmdline(pid)
		if cmdline == "" {
			cmdline = readPsArgs(pid)
		}
		if cmdline == "" {
			continue
		}
		if !cmdlineHasDaemonFlag(cmdline) {
			continue
		}
		if isTestBinaryCmdline(cmdline) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// isTestBinaryCmdline filters out Go test binaries spawned during `go test`.
// They typically live under /tmp/Test<name>... (t.TempDir paths) or
// /tmp/go-build... (compiled-test cache). We don't want a test that spawns a
// fake "af --daemon" subprocess to accidentally claim a real daemon match
// when run in parallel with another test.
func isTestBinaryCmdline(cmdline string) bool {
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, "/tmp/Test") || strings.HasPrefix(field, "/tmp/go-build") {
			return true
		}
	}
	return false
}

// signalAndWait sends SIGTERM to pid, polls for exit up to
// sigtermFallbackGrace, and escalates to SIGKILL if it has not exited.
func signalAndWait(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("FindProcess: %w", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errIsProcessGone(err) {
			return nil
		}
		return fmt.Errorf("SIGTERM: %w", err)
	}

	deadline := time.Now().Add(sigtermFallbackGrace)
	for time.Now().Before(deadline) {
		if !pidLooksAlive(pid) {
			return nil
		}
		time.Sleep(sigtermFallbackPoll)
	}

	log.WarningLog.Printf("sigterm fallback: pid %d did not exit within %s; escalating to SIGKILL", pid, sigtermFallbackGrace)
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errIsProcessGone(err) {
		return fmt.Errorf("SIGKILL: %w", err)
	}
	return nil
}

// errIsProcessGone reports whether err from Signal indicates the target is
// already gone. POSIX returns ESRCH; os.Process surfaces this as "os: process
// already finished".
func errIsProcessGone(err error) bool {
	if err == nil {
		return false
	}
	if err == os.ErrProcessDone {
		return true
	}
	return strings.Contains(err.Error(), "process already finished") ||
		strings.Contains(err.Error(), "no such process")
}

// formatPIDList renders []int as a comma-separated list for user-facing
// error messages.
func formatPIDList(pids []int) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}
