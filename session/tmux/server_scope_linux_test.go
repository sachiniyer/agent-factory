//go:build linux

package tmux

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

func stubSelfCgroup(t *testing.T, content string, err error) {
	t.Helper()
	previous := readSelfCgroup
	readSelfCgroup = func(string) ([]byte, error) {
		return []byte(content), err
	}
	t.Cleanup(func() { readSelfCgroup = previous })
}

func TestNewTmuxServerCommandScopesDaemonUnitSpawn(t *testing.T) {
	t.Setenv(systemdDaemonMarkerEnv, systemdDaemonUnit)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	stubSelfCgroup(t, "", errors.New("proc unavailable"))

	cmd, scoped := newTmuxServerCommand("new-session", "-d", "-s", "af_worker")
	if !scoped {
		t.Fatal("daemon-owned tmux server command was not marked systemd-scoped")
	}
	want := "systemd-run --user --scope --quiet --collect -- tmux new-session -d -s af_worker"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("daemon-owned server command = %q, want %q", got, want)
	}
}

func TestRunningInSystemdDaemonUnitRecognizesLegacyUnitCgroup(t *testing.T) {
	t.Setenv(systemdDaemonMarkerEnv, "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	stubSelfCgroup(t, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/agent-factory-daemon.service\n", nil)

	cmd, scoped := newTmuxServerCommand("new-session")
	if !scoped {
		t.Fatal("legacy daemon cgroup command was not marked systemd-scoped")
	}
	if got := cmd.Args[0]; got != "systemd-run" {
		t.Fatalf("legacy unit cgroup launched %q, want systemd-run scope", got)
	}
}

func TestNewTmuxServerCommandDoesNotTrustInheritedSystemdMarker(t *testing.T) {
	t.Setenv(systemdDaemonMarkerEnv, systemdDaemonUnit)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()+1))
	stubSelfCgroup(t, "0::/user.slice/user-1001.slice/session-7.scope\n", nil)

	cmd, scoped := newTmuxServerCommand("new-session")
	if scoped {
		t.Fatal("descendant with inherited marker was marked systemd-scoped")
	}
	if got := strings.Join(cmd.Args, " "); got != "tmux new-session" {
		t.Fatalf("descendant with inherited marker launched %q, want direct tmux client", got)
	}
}

func TestCgroupContainsUnitRequiresExactPathComponent(t *testing.T) {
	for _, content := range []string{
		"0::/user.slice/session-7.scope\n",
		"0::/user.slice/not-agent-factory-daemon.service\n",
		"12:memory:/agent-factory-daemon.service-old\n",
		"malformed\n",
	} {
		if cgroupContainsUnit(content, systemdDaemonUnit) {
			t.Errorf("cgroupContainsUnit(%q) = true for no exact unit component", content)
		}
	}
	if !cgroupContainsUnit("12:memory:/x/agent-factory-daemon.service/y\n", systemdDaemonUnit) {
		t.Error("exact legacy/hybrid cgroup component was not recognized")
	}
}

type refusingTrackedPtyFactory struct {
	t       *testing.T
	waitErr error
}

func (f refusingTrackedPtyFactory) Start(*exec.Cmd) (*os.File, error) {
	f.t.Fatal("Start called instead of StartTracked")
	return nil, nil
}

func (f refusingTrackedPtyFactory) StartTracked(*exec.Cmd) (*os.File, <-chan error, error) {
	ptmx, err := os.CreateTemp(f.t.TempDir(), "scope-refusal-pty")
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	done <- f.waitErr
	close(done)
	return ptmx, done, nil
}

func (refusingTrackedPtyFactory) Close() {}

// TestSystemdRunRefusalIsActionable covers the severe failure mode where the
// wrapper binary exists but the user manager refuses the transient scope. Pty
// used to discard that exit status, so Start waited two seconds and blamed tmux
// readiness; the CreateSession RPC now carries systemd-run's role and failure.
func TestSystemdRunRefusalIsActionable(t *testing.T) {
	t.Setenv(systemdDaemonMarkerEnv, systemdDaemonUnit)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	stubSelfCgroup(t, "", errors.New("proc unavailable"))
	forceNewSessionEnvMarkers(t, false)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				return errors.New("session not found")
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ptyFactory := refusingTrackedPtyFactory{
		t:       t,
		waitErr: errors.New("Failed to start transient scope unit: Access denied"),
	}
	ts := newTmuxSession("af_scope-refusal", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after systemd-run refused the transient scope")
	}
	for _, want := range []string{
		"systemd-run --user --scope failed",
		"Failed to start transient scope unit: Access denied",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Start error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "timed out waiting for tmux") {
		t.Fatalf("systemd-run refusal was misreported as a tmux readiness timeout: %v", err)
	}
}
