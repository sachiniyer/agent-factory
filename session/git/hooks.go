package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// hookWaitDelay bounds how long cmd.Wait blocks after a hook's shell exits
// before the inherited stdout/stderr pipes are force-closed. A script that
// backgrounds a process with `&`/`disown` and exits immediately leaves that
// grandchild holding the write end of the capture pipe, so without a bound
// cmd.Wait would block until the grandchild itself exits (defeating the
// process-group cleanup below). It only elapses when something outlives the
// shell — normal hooks complete their I/O at shell exit and return instantly.
const hookWaitDelay = 2 * time.Second

// RunPostWorktreeHooksAsync runs the per-repo post_worktree_commands in the
// background. Each command is executed sequentially via "sh -c" with the
// working directory set to worktreePath. The provided context can be used to
// cancel in-flight hooks (e.g. when the worktree is being cleaned up). Each
// command runs as the leader of its own process group so that the whole tree
// — including grandchildren the script backgrounded with `&` or `disown` —
// is killed together once the hook's shell exits, whether that exit is normal
// completion or cancellation. Backgrounded grandchildren therefore never
// outlive their parent hook.
// Errors are logged but do not propagate.
//
// The returned channel is closed once every hook has finished — whether by
// normal completion, failure, or ctx cancellation. It is closed immediately
// when there are no hooks to run (or the repo config can't be resolved). It
// lets callers tell whether provisioning is still in flight; in particular the
// readiness wait uses it so a slow build hook running concurrently with the
// agent is not charged against the agent's startup budget (see task.WaitForReady).
func RunPostWorktreeHooksAsync(ctx context.Context, repoPath, worktreePath string) <-chan struct{} {
	return RunPostWorktreeHooksAsyncWithEnvironment(ctx, repoPath, worktreePath, "", nil)
}

// RunPostWorktreeHooksAsyncWithEnvironment is the session-aware form used by
// GitWorktree. The compatibility wrapper above remains default-deny too, but
// has no selected-agent or explicit extension names to add.
func RunPostWorktreeHooksAsyncWithEnvironment(ctx context.Context, repoPath, worktreePath, agent string, passthrough []string) <-chan struct{} {
	done := make(chan struct{})
	repoCfg, err := config.ResolveConfig(repoPath)
	if err != nil {
		log.WarningLog.Printf("failed to resolve repo config for hooks: %v", err)
		close(done)
		return done
	}
	if len(repoCfg.PostWorktreeCommands) == 0 {
		close(done)
		return done
	}

	cmds := repoCfg.PostWorktreeCommands
	go func() {
		defer close(done)
		for _, cmdStr := range cmds {
			select {
			case <-ctx.Done():
				log.InfoLog.Printf("post-worktree hooks cancelled for %s", worktreePath)
				return
			default:
			}
			log.InfoLog.Printf("running post-worktree hook in %s: %s", worktreePath, cmdStr)

			var output bytes.Buffer
			cmd := exec.Command("sh", "-c", cmdStr)
			cmd.Env = sessionenv.Filter(os.Environ(), agent, passthrough)
			cmd.Dir = worktreePath
			cmd.Stdout = &output
			cmd.Stderr = &output
			// Place sh in its own process group so we can signal the whole
			// tree on cancellation. exec.CommandContext only kills the
			// immediate shell, leaving grandchildren the script backgrounded
			// with `&` or `disown` alive — they get reparented to init and
			// outlive the session (see #610).
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			// Bound the post-exit wait so a backgrounded grandchild holding the
			// capture pipe cannot keep cmd.Wait blocked until it exits.
			cmd.WaitDelay = hookWaitDelay

			if err := cmd.Start(); err != nil {
				log.ErrorLog.Printf("post-worktree hook %q failed: %v", cmdStr, err)
				continue
			}

			// While the hook is running, a watchdog SIGKILLs the whole process
			// group on cancellation (negative pid targets the group led by
			// cmd.Process.Pid) so a long-running hook is torn down promptly.
			// doneCh stops the watchdog once cmd.Wait() returns so it does not
			// leak across loop iterations.
			doneCh := make(chan struct{})
			go func(pgid int) {
				select {
				case <-ctx.Done():
					_ = syscall.Kill(-pgid, syscall.SIGKILL)
				case <-doneCh:
				}
			}(cmd.Process.Pid)

			waitErr := cmd.Wait()
			close(doneCh)

			// Always SIGKILL the process group once the shell has exited, not
			// just on cancellation. If the script backgrounded a process with
			// `&` or `disown` and the shell exited immediately (no `wait`), that
			// grandchild keeps running in the worktree's process group; the
			// watchdog above only fires on cancellation and may already have
			// exited via doneCh. Signalling the group here reaps any survivors
			// on every exit path — normal completion or a cancellation that
			// raced ahead of doneCh — so no grandchild outlives its parent hook
			// (see #610, #769).
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

			if ctx.Err() != nil {
				log.InfoLog.Printf("post-worktree hooks cancelled for %s", worktreePath)
				return
			}
			switch {
			case waitErr == nil:
				log.InfoLog.Printf("post-worktree hook %q completed successfully", cmdStr)
			case errors.Is(waitErr, exec.ErrWaitDelay):
				// The shell exited but a backgrounded grandchild held the
				// capture pipe open past hookWaitDelay; it was just killed with
				// the process group above. This is not a hook failure.
				log.InfoLog.Printf("post-worktree hook %q completed; terminated backgrounded processes that outlived the shell", cmdStr)
			default:
				log.ErrorLog.Printf("post-worktree hook %q failed: %v\n%s", cmdStr, waitErr, output.String())
			}
		}
	}()
	return done
}
