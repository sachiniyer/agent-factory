package daemon

import (
	"errors"
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
//  2. Otherwise (or if the PID file is missing), scan with `pgrep -f --
//     '--daemon'`, keep only processes whose binary is `af` or
//     `agent-factory` (#937 — source builds run under the latter name),
//     filter out /tmp/Test* paths (Go test binaries) and the current
//     process, and require exactly one candidate.
//
// Returns ShutdownViaSIGTERM when a signal was delivered, or ShutdownFailed
// with an actionable error when the daemon (which is provably running — the
// caller only invokes us after the Shutdown RPC returned method-not-found,
// not ECONNREFUSED) could not be located or signaled. Returning
// ShutdownNoDaemon here would contradict the established state and silently
// leave the stale daemon running (#553).
func sigtermFallback() (ShutdownResult, error) {
	pid, source, err := locateDaemonPID()
	if err != nil {
		return ShutdownFailed, fmt.Errorf(
			"sigterm fallback failed: %w; run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			err,
		)
	}
	if pid == 0 {
		return ShutdownFailed, fmt.Errorf(
			"sigterm fallback: daemon is running on the control socket but no PID candidate was found (%s); "+
				"run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			source,
		)
	}

	log.InfoLog.Printf("sigterm fallback: signaling pre-#501 daemon (pid=%d source=%s)", pid, source)
	if err := signalAndWait(pid); err != nil {
		return ShutdownFailed, fmt.Errorf(
			"sigterm fallback for daemon pid %d: %w; run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			pid, err,
		)
	}

	// Best-effort PID file cleanup so the next `af` invocation does not see
	// a stale file. StopDaemon does this on its happy path too; doing it
	// here keeps state tidy when the daemon binary never wrote one itself.
	removeDaemonPIDFile()
	return ShutdownViaSIGTERM, nil
}

// locateDaemonPID returns the PID of the running daemon to signal and the
// source it was found in ("pid-file" or "pgrep") on success. On failure to
// locate a PID, returns (0, source, nil) where source describes the suspected
// PID source for diagnostics (e.g. "pid-file pid=N stale, pgrep: no matches"
// or "no pid-file, pgrep unavailable"). An error is returned only for hard
// failures (ambiguous pgrep results, pgrep itself failing to execute).
func locateDaemonPID() (int, string, error) {
	pidFileSource := "no pid-file"
	if pid, ok := readPIDFromFile(); ok {
		if pidLooksAlive(pid) && isAgentFactoryDaemon(pid) {
			return pid, "pid-file", nil
		}
		log.InfoLog.Printf("sigterm fallback: PID file pid=%d is dead or non-daemon; falling back to pgrep", pid)
		pidFileSource = fmt.Sprintf("pid-file pid=%d stale", pid)
	}

	pids, err := scanDaemonCandidatesFn()
	if err != nil {
		if errors.Is(err, errPgrepUnavailable) {
			return 0, fmt.Sprintf("%s, pgrep unavailable", pidFileSource), nil
		}
		return 0, "", fmt.Errorf("%s, pgrep: %w", pidFileSource, err)
	}
	switch len(pids) {
	case 0:
		return 0, fmt.Sprintf("%s, pgrep: no matches", pidFileSource), nil
	case 1:
		return pids[0], "pgrep", nil
	default:
		return 0, "", fmt.Errorf(
			"sigterm fallback: ambiguous, found %d `--daemon` processes (%s) — "+
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
// On platforms without /proc (macOS), the cmdline read below returns "" and we
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

// scanDaemonCandidatesFn is the process-scan entry point used by
// locateDaemonPID. It is a function var so tests can substitute a controlled
// candidate list: the real pgrep scan is host-wide, so on any machine running
// the supervised daemon (`af daemon install`, the recommended setup since
// #791) it finds that unrelated process and the stale-PID / no-candidates
// branches become unreachable — and worse, a test exercising the fallback
// could SIGTERM the host's real daemon (#793).
var scanDaemonCandidatesFn = pgrepDaemonCandidates

// errPgrepUnavailable signals that `pgrep` is not on PATH, distinct from
// "pgrep ran and returned no matches". Surfaced by locateDaemonPID so the
// caller can build an actionable error rather than misclassifying the running
// daemon as absent (#553).
var errPgrepUnavailable = errors.New("pgrep not found in PATH")

// pgrepDaemonCandidates scans for processes whose full command line carries a
// "--daemon" token, then keeps only those whose binary is `af` or
// `agent-factory`. The `--` before the pattern stops pgrep from treating the
// leading-dash pattern as a flag. We match the bare `--daemon` token rather
// than "af --daemon" so source-built daemons running as `agent-factory
// --daemon` are found too (#937); argsAreDaemonBinary restores the
// binary-name specificity the old substring gave. Go test binaries living
// under /tmp/Test* and the current process are excluded. We rely on pgrep -f
// rather than parsing /proc directly so the path works on both Linux and macOS.
func pgrepDaemonCandidates() ([]int, error) {
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		log.WarningLog.Printf("sigterm fallback: pgrep not found in PATH; cannot scan for daemons: %v", err)
		return nil, fmt.Errorf("%w: %v", errPgrepUnavailable, err)
	}
	out, err := exec.Command(pgrep, "-f", "--", "--daemon").Output()
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
		// Defensive: re-read the argv (boundaries preserved, so a spaced binary
		// path in argv[0] is classified correctly — #1214) and require it to (a)
		// contain "--daemon" as a discrete token (pgrep -f does substring
		// matching, not token matching), (b) belong to an `af`/`agent-factory`
		// binary so the broad `--daemon` pattern can't match an unrelated daemon
		// (#937), and (c) not be a Go test binary (these live under /tmp/Test...
		// when invoked from `go test`).
		args := daemonArgs(pid)
		if len(args) == 0 {
			continue
		}
		if !argsHaveDaemonFlag(args) {
			continue
		}
		if !argsAreDaemonBinary(args) {
			continue
		}
		if isTestBinaryArgs(args) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// isTestBinaryArgs filters out Go test binaries spawned during `go test`.
// They typically live under /tmp/Test<name>... (t.TempDir paths) or
// /tmp/go-build... (compiled-test cache). We don't want a test that spawns a
// fake "af --daemon" subprocess to accidentally claim a real daemon match
// when run in parallel with another test.
func isTestBinaryArgs(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "/tmp/Test") || strings.HasPrefix(a, "/tmp/go-build") {
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
