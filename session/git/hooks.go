package git

import (
	"context"
	"os/exec"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// RunPostWorktreeHooksAsync runs the per-repo post_worktree_commands in the
// background. Each command is executed sequentially via "sh -c" with the
// working directory set to worktreePath. The provided context can be used to
// cancel in-flight hooks (e.g. when the worktree is being cleaned up).
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
			cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
			cmd.Dir = worktreePath
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				log.InfoLog.Printf("post-worktree hooks cancelled for %s", worktreePath)
				return
			}
			if err != nil {
				log.ErrorLog.Printf("post-worktree hook %q failed: %v\n%s", cmdStr, err, string(output))
			} else {
				log.InfoLog.Printf("post-worktree hook %q completed successfully", cmdStr)
			}
		}
	}()
}
