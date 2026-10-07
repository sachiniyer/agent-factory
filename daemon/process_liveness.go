package daemon

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// pidLooksAlive returns true when signal 0 to pid succeeds AND the kernel
// still has user-space state for the process (cmdline is non-empty). The
// cmdline check is the cheap Linux-side way to filter out zombies: once a
// process has exited but not yet been reaped, /proc/<pid>/cmdline is empty,
// but kill(pid, 0) still succeeds because the process entry exists. Without
// the second check, signalAndWait would wait the full sigtermFallbackGrace
// for any zombie before escalating to SIGKILL — visible as a 5s pause in
// `af upgrade` when the dying daemon's parent isn't waiting.
//
// macOS has no /proc, so there it asks the process table (proctree.Lookup, one
// kern.proc.pid sysctl — no subprocess, so it is cheap at the wait's 50ms
// cadence) and treats ErrProcessExited, which it returns for a zombie, as dead.
// Without that, an exited-but-unreaped daemon keeps passing signal 0, and
// WaitForShutdownCompletion would burn its full grace and withhold the respawn
// over a daemon that is already gone (#5007). Any other lookup failure falls
// back to the signal-0 result: a failed read must not fabricate a death.
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
	if runtime.GOOS == "darwin" {
		if _, err := proctree.Lookup(pid); errors.Is(err, proctree.ErrProcessExited) {
			return false
		}
	}
	return true
}

// pidExitObserved reports whether pid has provably exited, for a wait that
// must not mistake "cannot tell" for "gone". Unlike !pidLooksAlive it treats a
// signal-0 failure as exit only when the answer is absence (ESRCH /
// os.ErrProcessDone): EPERM means the process exists but this caller may not
// signal it — an LSM or credential boundary — and reading that as exit would
// end the shutdown wait while the daemon still drains (#5007). A process that
// does answer signal 0 has exited when pidLooksAlive's zombie checks say so.
// The wait pairs this with its start-token check, so treating an unsignalable
// PID as alive cannot pin it on a recycled stranger.
func pidExitObserved(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return errIsProcessGone(err)
	}
	return !pidLooksAlive(pid)
}

// processStartToken identifies one incarnation of pid by its start stamp, so a
// wait that outlives the process can tell "still the daemon" from "the PID was
// recycled to something else" (#5007). It is proctree's StartID — the same
// stamp the identity-checked signal path binds to (Linux: /proc/<pid>/stat
// field 22; darwin: kinfo_proc's p_starttime) — compared for equality only.
// Returns "" when it cannot be observed (the process is gone or a zombie,
// another platform, a read failure) — callers then rely on liveness alone.
func processStartToken(pid int) string {
	proc, err := proctree.Lookup(pid)
	if err != nil || proc.StartID == 0 {
		return ""
	}
	return strconv.FormatUint(proc.StartID, 10)
}

// processStartTokenFn is processStartToken, indirected so a test can simulate
// PID reuse — a real reuse cannot be arranged on demand.
var processStartTokenFn = processStartToken
