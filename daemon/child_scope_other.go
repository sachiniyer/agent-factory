//go:build !linux

package daemon

import "os/exec"

// launchd has no shared service cgroup and no systemd transient scopes. Keep
// the existing direct child launch on Darwin and other non-Linux platforms.
func newDaemonChildCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
