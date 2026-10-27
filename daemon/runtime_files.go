package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// cleanupDaemonRuntimeFiles removes the PID file and (best-effort) the control
// socket left behind by a stopped daemon. pid is the daemon this stop act
// accounted for — the PID the caller read from pidFile and signaled. The PID
// file is tolerated as already-gone because the daemon's own teardown removes
// it via removeDaemonPIDFile() after releasing the home lock — so on the
// SIGTERM-success path we race with the daemon's own cleanup.
//
// A NEW daemon can also start during StopDaemon's signal/poll window (the
// autostart unit racing `af daemon install`, or an upgrade respawn) and bind
// the control socket before this cleanup runs. Removing the socket then would
// unlink the live daemon's socket file: the daemon keeps serving the
// unreachable inode, pings against the path fail, and the next EnsureDaemon
// spawns yet another daemon while the first leaks (#767). So if anything
// ANSWERS on the socket, the runtime files belong to a live daemon — leave
// them all in place. The daemon we just stopped cannot answer: its listener
// died with the process. The worst false positive (a ping answered by a
// process still mid-SIGKILL) merely leaves a stale socket behind, which the
// next spawn's bind path replaces.
//
// The probe is bounded by the caller's admission deadline, so it cannot
// overshoot a CLI budget — but a bounded probe has THREE outcomes, not two,
// and only one of them licenses the unlink (#4163). An answer means live. A
// refusal or ENOENT means nothing is there. A lapsed deadline or a dial
// timeout means af DID NOT LOOK, which is not evidence of absence: the earlier
// code read every non-nil error as "nobody home" and unlinked a live daemon's
// socket whenever the 5s admission budget happened to be spent, which is the
// #767 failure this guard exists to prevent. Indeterminate therefore leaves
// the files in place, the same stance refuseIndeterminateReap takes for an
// unreachable sandbox: unreachable is not gone.
//
// The PID file removal is removePIDFileIfStillNames — the locked
// re-read-and-compare unlink every stop-side removal uses (#5132/#4793).
// The socket probe above cannot see a successor that claimed the home but
// has not bound its socket yet, and that successor's writeDaemonPIDFile can
// land between this read and an unconditional unlink — deleting the live
// daemon's file and orphaning it (#5188). The compare also carries the #3672
// symlink policy: a symlinked daemon.pid is left in place because af cannot
// have written through one.
//
// The control socket gets the same treatment, one step down: the PID
// compare can sit up to its budget on a contended daemon.pid.lock, and a
// successor can claim the released home lock and bind its socket inside
// that wait — an unconditional unlink then deletes the live listener's
// path, which is the #767 failure the first probe exists to prevent. So
// the socket is re-probed adjacent to its unlink AND serialized with
// daemon startup: a successor binds inside bindControlServerExclusive's
// daemon.spawn lock, so taking that lock here makes "nothing answered"
// and the os.Remove atomic against its bind — an unserialized pair could
// observe ECONNREFUSED a microsecond before the successor's bind and then
// unlink its live listener. The lock wait is bounded (a wedged holder must
// not hang a stop) and on timeout the socket is LEFT — an unverifiable
// socket is never unlinked blind. The same three-way discipline applies
// inside: an answer keeps it, and "could not look" keeps it too —
// unreachable is not gone. The socket is not an af-managed regular file —
// it is the listener's bind path — so its os.Remove is unrelated to the
// link policy.
func cleanupDaemonRuntimeFiles(pidFile string, pid int, deadline time.Time) {
	// Bound the opening probe the same way the second probe (adjacent to the
	// unlink, below) is. A zero deadline (public StopDaemon) gets
	// socketRecheckBudget as its floor, so SetDeadline always runs inside
	// callDaemonNoEnsureAttemptBefore — without it a wedged listener (bound
	// but never servicing the RPC, the net.Listen→Accept window) would make
	// the dial succeed and client.Call block forever, hanging StopDaemon.
	// A non-zero deadline (EnsureDaemon reclaim) is capped at the earlier
	// of the caller's absolute deadline and now+socketRecheckBudget, so
	// this probe cannot consume the whole admission budget before the
	// safety-critical second probe runs — and it never overshoots the
	// caller's deadline even if the goroutine is paused between computing
	// the budget and issuing the ping (a fresh time.Now().Add(probe) could
	// land past the original deadline when little time remained).
	probeDeadline := time.Now().Add(socketRecheckBudget)
	if !deadline.IsZero() {
		if deadline.Before(probeDeadline) {
			probeDeadline = deadline
		}
		if !probeDeadline.After(time.Now()) {
			log.InfoLog.Printf("caller deadline spent before the first control-socket probe; leaving runtime files in place")
			return
		}
	}
	switch err := pingDaemonUntil(probeDeadline); {
	case err == nil:
		log.InfoLog.Printf("a live daemon answered on the control socket after stop; leaving its runtime files in place")
		return
	case probeCouldNotDetermineLiveness(err):
		log.WarningLog.Printf("could not determine whether a daemon is still answering on the control socket (%v); leaving its runtime files in place rather than unlinking a socket that may be live", err)
		return
	}
	removePIDFileIfStillNames(pidFile, pid, pidLockCleanupDeadline(deadline))
	if socketPath, socketErr := DaemonSocketPath(); socketErr == nil {
		// A home deleted mid-stop must not be resurrected here:
		// WithFileLockTimeout ensures the spawn lock's parent exists, so
		// taking it on a deleted home would recreate <home>/ and leave
		// daemon.spawn.lock behind — a new artifact where the user just
		// deleted one. The stat below filters the common case; the
		// residual stat→acquire window is closed inside the lock, where a
		// home containing ONLY the lock file this acquisition just made is
		// detected as the resurrection and undone.
		if _, statErr := os.Stat(filepath.Dir(socketPath)); statErr != nil {
			return
		}
		// Both waits derive from the CALLER's remaining deadline, not
		// fresh budgets: this cleanup runs at the tail of a stop that may
		// have already consumed the admission budget, and a contended
		// daemon.spawn lock (a successor's ping→publish→bind) could hold
		// for most of it — unbounded extra seconds after the deadline
		// would keep a deadline-expired ensureDaemonUntil blocked. Zero
		// deadline (public StopDaemon) gets the recheck budget as its
		// floor; an expired one leaves the socket untouched.
		budget := socketRecheckBudget
		if !deadline.IsZero() {
			if budget = time.Until(deadline); budget <= 0 {
				log.InfoLog.Printf("no caller deadline remains for the control-socket recheck; leaving it in place")
				return
			} else if budget > socketRecheckBudget {
				budget = socketRecheckBudget
			}
		}
		lockTarget, lockErr := daemonSpawnLockTarget()
		if lockErr != nil {
			log.WarningLog.Printf("cannot serialize the control-socket recheck (%v); leaving it in place", lockErr)
			return
		}
		err := config.WithFileLockTimeout(lockTarget, budget, func() error {
			// Resurrection check, now that existence and acquisition are
			// atomic: if the home was deleted between the stat above and
			// this lock, ensureStorageParent recreated the directory and
			// it contains ONLY the lock file this acquisition just made —
			// a real home reaching spawn-lock stage already holds
			// daemon.lock, config.toml, and friends. Undo the resurrection
			// (rmdir fails non-empty if anything raced in — safe) and skip
			// the recheck: a deleted home has no socket left to probe.
			home := filepath.Dir(socketPath)
			if entries, derr := os.ReadDir(home); derr == nil &&
				len(entries) == 1 && entries[0].Name() == filepath.Base(lockTarget)+".lock" {
				_ = os.Remove(lockTarget + ".lock")
				_ = os.Remove(home)
				return nil
			}
			// Second look, immediately before the unlink and under the
			// spawn lock: the window since the first probe is exactly
			// where a successor binds. The probe keeps the same caller
			// deadline so lock wait plus dial cannot outrun the budget.
			// Nothing listening is the common case — the dial comes back
			// refused in microseconds, so this stays cheap when there is
			// no race to win.
			probe := socketRecheckBudget
			if !deadline.IsZero() {
				if r := time.Until(deadline); r < probe {
					probe = r
				}
			}
			if probe <= 0 {
				log.InfoLog.Printf("caller deadline spent before the control-socket recheck; leaving it in place")
				return nil
			}
			switch err := pingDaemonUntil(time.Now().Add(probe)); {
			case err == nil:
				log.InfoLog.Printf("a daemon bound the control socket during cleanup; leaving it in place")
			case probeCouldNotDetermineLiveness(err):
				log.WarningLog.Printf("could not determine whether the control socket is live (%v); leaving it in place", err)
			default:
				_ = os.Remove(socketPath)
			}
			return nil
		})
		if err != nil {
			log.WarningLog.Printf("could not take the spawn lock to recheck the control socket (%v); leaving it in place", err)
		}
	}
}

// socketRecheckBudget bounds the second control-socket probe inside
// cleanupDaemonRuntimeFiles. It is deliberately small: the probe is a
// refusal check, not a readiness wait — when no listener exists the answer
// is immediate, and when one is starting up a second is long enough for
// its accept loop to be serving.
const socketRecheckBudget = 2 * time.Second

// probeCouldNotDetermineLiveness reports whether a post-stop ping failed in a
// way that says nothing about whether a daemon is listening. A lapsed
// admission deadline returns before any dial is attempted, and a shortened
// dial can time out against a daemon that is simply slow to accept; neither is
// a refusal. Only a real refusal (ECONNREFUSED) or a missing socket proves
// absence, so everything timeout-shaped is treated as unknown.
func probeCouldNotDetermineLiveness(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
