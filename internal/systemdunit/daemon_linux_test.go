//go:build linux

package systemdunit

import (
	"errors"
	"os"
	"strconv"
	"testing"
)

func stubSelfCgroup(t *testing.T, content string, err error) {
	t.Helper()
	previous := readSelfCgroup
	readSelfCgroup = func(string) ([]byte, error) { return []byte(content), err }
	t.Cleanup(func() { readSelfCgroup = previous })
}

func TestRunningDaemonProcessUsesProcessSpecificMarker(t *testing.T) {
	t.Setenv(DaemonMarkerEnv, DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	stubSelfCgroup(t, "", errors.New("proc unavailable"))
	if !RunningDaemonProcess() {
		t.Fatal("systemd's marked main process was not recognized")
	}

	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()+1))
	if RunningDaemonProcess() {
		t.Fatal("descendant with an inherited marker was mistaken for the daemon")
	}
}

func TestRunningDaemonProcessRecognizesLegacyUnitCgroup(t *testing.T) {
	t.Setenv(DaemonMarkerEnv, "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	stubSelfCgroup(t, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/agent-factory-daemon.service\n", nil)
	if !RunningDaemonProcess() {
		t.Fatal("legacy daemon cgroup was not recognized")
	}
}

func TestCgroupContainsUnitRequiresExactPathComponent(t *testing.T) {
	for _, content := range []string{
		"0::/user.slice/session-7.scope\n",
		"0::/user.slice/not-agent-factory-daemon.service\n",
		"12:memory:/agent-factory-daemon.service-old\n",
		"malformed\n",
	} {
		if cgroupContainsUnit(content, DaemonUnitName) {
			t.Errorf("cgroupContainsUnit(%q) = true for no exact unit component", content)
		}
	}
	if !cgroupContainsUnit("12:memory:/x/agent-factory-daemon.service/y\n", DaemonUnitName) {
		t.Error("exact legacy/hybrid cgroup component was not recognized")
	}
}
