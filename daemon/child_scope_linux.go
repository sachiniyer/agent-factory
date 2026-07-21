//go:build linux

package daemon

import (
	"os/exec"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// systemdBoundChildStopTimeout is shorter than the unit's RestartSec, but the
// correctness barrier does not depend on that gap: BindsTo+After puts stopping
// the old scope and restarting its owner in one ordered transaction. The bound
// keeps a TERM-ignoring child from delaying recovery for systemd's default 90s.
const systemdBoundChildStopTimeout = "4s"

// newDaemonChildCommand places every long-lived daemon-owned process tree in a
// transient scope bound to the daemon service. The service itself intentionally
// uses KillMode=process so legacy tmux servers trapped in its cgroup survive an
// upgrade. A separate bound scope gives systemd authoritative ownership of
// watchers/editors without putting those unrelated lifetime classes back in one
// kill domain.
//
// BindsTo stops the scope when the daemon unit becomes inactive after a panic,
// SIGKILL, or OOM. After supplies both halves of the ordering guarantee: the
// child cannot start before its owner, and on restart the old child scope is
// fully stopped before the replacement daemon may start. There is therefore no
// startup reconciliation window in which two watcher/editor generations run.
func newDaemonChildCommand(name string, args ...string) *exec.Cmd {
	if !systemdunit.RunningDaemonProcess() {
		return exec.Command(name, args...)
	}
	scopeArgs := []string{
		"--user", "--scope", "--quiet", "--collect",
		"--property=BindsTo=" + systemdunit.DaemonUnitName,
		"--property=After=" + systemdunit.DaemonUnitName,
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec=" + systemdBoundChildStopTimeout,
		"--", name,
	}
	scopeArgs = append(scopeArgs, args...)
	return exec.Command("systemd-run", scopeArgs...)
}
