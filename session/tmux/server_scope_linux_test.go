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
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

func TestNewTmuxServerCommandScopesDaemonUnitSpawn(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	cmd, scoped := newTmuxServerCommand("new-session", "-d", "-s", "af_worker")
	if !scoped {
		t.Fatal("daemon-owned tmux server command was not marked systemd-scoped")
	}
	want := "systemd-run --user --scope --quiet --collect -- tmux new-session -d -s af_worker"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("daemon-owned server command = %q, want %q", got, want)
	}
}

func TestNewTmuxServerCommandDoesNotTrustInheritedSystemdMarker(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()+1))

	cmd, scoped := newTmuxServerCommand("new-session")
	if scoped {
		t.Fatal("descendant with inherited marker was marked systemd-scoped")
	}
	if got := strings.Join(cmd.Args, " "); got != "tmux new-session" {
		t.Fatalf("descendant with inherited marker launched %q, want direct tmux client", got)
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

// TestSystemdRunRefusalIsActionableButCleanupUnsafe covers the severe failure
// mode where the wrapper binary exists but the user manager refuses the
// transient scope. Pty used to discard that exit status, so Start waited two
// seconds and blamed tmux readiness; the CreateSession RPC now carries
// systemd-run's role and failure. The wait channel cannot prove whether a
// generic non-zero wrapper status came from systemd-run itself or the scoped
// tmux child, though, so it must not authorize fresh-worktree deletion.
func TestSystemdRunRefusalIsActionableButCleanupUnsafe(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
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
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("a systemd-run exit was mistaken for proof that its scoped child never started: %v", err)
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
