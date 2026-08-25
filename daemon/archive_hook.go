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
//
// A var (not a const) only so tests can shorten it — the deadline's interaction
// with WaitDelay cleanup is exactly what #3407 got wrong, and it is not testable
// on a 30-minute clock. Production never reassigns it.
var onArchiveHookTimeout = 30 * time.Minute

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
	// What the hook ITSELF did outranks what the clock says (#3407). Both checks
	// below used to run in the opposite order, so a deadline that fired anywhere in
	// this window reported a timeout for a hook that had already finished — and an
	// archive hook is a committed operation whose outcome an operator reads out of
	// this one message.
	if errors.Is(err, exec.ErrWaitDelay) {
		// The shell itself succeeded — a non-zero exit surfaces as an *exec.ExitError,
		// never as this — and WaitDelay only found an inherited process still holding
		// the capture pipe, which the process-group kill above reaped. Match the
		// existing post-worktree hook contract.
		//
		// This is checked BEFORE ctx.Err() because both are routinely true together:
		// a hook that exits in milliseconds after backgrounding a child keeps Run
		// blocked on that child's pipe for the full WaitDelay, and any deadline
		// shorter than that elapses inside the wait. The hook had already succeeded;
		// only the cleanup crossed the line.
		return nil
	}
	// A deadline that fires while the hook is still RUNNING is a timeout. One that
	// fires after the hook reached its own exit is not — the hook ran, and its exit
	// status is the outcome to report, straggler or no straggler. ProcessState is
	// what tells them apart: Exited() is true only when the shell terminated on its
	// own terms, and false when the deadline's SIGKILL ended it (Run reports
	// "signal: killed" and the exit code is -1). Without that second clause a hook
	// that exited 23 while a child held the pipe past the deadline was reported as
	// "timed out after 1s" — a failure the operator cannot act on, standing in for a
	// specific exit status and its output, which they can.
	if ctx.Err() != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Exited()) {
		return fmt.Errorf("timed out after %s%s", onArchiveHookTimeout, archiveHookOutput(output.String()))
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
