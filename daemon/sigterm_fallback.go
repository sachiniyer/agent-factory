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

// sigtermFallback locates a running pre-#501 daemon and sends it SIGTERM,
// escalating to SIGKILL after sigtermFallbackGrace. Used when the Shutdown
// RPC returned method-not-found (the daemon is listening on the control
// socket but the binary predates #501).
//
// Strategy:
//  1. If ~/.agent-factory/daemon.pid exists, parse it. Verify the PID is
//     alive, its command line contains "--daemon" as a discrete token,
//     AND it belongs to THIS caller's AGENT_FACTORY_HOME. The cmdline check
//     alone cannot tell an `af --daemon` of this home from one of another
//     home, so a stale PID file whose PID was recycled by another home's
//     `af --daemon` would pass it while serving someone else's control
//     socket; the home binding (pidBelongsToThisHome) is what keeps the
//     fallback from SIGTERM-ing the wrong daemon.
//  2. Otherwise (or if the PID file is missing/stale/foreign), scan with
//     `pgrep -f -- '--daemon'`, keep only processes whose binary is `af`
//     or `agent-factory` (#937 — source builds run under the latter name),
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
		if pidLooksAlive(pid) && isAgentFactoryDaemon(pid) && pidBelongsToThisHome(pid) {
			return pid, "pid-file", nil
		}
		log.InfoLog.Printf("sigterm fallback: PID file pid=%d is dead, non-daemon, or not this home's daemon; falling back to pgrep", pid)
		pidFileSource = fmt.Sprintf("pid-file pid=%d stale", pid)
	}

	pids, err := scanDaemonCandidatesFn()
	if err != nil {
		if errors.Is(err, errPgrepUnavailable) {
			return 0, fmt.Sprintf("%s, pgrep unavailable", pidFileSource), nil
		}
		return 0, "", fmt.Errorf("%s, pgrep: %w", pidFileSource, err)
	}
	// Reclassify every scanned candidate by uid and AGENT_FACTORY_HOME before
	// selecting a signal target. The pgrep scan returns every `--daemon`
	// process on the host, so a single result that serves ANOTHER home (or
	// whose home is unverifiable) would otherwise be signalled — exactly the
	// unrelated process the PID-file binding just rejected, whenever it was
	// the only scan match. Filtering to this home's proven daemons also turns
	// the old false "ambiguous" refusal into the correct single target when
	// other homes' daemons are alive alongside ours (#4793). The PID-file fast
	// path above and this scan path now both require the home binding, so the
	// two agree on what is signalled (#1004).
	scoped := make([]int, 0, len(pids))
	for _, pid := range pids {
		if pidBelongsToThisHome(pid) {
			scoped = append(scoped, pid)
		}
	}
	switch len(scoped) {
	case 0:
		return 0, fmt.Sprintf("%s, pgrep: no matches for this home (%d scanned)", pidFileSource, len(pids)), nil
	case 1:
		return scoped[0], "pgrep", nil
	default:
		return 0, "", fmt.Errorf(
			"sigterm fallback: ambiguous, found %d `--daemon` processes for this home (%s) — "+
				"kill the right one manually then re-run `af upgrade`",
			len(scoped), formatPIDList(scoped),
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

// removePIDFileIfStillNames unlinks pidFile only when it still records pid.
// stopDaemonUntil read a stale foreign PID and proved the process it names is
// not this home's daemon; in the window between that read and this unlink a
// same-home daemon may have started and atomically rewritten daemon.pid with
// its own PID. Removing the file unconditionally would delete that valid
// replacement and recreate the untracked-daemon state the PID file exists to
// prevent — the new daemon would be live but no longer discoverable by
// StopDaemon. Re-read and compare first; leave a freshly-written valid file to
// its owner, and treat the unreadable/malformed case the same way rather than
// unlinking a file whose current contents we did not establish (#4793).
func removePIDFileIfStillNames(pidFile string, pid int) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		// Already gone (or unreadable) — nothing to remove; a missing file is
		// the desired end state, and a permission error is no worse than the
		// previous unconditional os.Remove would have been.
		return
	}
	var current int
	if _, err := fmt.Sscanf(string(data), "%d", &current); err != nil {
		// Malformed, and therefore not the foreign PID we read. A
		// newly-started daemon writes a valid PID, so this is neither the
		// stale file we own nor safe to claim — leave it for the next caller.
		return
	}
	if current != pid {
		return // a new daemon has written its own PID; keep the file
	}
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to remove stale daemon PID file %q: %v", pidFile, err)
	}
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

// pidBelongsToThisHome reports whether the process at pid is an agent-factory
// daemon serving THIS process's AGENT_FACTORY_HOME. It is the home-binding half
// of the trust decision a PID-file candidate must pass before it is signalled:
// cmdline alone (isAgentFactoryDaemon) cannot distinguish an `af --daemon` of
// THIS home from one of another home, and a stale daemon.pid whose PID the
// kernel recycled onto a DIFFERENT home's `af --daemon` passes the cmdline check
// while serving someone else's control socket. Signaling it would kill an
// unrelated daemon — possibly another user's on a shared host — so every path
// that reads daemon.pid and signals the PID it names must require this in
// addition to the cmdline match.
//
// It is a thin bool over classifyDaemonHome, the classifier StopDaemon and the
// pgrep scan path share so the two PID-validation paths agree on what "ours"
// means (#1004). "Unverifiable" (a uid, environ, or path that could not be
// established) is NOT ours: a PID that cannot be bound to this home is never
// trusted — guessing "ours" is exactly the bug. Callers that must act
// differently on a PROVEN-foreign PID and an INCONCLUSIVE one use
// classifyDaemonHome directly; see stopDaemonUntil.
func pidBelongsToThisHome(pid int) bool {
	return classifyDaemonHome(pid) == daemonOurs
}

// classifyDaemonHome decides whether the process at pid is a daemon serving
// THIS process's AGENT_FACTORY_HOME, surfacing the inconclusive case
// (daemonUnverifiable) the bool helper collapses. StopDaemon reads daemon.pid
// and then decides whether to signal the PID it names: proving the PID is
// another home's daemon (daemonForeign) lets it treat the file as stale, but a
// PID whose home binding is merely inconclusive (daemonUnverifiable — a uid
// that could not be read, a foreign or withheld environ, an unresolvable
// AGENT_FACTORY_HOME) is neither safe to signal (it may be another home's
// daemon — the #4793 hazard) nor safe to delete the PID file over (that
// orphans the live daemon the file names, and every later recovery loses its
// handle to it). StopDaemon therefore needs the scope, not just the trust
// decision; locateDaemonPID only needs the trust decision, so it keeps the bool
// helper and the two paths still agree on what "ours" means (#1004).
//
// It is the classifier for a PID an explicit caller RECORDED — daemon.pid for
// StopDaemon, or a single pgrep result for the SIGTERM fallback — so it is tuned
// for that, not for the host-wide scan af reset's verifyScopedDaemon serves. It
// differs from verifyScopedDaemon in the three ways following a recorded PID
// demands:
//
//   - It does NOT apply isTestBinaryArgs. That heuristic keeps `go test`-spawned
//     fakes out of a HOST-WIDE scan, where a test binary is indistinguishable
//     from a real daemon by argv alone. A PID FILE was written by a real daemon,
//     so a binary under /tmp/go-build* or /tmp/Test* — an uncached
//     `go run . --daemon` or a source build placed in a temp dir — is a
//     legitimate target here, not a test fake. Applying the heuristic would
//     classify it foreign, delete its live PID file, and leave it running —
//     failing open on an inconclusive binding. The host-wide scan keeps the
//     heuristic (pgrepDaemonCandidates); this recorded-PID path does not.
//
//   - It resolves a RELATIVE AGENT_FACTORY_HOME against the DAEMON's working
//     directory, not ours. Two daemons launched from different directories with
//     the same relative value serve different homes; resolving both against the
//     caller's cwd would label a foreign one "ours" and let a stale PID file
//     signal it — the cross-home hazard #4793 is about. This mirrors
//     daemonProcessHome in doctor/skew.go: a home is spelled in the frame of the
//     process that set it. A relative home whose cwd cannot be read is
//     unresolvable (daemonUnverifiable), never resolved against ours.
//
//   - It resolves the DEFAULT home (no AGENT_FACTORY_HOME) from the DAEMON's
//     own $HOME, not ours. config.ConfigDirFor("") resolves the default against
//     the caller's $HOME, so a same-UID daemon launched under a different HOME
//     would otherwise be labelled ours and signalled on a stale PID file — the
//     #4793 hazard via the empty-env form. An unreadable or absent HOME is
//     daemonUnverifiable, never resolved against ours. This too mirrors
//     daemonProcessHome in doctor/skew.go.
//
// af reset's orphan scan deliberately keeps verifyScopedDaemon; a recorded PID
// and a host-wide discovery carry different evidence and warrant different
// policy.
func classifyDaemonHome(pid int) daemonScope {
	wantConfigDir, err := config.GetConfigDir()
	if err != nil {
		return daemonUnverifiable
	}
	wantHome, err := canonicalDir(wantConfigDir)
	if err != nil {
		return daemonUnverifiable
	}
	// Re-read argv rather than trusting the recorded PID: a PID is a reusable
	// kernel handle, so the process it names may have exited and its number
	// been recycled onto an unrelated process since the file was written.
	if !isAgentFactoryDaemon(pid) {
		return daemonForeign
	}
	owner, ok := processUID(pid)
	if !ok {
		return daemonUnverifiable
	}
	if owner != os.Getuid() {
		return daemonForeign
	}
	env, readable := daemonHomeEnv(pid)
	if !readable {
		return daemonUnverifiable
	}
	if env == "" {
		// AGENT_FACTORY_HOME is genuinely UNSET, so this daemon resolved the
		// DEFAULT home from ITS OWN $HOME (os.UserHomeDir at the daemon's
		// exec), not ours. config.ConfigDirFor("") resolves the default
		// against the CALLER's $HOME, so a same-UID daemon launched under a
		// different HOME would resolve to OUR default, compare equal to
		// wantHome, and be marked ours — letting a stale PID file or lone
		// pgrep result signal a daemon serving another home (the #4793
		// hazard via the empty-env form). Mirror daemonProcessHome in
		// doctor/skew.go: derive the default from the daemon's HOME read
		// out of its environ, and treat an unreadable or absent HOME as
		// unverifiable rather than guessing ours.
		daemonHome, status := proctree.LookupEnv(pid, "HOME")
		if status != proctree.EnvFound || daemonHome == "" {
			return daemonUnverifiable
		}
		env = filepath.Join(daemonHome, ".agent-factory")
	}
	gotHome, err := config.ConfigDirFor(env)
	if err != nil {
		// The daemon holds an AGENT_FACTORY_HOME we cannot resolve. The
		// "~user" form is rejected here; an unresolvable home is not "not
		// ours" — say so instead of guessing.
		log.WarningLog.Printf("sigterm fallback: cannot resolve AGENT_FACTORY_HOME=%q for daemon pid %d: %v", env, pid, err)
		return daemonUnverifiable
	}
	gotHome, ok = resolveHomeInDaemonFrame(pid, gotHome)
	if !ok {
		return daemonUnverifiable
	}
	got, err := canonicalDir(gotHome)
	if err != nil {
		return daemonUnverifiable
	}
	if got != wantHome {
		return daemonForeign
	}
	return daemonOurs
}

// resolveHomeInDaemonFrame absolutizes a daemon's (possibly relative) home
// against the DAEMON's own working directory rather than the caller's. An
// absolute home is returned unchanged; a relative one is joined to the cwd
// proctree.WorkingDir reports for pid; a relative one whose cwd cannot be read
// is reported unresolvable — resolving it against ours instead would make two
// daemons with the same relative value but different launch directories compare
// equal, the cross-home hazard #4793 turns on. See daemonProcessHome in
// doctor/skew.go for the same frame decision.
func resolveHomeInDaemonFrame(pid int, home string) (string, bool) {
	if filepath.IsAbs(home) {
		return home, true
	}
	cwd, ok := proctree.WorkingDir(pid)
	if !ok || cwd == "" {
		return "", false
	}
	return filepath.Join(cwd, home), true
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
