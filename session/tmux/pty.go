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

// Pty starts a "real" pseudo-terminal (PTY) using the creack/pty package.
type Pty struct{}

func (pt Pty) Start(cmd *exec.Cmd) (*os.File, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	// Reap the child process when it exits to avoid zombie processes.
	go func() {
		_ = cmd.Wait()
	}()
	return ptmx, nil
}

func (pt Pty) Close() {}

func MakePtyFactory() PtyFactory {
	return Pty{}
}
