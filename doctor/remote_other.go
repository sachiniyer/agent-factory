//go:build !linux

package doctor

import (
	"errors"
	"syscall"
)

// processExitedBeforeCancel reports whether the process has already exited
// before the context's Cancel fires. It uses Wait4 with WNOHANG|WNOWAIT to
// probe the process state without reaping it, so the exec package's Wait can
// still collect the exit status. A non-zero return means the process is a
// zombie (has exited but not been reaped); an ECHILD error means the process
// has been reaped. In either case the context did not kill the process — it
// was already dead when the deadline fired — so Cancel should not set
// ctxKilled and should return ErrProcessDone so the exec package knows the
// process already finished. A zero return means the process is still running,
// so the context's Kill is the cause of death and ctxKilled should be set.
//
// This distinguishes a genuine context-killed timeout (process was alive when
// Cancel fired) from an external signal death that races the deadline (process
// was already a zombie). Without this probe, Kill returns nil for both a live
// process and a zombie, and the ExitError's SIGKILL is indistinguishable
// between the context's Kill and an external OOM/kill -9.
//
// On Linux, wait4 rejects WNOWAIT with EINVAL, so the Linux variant in
// remote_linux.go uses waitid instead.
func processExitedBeforeCancel(pid int) bool {
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG|syscall.WNOWAIT, nil)
	if err != nil {
		// ECHILD: the process has been reaped (no child to wait for).
		// Other errors (EINVAL, ENOSYS): can't determine the state; be
		// conservative and assume the process is still running so Kill
		// can decide.
		return errors.Is(err, syscall.ECHILD)
	}
	// wpid == 0: the process is still running (no state change available).
	// wpid == pid: the process is a zombie (has exited, not yet reaped).
	// WNOWAIT leaves the zombie unreaped so the exec package's Wait collects it.
	return wpid != 0
}
