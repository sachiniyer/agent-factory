//go:build linux || darwin

package testguard

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
)

// StartGroupProcess starts cmd in its own process group
// (SysProcAttr.Setpgid) and registers cleanups that SIGKILL the whole group
// and then reap the direct child — the surviving-the-test guarantee every
// spinning or TERM-ignoring fixture needs (#4412).
//
// Kill the GROUP, not just the direct process: fixtures like
// `while :; do sleep 1; done` fork a fresh sleeper each iteration, and
// `... &` fixtures background children on purpose — killing only the shell
// orphans whatever is mid-flight. SIGKILL, not SIGTERM, because several
// fixtures deliberately `trap "" TERM` and must stay uncooperative for the
// test to remain meaningful; cleanup kills them from the harness side.
//
// The returned cmd is started; callers may still Wait on it themselves — the
// cleanup reap is a harmless no-op then — and should not Setpgid twice.
func StartGroupProcess(t testing.TB, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture process %q: %v", cmd.Path, err)
	}
	// Pin the group id with a second child joined to the same group and kept
	// unreaped until AFTER the cleanup kill. While any member exists — alive,
	// or a zombie the harness still holds — the pgid cannot be recycled
	// (POSIX XBD 3.297), so the cleanup's kill(-pgid) can never land on a
	// foreign group that acquired the number after the fixture's own members
	// died. Without the pin a caller that Waits the leader mid-test frees the
	// id early, and a group kill at an arbitrarily later point is exactly the
	// recycled-pgid hazard the daemon's reapers avoid by killing microseconds
	// after their own Wait (daemon/vscode_server.go reap). The pin dies with
	// the group when the kill lands; if a test kills the group itself first,
	// its held zombie keeps pinning.
	pin := exec.Command("sh", "-c", "exec sleep 86400")
	pin.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}
	if err := pin.Start(); err != nil {
		t.Fatalf("start fixture group pin: %v", err)
	}
	// Registered in this order so cleanup runs kill-then-wait (t.Cleanup is
	// LIFO): the pin's reap is registered before the leader's and the kill is
	// registered last, so both members still pin the id when the kill is
	// delivered. Wait before the kill would block forever on a live fixture.
	// Process.Wait rather than cmd.Wait: a test may already hold a cmd.Wait
	// goroutine, and concurrent Cmd.Wait calls are a data race on
	// ProcessState — os.Process.Wait is the concurrent-safe reap.
	t.Cleanup(func() { _, _ = pin.Process.Wait() })
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })
	KillProcessGroupOnCleanup(t, cmd.Process.Pid)
	return cmd
}

// processAlive reports whether pid currently names a process: kill(pid, 0)
// delivers no signal and answers ESRCH only when no such process exists.
// EPERM counts as alive — a pid owned by another user still exists, so an
// owner watched by ExitWhenOrphaned is reported correctly even when it is
// not ours to signal.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// KillProcessGroupOnCleanup registers a t.Cleanup that SIGKILLs process
// group pgid. For fixture processes the test already put in their own
// process group — or that made themselves group leaders, e.g. under
// systemd-run --scope — where the only handle the harness has is the pgid.
func KillProcessGroupOnCleanup(t testing.TB, pgid int) {
	t.Helper()
	t.Cleanup(func() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("kill fixture process group %d: %v", pgid, err)
		}
	})
}
