//go:build linux

package testguard

import (
	"errors"
	"testing"

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
