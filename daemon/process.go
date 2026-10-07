package daemon

import (
	"errors"
	"fmt"
	"io"
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

// lastDaemonSpawnPID and lastDaemonSpawnLogOffset describe the most recent
// detached daemon child launchDaemonProcessAt spawned: its PID (0 before any
// spawn) and the daemon log's size at spawn time, so a caller diagnosing a
// readiness failure can scope a log-tail scan to the content THIS daemon
// wrote. Written by launchDaemonProcessAt inside the ensureDaemonMu-critical
// launch() call and read after the failed readiness wait in the same
// critical section; ensureDaemonAdHocUntil zeroes the PID before launch so a
// stubbed or early-failing launcher cannot attribute a stale spawn's exit to
// this attempt.
var (
	lastDaemonSpawnPID       int
	lastDaemonSpawnLogOffset int64
)

// daemonLogPathFn resolves the daemon's agent-factory.log path — the file a
// detached child writes its startup failure into. Package-level so tests can
// pin the resolution (the package's TestMain initializes logging under a
// sandboxed home, which would otherwise freeze the path for every test).
var daemonLogPathFn = log.LogFilePath

func launchDaemonProcess() error {
	// Find the agent-factory binary.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	return launchDaemonProcessAt(execPath)
}

func launchDaemonProcessAt(execPath string) error {
	// Record the daemon log's size BEFORE spawning so a readiness-failure
	// diagnostic can quote only what this daemon wrote; stat failure (no log
	// yet) records 0 — the whole file is then this spawn's to quote.
	if p := daemonLogPathFn(); p != "" {
		if info, statErr := os.Stat(p); statErr == nil {
			lastDaemonSpawnLogOffset = info.Size()
		} else {
			lastDaemonSpawnLogOffset = 0
		}
	}
	pid, err := startDaemonChild(execPath)
	if err != nil {
		return err
	}
	lastDaemonSpawnPID = pid

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

// daemonPIDWriteAttempts bounds the in-process retries writeDaemonPIDFile
// makes when the sidecar PID-file lock is contended
// (errDaemonPIDLockUnavailable). A stop's read-compare-unlink holds
// daemon.pid.lock for sub-millisecond stretches, so contention at startup is
// usually transient; failing the start on the FIRST contended attempt would
// hand the failure to the supervisor's restart budget (systemd's
// StartLimitBurst is only five), which a busy box could exhaust on a
// momentarily-stuck lock (#5188 review). Persistent failures — a symlinked
// daemon.pid, an unwritable home, ENOSPC — are never retried: they cannot
// resolve on their own, and the startup must fail closed promptly.
// daemonPIDWriteRetryPause lets a finishing holder's release land before the
// next attempt burns its acquisition budget. Package vars so tests can
// shrink them.
var (
	daemonPIDWriteAttempts   = 3
	daemonPIDWriteRetryPause = 100 * time.Millisecond
)

// writeDaemonPIDFile atomically writes the current process's PID to the daemon
// PID file with mode 0600. Used by RunDaemon so callers (StopDaemon, the
// SIGTERM fallback in RequestShutdown) can locate and signal this daemon.
//
// It REFUSES a symlinked path (#3672). The PID file is af's own liveness
// bookkeeping at a path af chose, written on start and deleted on teardown, so
// a link there is neither af's to write through nor af's to replace — the same
// answer the bearer token and the autostart unit take. It takes the PID-file
// lock (withDaemonPIDLock) so a stop's read-compare-unlink can't interleave.
//
// The returned error always names the PID-file path and the underlying cause,
// which for a planted symlink already carries the remedy: RunDaemon surfaces
// it verbatim to the caller, and a detached spawn's copy lands in the daemon
// log (persisting the failure for status/doctor is #5196).
func writeDaemonPIDFile() error {
	path, err := daemonPIDFilePath()
	if err != nil {
		return fmt.Errorf("cannot write daemon PID file: %w", err)
	}
	var writeErr error
	for attempt := 0; attempt < daemonPIDWriteAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(daemonPIDWriteRetryPause)
		}
		writeErr = withDaemonPIDLock(path, time.Now().Add(daemonPIDLockStartupBudget), func() error {
			return config.AtomicWriteFileRefusingLink(path, []byte(strconv.Itoa(os.Getpid())), 0600)
		})
		if !errors.Is(writeErr, errDaemonPIDLockUnavailable) {
			break
		}
	}
	if writeErr != nil {
		return fmt.Errorf("cannot write daemon PID file %s: %w", path, writeErr)
	}
	return nil
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

// daemonPIDFileMaxBytes caps a daemon.pid read. The file is a pid and a
// newline; anything bigger is not something af wrote.
const daemonPIDFileMaxBytes = 64

// readManagedFileNoFollow reads a small af-managed file through a descriptor
// opened O_NOFOLLOW|O_NONBLOCK and validated by fstat — a planted or swapped-in
// symlink fails the open atomically (an Lstat-then-ReadFile pair could be
// swapped between the check and the open), a FIFO or device substitute cannot
// hold the caller in the open or the read, and a non-regular or oversized file
// reports as unreadable. Used by every daemon.pid read that must not hang or
// follow a swapped replacement: the lock-holding teardown re-reads, the
// liveness readers, and Health.
func readManagedFileNoFollow(path string, maxBytes int64) ([]byte, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		return nil, false
	}
	return data, true
}

// readPIDFromFile parses the daemon PID file. Returns (0, false) when the
// file is missing, malformed, or points at an obviously bogus PID. A stale
// or reused PID is not filtered here — callers re-verify with cmdline.
func readPIDFromFile() (int, bool) {
	path, err := daemonPIDFilePath()
	if err != nil {
		return 0, false
	}
	// Same no-follow/nonblocking read as the teardown path — a FIFO swapped
	// in for daemon.pid would otherwise hang every caller (status, doctor,
	// StopDaemon) in the open.
	data, ok := readManagedFileNoFollow(path, daemonPIDFileMaxBytes)
	if !ok {
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

// daemonPIDLockStartupBudget bounds how long ONE writeDaemonPIDFile attempt
// waits on the sidecar PID-file lock. It is sized so the whole retry loop —
// daemonPIDWriteAttempts budgets plus the pauses between them — cannot eat
// the caller's daemonReadyTimeout (5s): that window starts when the child is
// launched and must also cover config/manager initialization, the under-lock
// startup ping, and the socket bind, so a lock clearing on the LAST attempt
// still needs seconds of headroom for the work around the loop. A lock that
// never clears must also fail the start BEFORE the caller's readiness wait
// ends, or the caller reports a bare "did not become ready" timeout while
// this daemon's real error is still retrying (#5188 review). 700ms x 3
// attempts + 2 x 100ms pauses bounds the loop at ~2.3s — far past any real
// holder's sub-millisecond critical section, and leaving ~2.7s for the rest
// of startup. Package var so tests can shorten it.
var daemonPIDLockStartupBudget = 700 * time.Millisecond

// errDaemonPIDLockUnavailable is returned by acquireDaemonPIDLock when the
// deadline passed while the sidecar daemon.pid.lock stayed held — the
// TRANSIENT contention outcome (another writer is mid read-compare-unlink or
// a holder is briefly stalled), as distinct from a hard failure like a
// symlinked lock path or a flock that errors for a non-contention reason. writeDaemonPIDFile matches on it (errors.Is) to retry
// startup contention in-process rather than spending a supervisor restart on
// it (#5188); every other caller treats it like any lock failure.
var errDaemonPIDLockUnavailable = errors.New("daemon PID lock held by another writer")

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
// daemon.pid.lock would otherwise block startup indefinitely while the daemon
// already holds the per-home singleton lock and nothing can replace it — and a
// deadline-bounded stopDaemonUntil that reaches foreign-PID cleanup while
// another writer holds daemon.pid.lock does not block past its admission
// deadline. The lock is abandoned rather than waiting indefinitely on a
// suspended writer or a stalled filesystem.
func withDaemonPIDLock(pidFile string, deadline time.Time, fn func() error) error {
	lockPath := pidFile + ".lock"
	// os.OpenFile FOLLOWS a pre-existing daemon.pid.lock symlink, so a
	// symlinked sidecar is not a stable coordination object: a holder swapped
	// between the remover acquiring its lock and a new daemon acquiring its
	// own would let the remover hold the old inode (and read the stale PID)
	// while the writer holds the replacement inode and atomically writes a
	// fresh PID file — the remover then unlinks that fresh file, reopening
	// the read/compare/unlink race the sidecar lock is there to close (#4793
	// review). Open with O_NOFOLLOW so a symlink at the lock path is refused
	// atomically (ELOOP), the same way upgradetxn's locks refuse one; af
	// created this sidecar (O_CREATE) and re-opens it, so a link is a user
	// arrangement af did not author — the same policy writeDaemonPIDFile and
	// removeDaemonPIDFile take against a symlinked daemon.pid (#3672).
	// Callers treat lock-acquisition failure as best-effort: the startup
	// write is logged and proceeds (readers fall back to the pgrep scan), and
	// the stop-side removal leaves the stale file (safe — readers re-verify
	// cmdline + the home binding before acting), so a refused lock degrades
	// to the same safe "leave it" outcome a contended or untrusted-FS lock
	// already does.
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		return fmt.Errorf("open daemon PID lock: %w", err)
	}
	defer lock.Close()
	if err := acquireDaemonPIDLock(lock, deadline); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// acquireDaemonPIDLock takes an exclusive flock on lock, blocking indefinitely
// when deadline is zero and otherwise polling a nonblocking acquire at
// daemonPIDLockPoll against deadline. Returns errDaemonPIDLockUnavailable (a
// retryable-by-writeDaemonPIDFile contention outcome) only when the deadline
// passes while the lock stays held; a flock error that is NOT contention —
// ENOTSUP on a filesystem without flock support, an I/O error — is returned
// verbatim so fail-closed PID publication reports the real cause instead of
// retrying a hard failure for the whole startup budget and claiming another
// writer holds the lock.
func acquireDaemonPIDLock(lock *os.File, deadline time.Time) error {
	if deadline.IsZero() {
		// The blocking form never returns EWOULDBLOCK; a non-nil error is a
		// real failure, not contention.
		return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("flock daemon PID lock: %w", err)
		}
		if admissionDeadlineExpired(deadline) {
			return errDaemonPIDLockUnavailable
		}
		if !waitUntilAdmissionDeadline(deadline, daemonPIDLockPoll) {
			return errDaemonPIDLockUnavailable
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
// The number-only compare the lock guards is not enough on its own: a foreign
// PID that exits can have its number recycled by a same-home daemon that writes
// the same PID value to the PID file, so a stale entry that still names the old
// number can be the new daemon's freshly-written file. Re-classify the live PID
// under the lock (pidBelongsToThisHome) and leave the file when the PID now
// belongs to this home's daemon — a recycled number on our own daemon is its
// handle, not the stale foreign entry to unlink (#4793).
//
// The sidecar lock this held is no coordination at all on a filesystem whose
// flock cannot be trusted (NFS, SMB, 9p, FUSE — see lockFSReliable in
// singleton_lock.go): the writer's temp-then-rename is not serialized against
// this read-compare-unlink, so the very race the lock prevents on local
// filesystems reopens on a network one. The removal is skipped there and the
// stale file is left; a stale PID file is safe to leave because readers re-verify
// it (cmdline + home binding) before acting, and the writer's fresh file is
// preserved.
//
// Like removeStaleDaemonPIDFile it refuses a symlinked PID file (#3672):
// writeDaemonPIDFile refuses to write through one, so a link here is a user
// arrangement af did not author, and the cleanup neither reads its target
// through the link nor unlinks the link. The refusal is taken up front, so a
// symlinked daemon.pid is left in place the way the other stale-PID cleanups
// leave one.
//
// deadline propagates the caller's admission deadline to the lock acquisition
// (see withDaemonPIDLock): a deadline-bounded stopDaemonUntil does not block
// indefinitely on a contended lock. On a deadline the cleanup is abandoned
// (best-effort, logged) rather than waiting past the stop/restart budget.
func removePIDFileIfStillNames(pidFile string, pid int, deadline time.Time) {
	// On a filesystem whose flock cannot be trusted (NFS, SMB, 9p, FUSE — see
	// lockFSReliable in singleton_lock.go), the sidecar daemon.pid.lock does not
	// serialize the writer's temp-then-rename against this read-compare-unlink:
	// a successful flock may silently no-op, so a same-home daemon's freshly
	// written PID file can land in the window between the re-read and the unlink
	// and be deleted — the race the lock is meant to prevent (#4793). Skip the
	// conditional removal there and leave the stale file; readers re-verify a PID
	// file (cmdline + home binding) before acting, so a stale file left in place
	// is safe, and the writer's fresh one is preserved.
	if ok, _ := lockFSReliable(filepath.Dir(pidFile)); !ok {
		log.InfoLog.Printf("stale daemon PID file %q on a filesystem whose flock is untrusted; leaving it in place", pidFile)
		return
	}
	if err := withDaemonPIDLock(pidFile, deadline, func() error {
		// A symlinked PID file is not af's to unlink — writeDaemonPIDFile
		// refuses to write through one, so a link here is a user arrangement
		// af did not author. Refuse it the way the other stale-PID cleanups do
		// (removeStaleDaemonPIDFile, #3672): do not read its target through the
		// link to decide whether to unlink it, and do not unlink the link. A
		// missing path (an already-gone file) is an ordinary done state, so
		// RefuseManagedFileSymlink's nil-for-absent return falls through to the
		// read below.
		if err := config.RefuseManagedFileSymlink(pidFile); err != nil {
			if errors.Is(err, config.ErrManagedFileSymlink) {
				log.InfoLog.Printf("stale daemon PID file %q (PID: %d) is a symlink af did not write through; leaving it in place", pidFile, pid)
			}
			return nil
		}
		// No-follow, nonblocking, fstat-validated read: this runs holding
		// daemon.pid.lock, so an os.ReadFile here would let a FIFO swapped in
		// for daemon.pid block the daemon's deferred cleanup — and every
		// later lock waiter, including a successor's fail-closed PID
		// publication — on a writer that never comes. Anything that is not a
		// small regular file is not a PID file af wrote; leave it for the
		// reader-side verifications rather than unlinking on a guess.
		data, ok := readManagedFileNoFollow(pidFile, daemonPIDFileMaxBytes)
		if !ok {
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
		// The number still names the stale PID, but the kernel may have recycled it
		// onto a same-home daemon that wrote the same number. Re-classify the live
		// PID with the tri-state classifier (not the bool helper, which collapses
		// daemonUnverifiable into "not ours"): a PROVEN-foreign (or dead) PID is
		// unlinked; daemonOurs is the recycled number's new owner, and
		// daemonUnverifiable may be this home's own daemon whose environ could not
		// be read — unlinking it would orphan the live daemon, so retain it. (#4793)
		switch classifyDaemonHome(pid) {
		case daemonOurs, daemonUnverifiable:
			return nil
		case daemonForeign:
			if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
				log.WarningLog.Printf("failed to remove stale daemon PID file %q: %v", pidFile, err)
			}
		}
		return nil
	}); err != nil {
		log.WarningLog.Printf("sigterm fallback: could not coordinate removal of stale PID file %q: %v", pidFile, err)
	}
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
	// Deliberately a plain read: StopDaemon must TOLERATE a symlinked
	// daemon.pid — read the pid through it, classify the named process, and
	// leave the link untouched (the #3672 refusal governs writes/unlinks,
	// not this read; TestStopDaemon_DoesNotUnlinkASymlinkedStalePIDFile and
	// the LiveDaemonFifthSite variant pin the no-error contract). The
	// descriptor-validated read stays on the lock-holding teardown paths
	// where a swapped FIFO would wedge cleanup, not on the read-only
	// front door.
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
	// Capture the process identity at the home-binding decision so the
	// SIGTERM→SIGKILL escalation is bound to the instance classifyDaemonHome
	// proved serves this home, not a PID the kernel recycled between the
	// decision and the signal. proc (an os.Process for the numeric PID) is
	// PID-only — its Signal has no instance check, so a recycled PID serving
	// another home would be terminated and the grace poll (pidLooksAlive)
	// would mistake the replacement for the original still being alive and
	// SIGKILL it (#4793 review). proctree.Signal revalidates the StartID
	// immediately before each signal and refuses (ErrIdentityChanged) when the
	// PID no longer names this instance; AliveSame revalidates the same
	// instance for the grace poll, so a recycled PID is neither signaled nor
	// waited on.
	switch scope := classifyDaemonHome(pid); scope {
	case daemonOurs:
		// Proven to serve this home: capture its identity and signal it
		// through the identity-checked path below.
		daemonProc, perr := proctree.Lookup(pid)
		if perr != nil {
			if errors.Is(perr, proctree.ErrProcessExited) {
				// The classified daemon exited between the home binding and this
				// snapshot; nothing to signal, and a recycled PID is not. Clean
				// up the way the errIsProcessGone SIGTERM path below does.
				log.InfoLog.Printf("daemon process (PID: %d) exited before the signal; cleaning up", pid)
				cleanupDaemonRuntimeFiles(pidFile, deadline)
				return true, nil
			}
			// Could not bind the identity; do not fall back to a PID-only signal
			// that could hit a recycled PID. Leave the PID file for the next
			// caller to re-evaluate (the binding was proven, the snapshot was
			// not — a transient /proc read failure, not a foreign daemon).
			log.WarningLog.Printf("could not capture process identity for daemon pid %d; not signaling (%v)", pid, perr)
			return false, nil
		}

		// Send SIGTERM through the identity-checked signaler so a recycled PID is
		// refused (ErrIdentityChanged) rather than terminated.
		if err := proctree.Signal(daemonProc, syscall.SIGTERM); err != nil {
			if errors.Is(err, proctree.ErrIdentityChanged) {
				if !pidLooksAlive(pid) {
					// The classified daemon exited on its own before SIGTERM
					// landed; it is gone, which is the desired outcome and not a
					// recycled-PID signal.
					log.InfoLog.Printf("daemon process (PID: %d) exited before SIGTERM landed; cleaning up", pid)
					cleanupDaemonRuntimeFiles(pidFile, deadline)
					return true, nil
				}
				// The PID was recycled onto another process between the home
				// binding and the signal; do not terminate the replacement.
				return false, fmt.Errorf("daemon pid %d exited and its number was recycled onto a different process before SIGTERM; not signaling the replacement", pid)
			}
			return false, fmt.Errorf("failed to signal daemon process: %w", err)
		}

		// Poll for graceful exit. AliveSame checks the captured instance, not the
		// PID number, so a recycled PID does not masquerade as the original still
		// being alive (which would send it SIGKILL).
		gracefulDeadline := admissionBoundedDeadline(deadline, stopDaemonGrace)
		exited := false
		for time.Now().Before(gracefulDeadline) {
			if !proctree.AliveSame(daemonProc) {
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
			if err := proctree.Signal(daemonProc, syscall.SIGKILL); err != nil {
				if errors.Is(err, proctree.ErrIdentityChanged) {
					// Exited between the grace poll and SIGKILL — the daemon is
					// gone, which is the desired outcome; a recycled PID is not
					// signaled.
					log.InfoLog.Printf("daemon process (PID: %d) exited before SIGKILL landed; cleaning up", pid)
					cleanupDaemonRuntimeFiles(pidFile, deadline)
					return true, nil
				}
				if !errIsProcessGone(err) {
					return false, fmt.Errorf("failed to stop daemon process: %w", err)
				}
			}
		}

		cleanupDaemonRuntimeFiles(pidFile, deadline)
		log.InfoLog.Printf("daemon process (PID: %d) stopped successfully", pid)
		return true, nil
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
