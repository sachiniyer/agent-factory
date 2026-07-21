package tmux

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

type answeredFailurePtyFactory struct {
	t       *testing.T
	waitErr error
}

// TestStartAnsweredCommandFailureDoesNotOverrideObservedSession proves the exit
// status is not itself the cleanup marker. If the post-exit probe sees the
// runtime, the workspace must remain protected even though new-session failed.
func TestStartAnsweredCommandFailureDoesNotOverrideObservedSession(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	var probes atomic.Int32
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			for _, arg := range c.Args {
				if arg != "has-session" {
					continue
				}
				if probes.Add(1) >= 3 {
					return nil
				}
				return errors.New("session not found")
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ptyFactory := answeredFailurePtyFactory{
		t:       t,
		waitErr: errors.New("new-session returned a late failure"),
	}
	ts := NewTmuxSessionWithDeps("answered-start-live", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("Start marked the workspace cleanup-safe even though its post-exit probe saw the session: %v", err)
	}
}

func (f answeredFailurePtyFactory) Start(*exec.Cmd) (*os.File, error) {
	f.t.Fatal("Start called instead of StartTracked")
	return nil, nil
}

func (f answeredFailurePtyFactory) StartTracked(*exec.Cmd) (*os.File, <-chan error, error) {
	ptmx, err := os.CreateTemp(f.t.TempDir(), "answered-start-pty")
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	done <- f.waitErr
	close(done)
	return ptmx, done, nil
}

func (answeredFailurePtyFactory) Close() {}

// TestStartAnsweredCommandFailureDoesNotClaimPreSpawn covers a new-session (or
// its systemd-run wrapper) that exits non-zero while exact probes answer that no
// session exists. The launch process began, so later name absence is not proof
// that a pane never ran or finished flushing into the worktree.
func TestStartAnsweredCommandFailureDoesNotClaimPreSpawn(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ptyFactory := answeredFailurePtyFactory{
		t:       t,
		waitErr: errors.New("new-session refused the request"),
	}
	ts := NewTmuxSessionWithDeps("answered-start-failure", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("a post-spawn failure was misclassified as proof the process never began: %v", err)
	}
}
