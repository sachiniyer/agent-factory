//go:build !linux

package systemdunit

const (
	DaemonUnitName  = "agent-factory-daemon.service"
	DaemonMarkerEnv = "AGENT_FACTORY_SYSTEMD_UNIT"
)

func RunningDaemonProcess() bool { return false }
