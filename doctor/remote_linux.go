//go:build linux

package doctor

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// processExitedBeforeCancel reports whether the process has already exited
// before the context's Cancel fires. It uses waitid with WNOHANG|WNOWAIT|WEXITED
// to probe the process state without reaping it, so the exec package's Wait can
// still collect the exit status. On Linux, wait4 rejects WNOWAIT with EINVAL,
// whereas waitid natively supports it. A non-zero si_signo (SIGCHLD) means the
// process is a zombie (has exited but not been reaped); an ECHILD error means the
// process has been reaped. In either case the context did not kill the process —
// it was already dead when the deadline fired — so Cancel should not set
// ctxKilled and should return ErrProcessDone so the exec package knows the
// process already finished. A zero si_signo (or an unrecognised error) means the
// process is still running, so the context's Kill is the cause of death and
// ctxKilled should be set.
//
// This distinguishes a genuine context-killed timeout (process was alive when
// Cancel fired) from an external signal death that races the deadline (process
// was already a zombie). Without this probe, Kill returns nil for both a live
// process and a zombie, and the ExitError's SIGKILL is indistinguishable
// between the context's Kill and an external OOM/kill -9.
func processExitedBeforeCancel(pid int) bool {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if err != nil {
		// ECHILD: the process has been reaped (no child to wait for).
		// Other errors (ENOSYS): can't determine the state; be conservative
		// and assume the process is still running so Kill can decide.
		return errors.Is(err, syscall.ECHILD)
	}
	// info.Signo == 0: no child state change available (still running).
	// info.Signo != 0 (SIGCHLD): the child has exited (zombie, not reaped).
	// WNOWAIT leaves the zombie unreaped so the exec package's Wait collects it.
	return info.Signo != 0
}
