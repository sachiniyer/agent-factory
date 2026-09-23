package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	pid, source, scanned, err := locateDaemonPID()
	if err != nil {
		if scanned > 0 {
			// An ambiguity (multiple same-home `--daemon` candidates) or
			// another scan failure that left foreign/unverifiable daemons in
			// the scan (counted in `scanned`). The error already names the
			// same-home PIDs to kill by hand; do NOT append the blanket
			// `pkill -f -- '--daemon'` here — that command has no home or PID
			// constraint, so following it would kill exactly the foreign-home
			// daemons the home filter refused to touch, plus any unrelated
			// process carrying "--daemon".
			return ShutdownFailed, err
		}
		return ShutdownFailed, fmt.Errorf(
			"sigterm fallback failed: %w; run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			err,
		)
	}
	if pid == 0 {
		if scanned > 0 {
			// Every `--daemon` candidate the host scan found serves ANOTHER
			// home or could not be bound, and this filter deliberately refused
			// to signal any of them. Do NOT recommend a blanket
			// `pkill -f -- '--daemon'` here: that command has no home or PID
			// constraint, so following it would kill exactly the foreign-home
			// daemons this filtering refused to touch, plus any unrelated
			// process carrying "--daemon". The daemon serving THIS home must
			// be stopped by its PID, not a host-wide pattern.
			return ShutdownFailed, fmt.Errorf(
				"sigterm fallback: daemon is running on the control socket but no PID candidate was found for this home (%s); "+
					"%d `--daemon` process(es) were left untouched because they serve another home or could not be bound — "+
					"stop the daemon serving this home by its PID, then retry `af upgrade`",
				source, scanned,
			)
		}
		return ShutdownFailed, fmt.Errorf(
			"sigterm fallback: daemon is running on the control socket but no PID candidate was found (%s); "+
				"run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			source,
		)
	}

	log.InfoLog.Printf("sigterm fallback: signaling pre-#501 daemon (pid=%d source=%s)", pid, source)
	if err := signalAndWait(pid); err != nil {
		if scanned > 1 {
			// locateDaemonPID returned this proven-ours PID alongside one or
			// more foreign/unverifiable `--daemon` candidates (counted in
			// `scanned`). The blanket `pkill -f -- '--daemon'` the host-wide
			// hint below recommends matches on the full command line with no
			// home or PID constraint, so following it would signal exactly
			// those foreign daemons the home filter refused to touch, plus any
			// unrelated process carrying "--daemon". Carry the scoped recovery
			// the other branches already use: stop the daemon serving THIS
			// home by its PID, not a host-wide pattern.
			return ShutdownFailed, fmt.Errorf(
				"sigterm fallback for daemon pid %d: %w; %d other `--daemon` process(es) were left untouched because they serve another home or could not be bound — stop the daemon serving this home by its PID, then retry `af upgrade`",
				pid, err, scanned-1,
			)
		}
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

// locateDaemonPID returns the PID of the running daemon to signal, the source
// it was found in ("pid-file" or "pgrep"), and the count of `--daemon`
// candidates the host scan surfaced (0 when no scan ran, e.g. no PID file and
// pgrep unavailable). On failure to locate a PID, returns (0, source, scanned,
// nil) where source describes the suspected PID source for diagnostics (e.g.
// "pid-file pid=N foreign, pgrep: no matches for this home" or "no pid-file,
// pgrep unavailable") and scanned is the number of foreign/unverifiable
// candidates that were found and deliberately left untouched — including a
// PID-file entry the home binding rejected as a live foreign or unverifiable
// daemon, counted even when the scan finds nothing or does not run, so
// sigtermFallback does not recommend a blanket pkill that would kill it. An
// error is returned only for hard failures (ambiguous pgrep results, pgrep
// itself failing to execute).
func locateDaemonPID() (int, string, int, error) {
	pidFileSource := "no pid-file"
	// rejectedPIDFilePID is the PID a daemon.pid entry named when it was a
	// LIVE `af --daemon` the home binding PROVED serves another home
	// (daemonForeign) or could not bind (daemonUnverifiable). It is a
	// `--daemon` process this filter deliberately refused to signal, and a
	// blanket `pkill -f -- '--daemon'` (no home or PID constraint) would kill
	// it; counting it toward `scanned` even when the subsequent pgrep scan
	// finds nothing — or does not run, because pgrep is unavailable or errors
	// — keeps sigtermFallback from recommending that blanket pkill against
	// the very process the PID-file binding just refused to touch. A dead
	// or non-daemon PID-file entry is plain stale (no live foreign daemon
	// for a blanket pkill to hit), so it counts as 0.
	rejectedPIDFilePID := 0
	if pid, ok := readPIDFromFile(); ok {
		switch {
		case !pidLooksAlive(pid) || !isAgentFactoryDaemon(pid):
			log.InfoLog.Printf("sigterm fallback: PID file pid=%d is dead or not a daemon; falling back to pgrep", pid)
			pidFileSource = fmt.Sprintf("pid-file pid=%d stale", pid)
		default:
			switch classifyDaemonHome(pid) {
			case daemonOurs:
				return pid, "pid-file", 0, nil
			case daemonForeign:
				log.InfoLog.Printf("sigterm fallback: PID file pid=%d is a live daemon serving ANOTHER home; not signalling; falling back to pgrep", pid)
				pidFileSource = fmt.Sprintf("pid-file pid=%d foreign", pid)
				rejectedPIDFilePID = pid
			default: // daemonUnverifiable
				log.InfoLog.Printf("sigterm fallback: PID file pid=%d is a live daemon whose home could not be bound; not signalling; falling back to pgrep", pid)
				pidFileSource = fmt.Sprintf("pid-file pid=%d unverifiable", pid)
				rejectedPIDFilePID = pid
			}
		}
	}

	pids, err := scanDaemonCandidatesFn()
	if err != nil {
		// Preserve a rejected PID-file candidate as an untouched count even
		// when no scan ran, so sigtermFallback does not fall back to a
		// blanket pkill that would kill it.
		scanned := 0
		if rejectedPIDFilePID != 0 {
			scanned = 1
		}
		if errors.Is(err, errPgrepUnavailable) {
			return 0, fmt.Sprintf("%s, pgrep unavailable", pidFileSource), scanned, nil
		}
		return 0, "", scanned, fmt.Errorf("%s, pgrep: %w", pidFileSource, err)
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
		// Every scanned `--daemon` serves another home or is unverifiable, and
		// was deliberately NOT signalled. A rejected PID-file candidate is one
		// more such process even when pgrep found nothing, so it is folded into
		// `scanned` (without double-counting when pgrep also surfaced it) and
		// surfaced to the caller, which can then stop short of recommending a
		// blanket pkill that would kill exactly these foreign daemons.
		scanned := len(pids)
		if rejectedPIDFilePID != 0 && !slices.Contains(pids, rejectedPIDFilePID) {
			scanned++
		}
		return 0, fmt.Sprintf("%s, pgrep: no matches for this home (%d scanned)", pidFileSource, scanned), scanned, nil
	case 1:
		return scoped[0], "pgrep", len(pids), nil
	default:
		return 0, "", len(pids), fmt.Errorf(
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

// daemonPIDLockPoll is the cadence a nonblocking PID-file lock acquisition
// retries at when a deadline bounds the wait. Package var so tests can
// shorten it; production keeps it short so a deadline-bounded stop does not
// spend its whole budget asleep between attempts.
var daemonPIDLockPoll = 20 * time.Millisecond

// daemonPIDLockStartupBudget bounds how long writeDaemonPIDFile waits on the
// sidecar PID-file lock. RunDaemon reaches the PID-file write AFTER it has
// bound the control socket and acquired the per-home singleton lock, so a
// suspended or stalled writer holding daemon.pid.lock would otherwise block
// startup indefinitely: clients could Ping a daemon whose setup never
// advances past the PID-file write, while later launches are excluded by the
// home lock. The write is already best-effort (RunDaemon logs the failure and
// proceeds; the deferred removal only runs on success, and readers fall back
// to the pgrep scan when no PID file exists), so a lock this budget cannot
// acquire is abandoned rather than waited on. Package var so tests can
// shorten it; production keeps it short so a contended startup write does not
// stall a socket-bound daemon for long while still tolerating brief,
// legitimate contention (a concurrent stop's read-compare-unlink is sub-ms).
var daemonPIDLockStartupBudget = 2 * time.Second

// withDaemonPIDLock runs fn while holding an exclusive flock on a sidecar lock
// file next to the daemon PID file. writeDaemonPIDFile writes daemon.pid with an
// atomic temp-then-rename, and removePIDFileIfStillNames reads it and
// conditionally unlinks it; without coordination, a same-home daemon's atomic
// rename can land in the window between the removal's re-read and its unlink
// and have its freshly-written PID file deleted — orphaning the new daemon the
// way the unconditional unlink the removal replaced once did. A flock on the
// PID file itself does not help: the atomic rename changes daemon.pid's inode
// out from under any flock held on it, so a SEPARATE lock file is what the
// writer and the remover both hold to serialize read-compare-unlink against
// temp-then-rename. The lock is released by the kernel when the holder exits,
// so a crashed daemon never strands it. The lock file is left in place and
// re-opened by later callers, the way the rest of the codebase's sidecar .lock
// files are.
//
// deadline bounds the acquisition: zero blocks indefinitely (StopDaemon, which
// carries no caller deadline); a non-zero deadline makes the acquisition
// nonblocking and deadline-aware, so writeDaemonPIDFile's startup write caps
// its wait at daemonPIDLockStartupBudget — a suspended or stalled writer holding
// daemon.pid.lock would otherwise block startup indefinitely after RunDaemon
// bound the control socket and acquired the per-home singleton lock — and a
// deadline-bounded stopDaemonUntil that reaches foreign-PID cleanup while
// another writer holds daemon.pid.lock does not block past its admission
// deadline. The lock is abandoned (the write is best-effort anyway) rather
// than waiting indefinitely on a suspended writer or a stalled filesystem.
func withDaemonPIDLock(pidFile string, deadline time.Time, fn func() error) error {
	lock, err := os.OpenFile(pidFile+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open daemon PID lock: %w", err)
	}
	defer lock.Close()
	if !acquireDaemonPIDLock(lock, deadline) {
		return errors.New("daemon PID lock held by another writer")
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// acquireDaemonPIDLock takes an exclusive flock on lock, blocking indefinitely
// when deadline is zero and otherwise polling a nonblocking acquire at
// daemonPIDLockPoll against deadline. Returns false (without the lock) when
// the deadline expires, so the caller can abandon best-effort cleanup rather
// than exceed a bounded stop/restart contract.
func acquireDaemonPIDLock(lock *os.File, deadline time.Time) bool {
	if deadline.IsZero() {
		return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) == nil
	}
	for {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return true
		}
		if admissionDeadlineExpired(deadline) {
			return false
		}
		if !waitUntilAdmissionDeadline(deadline, daemonPIDLockPoll) {
			return false
		}
	}
}

// removePIDFileIfStillNames unlinks pidFile only when it still records pid.
// stopDaemonUntil read a stale foreign PID and proved the process it names is
// not this home's daemon; in the window between that read and this unlink a
// same-home daemon may have started and atomically rewritten daemon.pid with
// its own PID. Removing the file unconditionally would delete that valid
// replacement and recreate the untracked-daemon state the PID file exists to
// prevent — the new daemon would be live but no longer discoverable by
// StopDaemon. Re-read and compare first under the writer's lock (see
// withDaemonPIDLock) so the compare-and-unlink and a same-home daemon's
// atomic rewrite cannot interleave: leave a freshly-written valid file to its
// owner, and treat the unreadable/malformed case the same way rather than
// unlinking a file whose current contents we did not establish (#4793).
//
// deadline propagates the caller's admission deadline to the lock acquisition
// (see withDaemonPIDLock): a deadline-bounded stopDaemonUntil does not block
// indefinitely on a contended lock. On a deadline the cleanup is abandoned
// (best-effort, logged) rather than waiting past the stop/restart budget.
func removePIDFileIfStillNames(pidFile string, pid int, deadline time.Time) {
	if err := withDaemonPIDLock(pidFile, deadline, func() error {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			// Already gone (or unreadable) — nothing to remove; a missing file
			// is the desired end state, and a permission error is no worse than
			// the previous unconditional os.Remove would have been.
			return nil
		}
		var current int
		if _, err := fmt.Sscanf(string(data), "%d", &current); err != nil {
			// Malformed, and therefore not the foreign PID we read. A
			// newly-started daemon writes a valid PID, so this is neither the
			// stale file we own nor safe to claim — leave it for the next
			// caller.
			return nil
		}
		if current != pid {
			return nil // a new daemon has written its own PID; keep the file
		}
		if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("failed to remove stale daemon PID file %q: %v", pidFile, err)
		}
		return nil
	}); err != nil {
		log.WarningLog.Printf("sigterm fallback: could not coordinate removal of stale PID file %q: %v", pidFile, err)
	}
}

// reclaimDeadUnverifiablePIDFile handles the one TOCTOU the inconclusive
// binding leaves in stopDaemonUntil. classifyDaemonHome returns daemonUnverifiable
// when it cannot establish the candidate's uid or AGENT_FACTORY_HOME — a state
// stopDaemonUntil treats as "neither signal nor orphan": do not SIGTERM a PID
// that may be another home's daemon (#4793), and do not delete the PID file over
// it. But "unverifiable" can also be a TRANSIENT race rather than a real
// verdict: the recorded daemon passed the signal-0 and argv checks, then exited
// WHILE classifyDaemonHome was reading its uid or environ, which surfaces as
// unverifiable. If the PID is now dead there is nothing left to protect, so the
// stale PID file is reclaimed the way the earlier dead-PID checks do — removed
// only if it still names this PID (removePIDFileIfStillNames), so a same-home
// daemon that started in the interim is not orphaned — and the caller reports
// nothing to stop, letting a stop/restart or handoff proceed cleanly instead
// of failing against a process that no longer exists.
//
// Returns true when the PID is dead and the file was reclaimed (caller returns
// stopped=false, nil); false when the PID is still alive and the inconclusive
// verdict stands (caller surfaces the binding error). pidLooksAlive is the same
// liveness test the rest of stopDaemonUntil polls with; the exit it detects is
// the narrow window between the signal-0 probe upstream and this recheck. Lives
// in sigterm_fallback.go to keep daemon.go under the file-length lint limit
// (#1145), the same reason removePIDFileIfStillNames lives here. deadline is the
// caller's admission deadline, propagated to the PID-file lock the reclaim
// takes so a deadline-bounded stop does not block on a contended lock.
func reclaimDeadUnverifiablePIDFile(pidFile string, pid int, deadline time.Time) bool {
	if !pidLooksAlive(pid) {
		log.InfoLog.Printf("PID %d could not be bound to this home (unresolved) but has since exited; removing stale PID file", pid)
		removePIDFileIfStillNames(pidFile, pid, deadline)
		return true
	}
	return false
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
//   - It resolves the DEFAULT home (no AGENT_FACTORY_HOME) and a TILDE home
//     ("~" or "~/...") from the DAEMON's own $HOME, not ours. config.ConfigDirFor
//     resolves both the empty ("") and tilde ("~/state") forms against
//     os.UserHomeDir — the CALLER's $HOME — so a same-UID daemon launched under
//     a different HOME, or two daemons sharing a "~/state" spelling under
//     different HOME values, would both resolve to the caller's home, compare
//     equal, and be signalled on a stale PID file: the #4793 hazard via the
//     empty-env and tilde forms. Both are derived from the daemon's HOME read
//     out of its environ; an unreadable or absent HOME is daemonUnverifiable,
//     never resolved against ours; a "~user" form config.ConfigDirFor rejects
//     stays unverifiable. This too mirrors daemonProcessHome in doctor/skew.go.
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
	// Resolve a DEFAULT home (no AGENT_FACTORY_HOME) and a TILDE home ("~" or
	// "~/...") against the DAEMON's own $HOME, not ours. config.ConfigDirFor
	// expands both the empty ("") and tilde ("~/state") forms through
	// os.UserHomeDir — the CALLER's $HOME — so a same-UID daemon launched under
	// a different HOME, or two daemons sharing a "~/state" spelling under
	// different HOME values, would both resolve to the caller's home, compare
	// equal to wantHome, and be marked ours — letting a stale PID file or lone
	// pgrep result signal a daemon serving another home (the #4793 hazard via
	// the empty-env and tilde forms). Mirror daemonProcessHome in
	// doctor/skew.go: derive both from the daemon's HOME read out of its
	// environ, and treat an unreadable or absent HOME as unverifiable rather
	// than guessing ours.
	//
	// derivedFromDaemonHome records that `env` was rebuilt here from the
	// DAEMON's own $HOME. When the daemon's HOME is itself "~user"-prefixed
	// (HOME=~alice with no AGENT_FACTORY_HOME leaves env="~alice/.agent-factory";
	// or AGENT_FACTORY_HOME=~/state under HOME=~alice leaves "~alice/state"),
	// the rebuilt path carries a leading "~user" that came from the daemon's
	// valid HOME — a value the daemon's own file ops treated as a cwd-relative
	// path, not a home expansion. That prefix is NOT the raw
	// AGENT_FACTORY_HOME="~user" config.ConfigDirFor rejects; it is left for
	// resolveHomeInDaemonFrame to resolve against the daemon's cwd the way the
	// daemon did, and only a "~user" that came from a RAW AGENT_FACTORY_HOME
	// stays unverifiable (the rejection below gates on !derivedFromDaemonHome).
	derivedFromDaemonHome := false
	if env == "" || env == "~" || strings.HasPrefix(env, "~/") {
		daemonHome, status := proctree.LookupEnv(pid, "HOME")
		if status != proctree.EnvFound || daemonHome == "" {
			return daemonUnverifiable
		}
		switch {
		case env == "":
			env = filepath.Join(daemonHome, ".agent-factory")
		default: // "~" or "~/..."
			env = filepath.Join(daemonHome, strings.TrimPrefix(strings.TrimPrefix(env, "~"), "/"))
		}
		derivedFromDaemonHome = true
	}
	// env is now expanded in the DAEMON's frame. Resolve it here rather than
	// routing it through config.ConfigDirFor: when the daemon's own HOME is
	// itself "~"-prefixed (HOME=~/x, with no AGENT_FACTORY_HOME, the block
	// above leaves env="~/x/.agent-factory"), config.ConfigDirFor would expand
	// that leading "~" against the CALLER's $HOME — making a daemon that
	// actually serves <its cwd>/~/x/.agent-factory compare equal to the
	// caller's home and be marked ours on a stale PID file (the #4793 hazard
	// via a "~"-prefixed daemon HOME). A RAW "~user" form config.ConfigDirFor
	// rejects (AGENT_FACTORY_HOME="~user", NOT derived from the daemon's HOME)
	// stays unverifiable; a "~user" prefix derived from the daemon's HOME
	// above is already expanded in the daemon's frame and falls through.
	// Every other spelling (absolute, "~"-prefixed, or relative) is resolved
	// relative to the daemon's own cwd by resolveHomeInDaemonFrame the way the
	// daemon's own file ops resolved it.
	if !derivedFromDaemonHome && strings.HasPrefix(env, "~") && env != "~" && !strings.HasPrefix(env, "~/") {
		return daemonUnverifiable
	}
	gotHome, ok := resolveHomeInDaemonFrame(pid, env)
	if !ok {
		return daemonUnverifiable
	}
	got, err := canonicalDir(gotHome)
	if err != nil {
		return daemonUnverifiable
	}
	// gotHome was resolved and canonicalDir above renders it in the CALLER's
	// filesystem frame. An ABSOLUTE home such as AGENT_FACTORY_HOME=/state is
	// interpreted in the CANDIDATE's frame (its root / mount namespace), not
	// the caller's: a same-UID daemon in a container, chroot, or different mount
	// namespace whose /state is a different directory than the caller's /state
	// would resolve to the caller's /state here, compare equal to wantHome, be
	// classified daemonOurs, and be signalled through a stale PID file or lone
	// pgrep result — the #4793 hazard via a path collision across namespaces.
	// When the candidate's frame is not the caller's, or cannot be established,
	// the textual comparison cannot be trusted, so fail closed
	// (daemonUnverifiable) rather than guessing ours.
	if !sameProcessRoot(pid) {
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

// procRootFor returns the filesystem root the kernel exposes for pid on Linux
// (/proc/<pid>/root), resolved through the caller's frame so a symlinked root
// (a chroot reachable through /var/...) compares by its real target. The ok
// return is false when /proc is not present (not Linux — no mount-namespace
// hazard, sameProcessRoot keeps the existing behavior) or when the root symlink
// cannot be read (the process exited between the uid/environ probes above and
// this read, or the link is unavailable). A package var so a test can simulate
// a candidate whose root differs from the caller's — a container, chroot, or
// distinct mount namespace — which CI runners cannot create unprivileged.
var procRootFor = func(pid int) (string, bool) {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(link); err == nil {
		link = resolved
	}
	return link, true
}

// sameProcessRoot reports whether the process at pid shares the caller's
// filesystem root, so an absolute path resolved and compared in the caller's
// frame names the same directory the candidate sees. classifyDaemonHome
// resolves the candidate's home through the CALLER's frame (canonicalDir); a
// candidate in a container, chroot, or different mount namespace has its own
// root (/proc/<pid>/root is a symlink there), so its absolute AGENT_FACTORY_HOME
// is interpreted in THAT frame and an equal textual spelling is not the same
// directory. When the roots differ the comparison cannot be trusted and
// classifyDaemonHome fails closed (daemonUnverifiable) rather than guessing
// ours and signalling a cross-namespace daemon.
//
// Linux-only: /proc is the kernel's per-process root surface. On platforms
// without /proc (macOS) there is no equivalent filesystem-namespace hazard and
// this returns true so the existing path resolution stands; a /proc that IS
// present but whose root link cannot be read is a frame we cannot establish,
// so it returns false and the candidate falls through unverifiable.
func sameProcessRoot(pid int) bool {
	if _, err := os.Stat("/proc"); err != nil {
		return true
	}
	cand, ok := procRootFor(pid)
	if !ok {
		return false
	}
	self, ok := procRootFor(os.Getpid())
	if !ok {
		return false
	}
	return cand == self
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
