package git

import (
	"bytes"
	"context"
	"os/exec"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// RunPostWorktreeHooksAsync runs the per-repo post_worktree_commands in the
// background. Each command is executed sequentially via "sh -c" with the
// working directory set to worktreePath. The provided context can be used to
// cancel in-flight hooks (e.g. when the worktree is being cleaned up). Each
// command runs as the leader of its own process group so that, on
// cancellation, the whole tree — including grandchildren the script
// backgrounded with `&` or `disown` — is killed together.
// Errors are logged but do not propagate.
func RunPostWorktreeHooksAsync(ctx context.Context, repoPath, worktreePath string) {
	repoID := config.RepoIDFromRoot(repoPath)
	repoCfg, err := config.LoadRepoConfig(repoID)
	if err != nil {
		log.WarningLog.Printf("failed to load repo config for hooks: %v", err)
		return
	}
	if len(repoCfg.PostWorktreeCommands) == 0 {
		return
	}

	cmds := repoCfg.PostWorktreeCommands
	go func() {
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
			cmd.Dir = worktreePath
			cmd.Stdout = &output
			cmd.Stderr = &output
			// Place sh in its own process group so we can signal the whole
			// tree on cancellation. exec.CommandContext only kills the
			// immediate shell, leaving grandchildren the script backgrounded
			// with `&` or `disown` alive — they get reparented to init and
			// outlive the session (see #610).
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

			if err := cmd.Start(); err != nil {
				log.ErrorLog.Printf("post-worktree hook %q failed: %v", cmdStr, err)
				continue
			}

			// On cancellation, SIGKILL the whole process group (negative pid
			// targets the group led by cmd.Process.Pid). doneCh ensures the
			// watchdog exits when the command finishes normally so it does
			// not leak across loop iterations.
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

			if ctx.Err() != nil {
				log.InfoLog.Printf("post-worktree hooks cancelled for %s", worktreePath)
				return
			}
			if waitErr != nil {
				log.ErrorLog.Printf("post-worktree hook %q failed: %v\n%s", cmdStr, waitErr, output.String())
			} else {
				log.InfoLog.Printf("post-worktree hook %q completed successfully", cmdStr)
			}
		}
	}()
}
