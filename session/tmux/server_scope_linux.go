//go:build linux

package tmux

import (
	"os/exec"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// newTmuxServerCommand wraps the only command that can create tmux's server in
// a transient user scope when af itself is the systemd-supervised daemon
// (#2176). setsid/double-forking changes ancestry but not cgroup membership;
// asking the user manager for a sibling scope is what deliberately moves the
// server out of agent-factory-daemon.service.
//
// Existing tmux servers are unaffected: new-session is merely a client in the
// short-lived scope and connects to the server that already owns the socket.
func newTmuxServerCommand(args ...string) (*exec.Cmd, bool) {
	if !systemdunit.RunningDaemonProcess() {
		return exec.Command("tmux", args...), false
	}
	scopeArgs := []string{"--user", "--scope", "--quiet", "--collect", "--", "tmux"}
	scopeArgs = append(scopeArgs, args...)
	return exec.Command("systemd-run", scopeArgs...), true
}
