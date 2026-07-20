package tmux

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type PtyFactory interface {
	Start(cmd *exec.Cmd) (*os.File, error)
	Close()
}

// trackedPtyFactory is the production PtyFactory extension used when the
// caller needs the short-lived PTY command's exit status. Keep it optional so
// existing injected factories remain source-compatible; Start's test doubles
// generally model tmux's side effects directly and have no child to wait for.
type trackedPtyFactory interface {
	StartTracked(cmd *exec.Cmd) (*os.File, <-chan error, error)
}

// Pty starts a "real" pseudo-terminal (PTY) using the creack/pty package.
type Pty struct{}

func (pt Pty) Start(cmd *exec.Cmd) (*os.File, error) {
	ptmx, _, err := pt.StartTracked(cmd)
	return ptmx, err
}

func (pt Pty) StartTracked(cmd *exec.Cmd) (*os.File, <-chan error, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	// Reap the child process when it exits to avoid zombie processes.
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	return ptmx, done, nil
}

func startPtyTracked(factory PtyFactory, cmd *exec.Cmd) (*os.File, <-chan error, error) {
	if tracked, ok := factory.(trackedPtyFactory); ok {
		return tracked.StartTracked(cmd)
	}
	ptmx, err := factory.Start(cmd)
	return ptmx, nil, err
}

func (pt Pty) Close() {}

func MakePtyFactory() PtyFactory {
	return Pty{}
}
