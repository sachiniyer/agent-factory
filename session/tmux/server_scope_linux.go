//go:build linux

package tmux

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	systemdDaemonUnit      = "agent-factory-daemon.service"
	systemdDaemonMarkerEnv = "AGENT_FACTORY_SYSTEMD_UNIT"
)

var readSelfCgroup = os.ReadFile

// newTmuxServerCommand wraps the only command that can create tmux's server in
// a transient user scope when af itself is the systemd-supervised daemon
// (#2176). setsid/double-forking changes ancestry but not cgroup membership;
// asking the user manager for a sibling scope is what deliberately moves the
// server out of agent-factory-daemon.service.
//
// Existing tmux servers are unaffected: new-session is merely a client in the
// short-lived scope and connects to the server that already owns the socket.
func newTmuxServerCommand(args ...string) (*exec.Cmd, bool) {
	if !runningInSystemdDaemonUnit() {
		return exec.Command("tmux", args...), false
	}
	scopeArgs := []string{"--user", "--scope", "--quiet", "--collect", "--", "tmux"}
	scopeArgs = append(scopeArgs, args...)
	return exec.Command("systemd-run", scopeArgs...), true
}

func runningInSystemdDaemonUnit() bool {
	// New unit templates carry an explicit marker. SYSTEMD_EXEC_PID makes the
	// check process-specific: tmux panes inherit the environment, but their pid
	// differs, so a nested af does not mistake itself for the daemon.
	if os.Getenv(systemdDaemonMarkerEnv) == systemdDaemonUnit {
		pid, err := strconv.Atoi(os.Getenv("SYSTEMD_EXEC_PID"))
		if err == nil && pid == os.Getpid() {
			return true
		}
	}

	// Units installed by an older af have no marker until reinstalled. The
	// kernel's membership is authoritative for those upgrades and works for
	// both unified and legacy/hybrid /proc/self/cgroup formats.
	data, err := readSelfCgroup("/proc/self/cgroup")
	return err == nil && cgroupContainsUnit(string(data), systemdDaemonUnit)
}

func cgroupContainsUnit(content, unit string) bool {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, component := range strings.Split(parts[2], "/") {
			if component == unit {
				return true
			}
		}
	}
	return false
}
