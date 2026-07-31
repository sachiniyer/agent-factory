package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// onArchiveHookTimeout bounds an operator hook that never returns. Archive is a
// synchronous lifecycle operation, but real dependency-tree sweeps can take
// minutes on a large worktree, so this is intentionally generous.
const onArchiveHookTimeout = 30 * time.Minute

const onArchiveHookWaitDelay = 2 * time.Second

// A hook is operator-authored and may be arbitrarily noisy. Keep only the tail:
// the end normally contains the actionable failure while a hard ceiling prevents
// a 30-minute hook from growing the daemon heap without bound.
const onArchiveHookOutputLimit = 64 * 1024

type archiveHookOutputTail struct {
	data      []byte
	truncated bool
}

func (w *archiveHookOutputTail) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) >= onArchiveHookOutputLimit {
		w.data = append(w.data[:0], p[len(p)-onArchiveHookOutputLimit:]...)
		w.truncated = true
		return written, nil
	}
	if overflow := len(w.data) + len(p) - onArchiveHookOutputLimit; overflow > 0 {
		copy(w.data, w.data[overflow:])
		w.data = w.data[:len(w.data)-overflow]
		w.truncated = true
	}
	w.data = append(w.data, p...)
	return written, nil
}

func (w *archiveHookOutputTail) String() string {
	if !w.truncated {
		return string(w.data)
	}
	return fmt.Sprintf("[output truncated to last %d bytes]\n%s", onArchiveHookOutputLimit, w.data)
}

type onArchiveHookContext struct {
	sessionID   string
	title       string
	repoRoot    string
	worktree    string
	archivePath string
}

// runOnArchiveHook resolves the command at operation time and runs it at the
// teardown chokepoint. ResolveConfig is load-bearing security policy here: the
// manifest admits on_archive_command from global and personal-project config,
// while LoadInRepoConfig rejects the same key before any repository-controlled
// command can execute.
func runOnArchiveHook(hookCtx onArchiveHookContext) error {
	resolved, err := config.ResolveConfig(hookCtx.repoRoot)
	if err != nil {
		return fmt.Errorf("load on-archive hook configuration: %w", err)
	}
	command := strings.TrimSpace(resolved.OnArchiveCommand)
	if command == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), onArchiveHookTimeout)
	defer cancel()

	var output archiveHookOutputTail
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = hookCtx.worktree
	cmd.Env = append(
		sessionenv.Filter(os.Environ(), "", resolved.SessionEnvPassthrough),
		"AF_SESSION_ID="+hookCtx.sessionID,
		"AF_SESSION_TITLE="+hookCtx.title,
		"AF_REPO_ROOT="+hookCtx.repoRoot,
		"AF_WORKTREE_PATH="+hookCtx.worktree,
		"AF_ARCHIVE_PATH="+hookCtx.archivePath,
	)
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = onArchiveHookWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}

	err = cmd.Run()
	if cmd.Process != nil {
		// A hook may background a descendant and exit. Kill the process group on
		// every path so no cleanup process survives the archive operation with a
		// cwd or inherited pipe in the worktree.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("timed out after %s%s", onArchiveHookTimeout, archiveHookOutput(output.String()))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The shell itself succeeded; WaitDelay only found an inherited process
		// still holding the capture pipe, and the process-group kill above reaped
		// it. Match the existing post-worktree hook contract.
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w%s", err, archiveHookOutput(output.String()))
	}
	return nil
}

func archiveHookOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	return ": " + output
}
