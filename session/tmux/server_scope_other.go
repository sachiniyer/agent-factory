//go:build !linux

package tmux

import "os/exec"

func ConfigureDaemonServer(string) func() { return func() {} }

func EnsureDaemonServer() error { return nil }

func HandleDedicatedServerExec() {}

// launchd does not have systemd's shared service cgroup failure mode. Keep the
// existing direct tmux launch on Darwin (and other non-Linux platforms) rather
// than invoking a Linux-only service-manager command (#2176, #1931).
func newTmuxServerCommand(args ...string) (*exec.Cmd, bool) {
	return newTmuxServerCommandAfterEnsure(nil, args...)
}

func newTmuxServerCommandAfterEnsure(_ error, args ...string) (*exec.Cmd, bool) {
	return exec.Command("tmux", args...), false
}
