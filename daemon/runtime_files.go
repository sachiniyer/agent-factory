package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// cleanupDaemonRuntimeFiles removes the PID file and (best-effort) the control
// socket left behind by a stopped daemon. The PID file is tolerated as
// already-gone because the daemon's own SIGTERM handler removes it via
// removeDaemonPIDFile() before exiting — so on the SIGTERM-success path we
// race with the daemon's own cleanup.
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
func cleanupDaemonRuntimeFiles(pidFile string, deadline time.Time) {
	switch err := pingDaemonUntil(deadline); {
	case err == nil:
		log.InfoLog.Printf("a live daemon answered on the control socket after stop; leaving its runtime files in place")
		return
	case probeCouldNotDetermineLiveness(err):
		log.WarningLog.Printf("could not determine whether a daemon is still answering on the control socket (%v); leaving its runtime files in place rather than unlinking a socket that may be live", err)
		return
	}
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to remove daemon PID file: %v", err)
	}
	if socketPath, socketErr := DaemonSocketPath(); socketErr == nil {
		_ = os.Remove(socketPath)
	}
}

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
