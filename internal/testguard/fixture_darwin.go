//go:build darwin

package testguard

import "testing"

// becomeOrphanReaper is a no-op on darwin, which has no subreaper mechanism:
// orphans reparent to launchd, and launchd — unlike a container's PID 1 —
// reaps them. The zombie-leak concern the linux implementation answers is a
// container-init defect this platform does not have.
func becomeOrphanReaper(testing.TB) {}

// reapOrphanedChild is a no-op on darwin: without a subreaper the orphaned
// child is never this process's to wait on, and launchd collects it.
func reapOrphanedChild(testing.TB, int) {}
