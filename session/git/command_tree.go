package git

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// isolateGitCommandTree makes cancellation apply to git and every helper it
// spawned. CommandContext's default Cancel kills only the direct child; hooks,
// fsmonitor processes, and transports can otherwise outlive a timed-out safety
// probe while retaining its pipes and filesystem activity.
func isolateGitCommandTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}

func terminateGitCommandTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
