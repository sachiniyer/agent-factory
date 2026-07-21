//go:build linux

// Package systemdunit identifies the process that systemd started as the Agent
// Factory daemon. Both tmux escape scopes and daemon-child cleanup scopes must
// use the same process-specific answer: descendants inherit the unit marker,
// while legacy installed units have no marker and require the kernel cgroup.
package systemdunit

import (
	"os"
	"strconv"
	"strings"
)

const (
	DaemonUnitName  = "agent-factory-daemon.service"
	DaemonMarkerEnv = "AGENT_FACTORY_SYSTEMD_UNIT"
)

var readSelfCgroup = os.ReadFile

// RunningDaemonProcess reports whether the current process is the main process
// of Agent Factory's systemd user service. It deliberately rejects descendants
// that merely inherited DaemonMarkerEnv.
func RunningDaemonProcess() bool {
	if os.Getenv(DaemonMarkerEnv) == DaemonUnitName {
		pid, err := strconv.Atoi(os.Getenv("SYSTEMD_EXEC_PID"))
		if err == nil && pid == os.Getpid() {
			return true
		}
	}

	// Units installed by an older af have no marker until reinstalled. Kernel
	// cgroup membership is authoritative for that upgrade path and works for
	// unified and legacy/hybrid /proc/self/cgroup formats.
	data, err := readSelfCgroup("/proc/self/cgroup")
	return err == nil && cgroupContainsUnit(string(data), DaemonUnitName)
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
