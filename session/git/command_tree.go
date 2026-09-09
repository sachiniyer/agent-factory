package git

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

const gitDescendantReapWait = 500 * time.Millisecond

// isolateGitCommandTree makes cancellation apply to git and every helper it
// spawned. CommandContext's default Cancel kills only the direct child; hooks,
// fsmonitor processes, and transports can otherwise outlive a timed-out safety
// probe while retaining its pipes and filesystem activity.
func isolateGitCommandTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill descendants first and leave git (or the fake-git shell in the
		// regression witness) alive long enough to wait(2) for them. Killing the
		// whole group simultaneously makes their parent unable to reap; under a
		// daemon that is PID 1, those zombies then persist indefinitely.
		reapGitDescendants(cmd.Process.Pid)
		if err := cmd.Process.Kill(); err != nil {
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}

func terminateGitCommandTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		reapGitDescendants(cmd.Process.Pid)
		_ = cmd.Process.Kill()
	}
}

func reapGitDescendants(rootPID int) {
	snapshot, err := proctree.Snapshot()
	if err != nil {
		return
	}
	tree := proctree.TreeOf(snapshot, rootPID)
	if len(tree) <= 1 {
		return
	}
	descendants := tree[1:]
	// Reverse BFS is leaf-first, so an intermediate helper remains available to
	// collect its own children before it is terminated and then collected by git.
	// Wait after each signal rather than killing the whole slice at once: the wait
	// is what gives each still-live parent a chance to call wait(2).
	deadline := time.Now().Add(gitDescendantReapWait)
	for index := len(descendants) - 1; index >= 0; index-- {
		child := descendants[index]
		_ = proctree.Signal(child, syscall.SIGKILL)
		for time.Now().Before(deadline) {
			_, err := proctree.Lookup(child.PID)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
