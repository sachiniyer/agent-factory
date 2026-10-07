package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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
// macOS has no /proc, so there it asks ps for the process state and treats a
// zombie ("Z") as dead. Without that, an exited-but-unreaped daemon keeps
// passing signal 0, and WaitForShutdownCompletion would burn its full grace
// and withhold the respawn over a daemon that is already gone (#5007). If ps
// fails (the pid vanished between checks, or ps is missing) it falls back to
// the signal-0 result: a ps failure must not fabricate a death.
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
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err == nil && strings.Contains(string(out), "Z") {
			return false
		}
	}
	return true
}

// processStartToken identifies one incarnation of pid by its start time, so a
// wait that outlives the process can tell "still the daemon" from "the PID was
// recycled to something else" (#5007). On Linux it is /proc/<pid>/stat field
// 22 (starttime in clock ticks), read after the LAST ')' because comm may
// itself contain spaces and parentheses; on macOS it is `ps -o lstart=`.
// Returns "" when the start time cannot be observed (the process is gone,
// another platform, a read failure) — callers then rely on liveness alone.
func processStartToken(pid int) string {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return ""
		}
		stat := string(data)
		i := strings.LastIndexByte(stat, ')')
		if i < 0 {
			return ""
		}
		// Fields after comm start at field 3 (state), so field 22 is index 19.
		fields := strings.Fields(stat[i+1:])
		if len(fields) < 20 {
			return ""
		}
		return fields[19]
	case "darwin":
		out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return ""
}

// processStartTokenFn is processStartToken, indirected so a test can simulate
// PID reuse — a real reuse cannot be arranged on demand.
var processStartTokenFn = processStartToken
