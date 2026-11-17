package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// shutdownTargetKillWait bounds the wait for a SIGKILLed Shutdown target to be
// gone.
var shutdownTargetKillWait = 2 * time.Second

// FinishShutdownWithinGrace waits for the daemon a Shutdown RPC stopped to
// exit, within the grace StopDaemon gives a SIGTERM (stopDaemonGrace). A daemon
// still running then is stopped by signal with no second grace: SIGTERM, then
// SIGKILL at once. So a stopper that asked through the RPC (`af daemon adopt`,
// #7480) displaces a daemon within StopDaemon's own bound, rather than waiting
// out shutdownCompleteGrace for one whose stop never ends. escalated says a
// signal was sent.
//
// The signals go only to the process the Shutdown was sent to, identified by
// its pid and start time and re-verified immediately before each signal
// (#7482). They never go to whatever daemon.pid names by then: once the target
// exits, a successor can start and rewrite it. A target whose identity is
// unknown is never signalled. The error then says the daemon did not exit, and
// wraps ErrShutdownIncomplete.
func FinishShutdownWithinGrace(target ShutdownTarget) (escalated bool, err error) {
	waitErr := waitForShutdownCompletionWithin(target, stopDaemonGrace, stopDaemonGrace)
	if waitErr == nil {
		return false, nil
	}
	proc, known := shutdownTargetProcess(target)
	if !known {
		return false, fmt.Errorf("%w; its process is not known, so it was not stopped by signal", waitErr)
	}
	log.WarningLog.Printf("daemon pid %d did not exit within %s of its Shutdown; stopping it by signal", proc.PID, stopDaemonGrace)
	return true, stopShutdownTarget(proc)
}

// shutdownTargetProcess is the process a Shutdown was sent to, by pid and
// start time. known is false when either is missing.
func shutdownTargetProcess(target ShutdownTarget) (proc proctree.Process, known bool) {
	if target.PID <= 0 || target.StartToken == "" {
		return proctree.Process{}, false
	}
	startID, err := strconv.ParseUint(target.StartToken, 10, 64)
	if err != nil || startID == 0 {
		return proctree.Process{}, false
	}
	return proctree.Process{PID: target.PID, StartID: startID}, true
}

// stopShutdownTarget stops exactly proc: SIGTERM, then SIGKILL at once. Each
// signal is delivered only after re-verifying that the pid still names that
// process instance (proctree.Signal). A process that has gone, or whose pid now
// names another process, is not signalled again: it has exited, which is the
// goal. Then the runtime files are cleaned up as StopDaemon cleans them,
// keeping a daemon.pid that no longer names proc.
func stopShutdownTarget(proc proctree.Process) error {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := proctree.Signal(proc, sig); err != nil {
			if errors.Is(err, proctree.ErrIdentityChanged) {
				break
			}
			return fmt.Errorf("failed to signal daemon pid %d: %w", proc.PID, err)
		}
	}
	if alive := proctree.WaitForExits([]proctree.Process{proc}, shutdownTargetKillWait); len(alive) > 0 {
		return fmt.Errorf("%w: daemon pid %d still running %s after SIGKILL", ErrShutdownIncomplete, proc.PID, shutdownTargetKillWait)
	}
	if pidDir, err := config.GetConfigDir(); err == nil {
		cleanupDaemonRuntimeFiles(filepath.Join(pidDir, "daemon.pid"), proc.PID, time.Time{})
	}
	return nil
}
