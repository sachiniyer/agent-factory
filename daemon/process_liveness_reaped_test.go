package daemon

import (
	"os"
	"syscall"
	"testing"
)

// #6381: pidLooksAlive answered true for a process reaped between its signal-0
// probe and its /proc read. A zombie passes signal 0; if the parent reaps it
// before the cmdline read, /proc/<pid> vanishes and the read fails with ENOENT.
// That is an exit, and reading it as "alive" made TestWaitForShutdownCompletion-
// WaitsForPIDExit disagree with the wait that had just observed the exit. The
// window is microseconds wide, so the test makes the entry vanish on demand.
func TestPidLooksAlive_ProcEntryVanishedAfterSignalZeroIsAnExit(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("/proc is Linux-specific")
	}
	pid := os.Getpid() // answers signal 0
	orig := readProcCmdline
	t.Cleanup(func() { readProcCmdline = orig })

	readProcCmdline = func(int) ([]byte, error) {
		return nil, &os.PathError{Op: "open", Path: "/proc/x/cmdline", Err: syscall.ENOENT}
	}
	if pidLooksAlive(pid) {
		t.Fatal("pidLooksAlive = true for a process whose /proc entry vanished after signal 0; that is an exit")
	}
	if !pidExitObserved(pid) {
		t.Fatal("pidExitObserved = false for a process whose /proc entry vanished after signal 0")
	}

	// A read that fails for any other reason is not evidence of death.
	readProcCmdline = func(int) ([]byte, error) {
		return nil, &os.PathError{Op: "open", Path: "/proc/x/cmdline", Err: syscall.EACCES}
	}
	if !pidLooksAlive(pid) {
		t.Fatal("pidLooksAlive = false after an EACCES read: a failed read must not fabricate a death")
	}
}
