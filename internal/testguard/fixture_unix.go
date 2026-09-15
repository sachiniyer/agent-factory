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
	// Registered in this order so cleanup runs kill-then-wait (t.Cleanup is
	// LIFO): Wait before the kill would block forever on a live fixture.
	// Process.Wait rather than cmd.Wait: a test may already hold a cmd.Wait
	// goroutine, and concurrent Cmd.Wait calls are a data race on
	// ProcessState — os.Process.Wait is the concurrent-safe reap.
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })
	KillProcessGroupOnCleanup(t, cmd.Process.Pid)
	return cmd
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
