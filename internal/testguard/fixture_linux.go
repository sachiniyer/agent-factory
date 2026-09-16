//go:build linux

package testguard

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// becomeOrphanReaper marks the test process a child subreaper
// (PR_SET_CHILD_SUBREAPER), so a fixture orphaned mid-test reparents HERE
// rather than to PID 1. On a containerized runner whose init never collects
// descendants, an orphan that exits under it stays a zombie for the life of
// the container — a process-table entry every watchdog test leaks (#4412).
// The reparenting takes effect for orphans created after the call, so it
// must run before the fixture's parent is killed. The attribute is reset on
// cleanup so a subreaper test cannot change where a sibling test's orphans
// land.
func becomeOrphanReaper(t testing.TB) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatalf("mark test process a child subreaper: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0)
	})
}

// reapOrphanedChild collects the zombie an adopted orphan left behind. It is
// a no-op when pid is not a child of this process — the orphan went to init
// because no subreaper was set — so the caller's death assertion, not this
// reap, remains the verdict.
func reapOrphanedChild(t testing.TB, pid int) {
	t.Helper()
	var status unix.WaitStatus
	var rusage unix.Rusage
	if _, err := unix.Wait4(pid, &status, 0, &rusage); err != nil && !errors.Is(err, unix.ECHILD) {
		t.Logf("reap orphaned fixture child %d: %v", pid, err)
	}
}

// reapGroupOrphans collects the members of a just-killed fixture group that
// were reparented HERE under the subreaper mark StartGroupProcess set: the
// grandchildren a whole-group SIGKILL orphans — including the pin's own
// `sleep 1`, so even a childless fixture leaves one — would otherwise sit as
// permanent zombies under a container init that never collects, a
// process-table entry every fixture test leaks (#4417 review).
//
// waitid(P_PGID) scopes the reap to the killed group's members: a foreign
// child of this test binary — adopted under the same subreaper flag, or a
// sibling test's process — is never consumed, which a wait4(-1) drain cannot
// promise. Each nil return is one collected member or a still-dying one —
// the two need no distinction, both mean keep polling — while ECHILD means
// no member of the group remains this process's child. Reparenting trails
// the kill by a few scheduler ticks, so a lone ECHILD cannot end the loop
// (it can mean "orphans still in flight"); it must hold across several
// polls. The loop is bounded so a member wedged in an unkillable state costs
// this cleanup its budget rather than hanging the test binary.
func reapGroupOrphans(t testing.TB, pgid int) {
	t.Helper()
	var info unix.Siginfo
	var rusage unix.Rusage
	quiet := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := unix.Waitid(unix.P_PGID, pgid, &info, unix.WEXITED|unix.WNOHANG, &rusage)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ECHILD):
			if quiet++; quiet >= 5 {
				return
			}
		case err != nil:
			t.Logf("reap fixture group %d orphans: %v", pgid, err)
			return
		default:
			quiet = 0
		}
		time.Sleep(5 * time.Millisecond)
	}
}
