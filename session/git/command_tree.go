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

func terminateGitCommandTree(cmd *exec.Cmd, waitErr error) {
	if cmd.Process == nil {
		return
	}
	if waitErr != nil {
		// Wait has already reaped Git on both ErrWaitDelay and an ordinary
		// ExitError. A helper holding an output pipe may therefore be reparented,
		// so a fresh parent/child snapshot cannot find it through the dead leader.
		// Setpgid preserved the other correlation: every unescaped helper remains
		// in the process group whose id is the original leader PID. Kill that
		// group on every failed wait, including a nonzero Git exit whose primary
		// error remains ExitError after WaitDelay expires.
		killAndReapGitProcessGroup(cmd.Process.Pid)
		return
	}
	reapGitDescendants(cmd.Process.Pid)
	_ = cmd.Process.Kill()
}

// killAndReapGitProcessGroup handles the post-WaitDelay case, where Wait has
// already collected Git before revealing that a descendant retained its output
// pipe. If AF is PID 1 or a child subreaper, those descendants are adopted by
// AF when Git exits; killing them without wait(2) would turn every probe into a
// permanent zombie. wait4 with a negative pid is scoped to this command's
// isolated process group, so it cannot steal an unrelated exec.Cmd child.
func killAndReapGitProcessGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	deadline := time.Now().Add(gitDescendantReapWait)
	for {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-pgid, &status, syscall.WNOHANG, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if pid <= 0 {
				break
			}
		}
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
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
