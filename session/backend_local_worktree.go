package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
)

// the worktree lives.
func (i *Instance) newWorktreeHandle() (*git.GitWorktree, string, error) {
	var (
		gitWorktree *git.GitWorktree
		branchName  string
		err         error
	)
	if i.inPlace {
		// --here: attach to the repo's own working tree at its current
		// branch. The worktree is external, so Setup() below is a no-op
		// and Cleanup()/Kill never removes the user's tree or branch.
		gitWorktree, branchName, err = git.NewGitWorktreeInPlace(i.Path)
	} else {
		branchPrefix := i.branchPrefix
		if i.branchName == "" && !i.branchPrefixResolved {
			var cfg *config.Config
			cfg, err = config.LoadConfig()
			if err != nil {
				err = fmt.Errorf("failed to load config: %w", err)
			} else {
				branchPrefix = cfg.BranchPrefix
			}
		}
		if err == nil {
			if i.branchName != "" {
				// The daemon already named this branch (#5571 W2.4).
				gitWorktree, branchName, err = git.NewGitWorktreeNamed(i.Path, i.Title, i.branchName)
			} else {
				gitWorktree, branchName, err = git.NewGitWorktree(i.Path, i.Title, branchPrefix)
			}
		}
	}
	return gitWorktree, branchName, err
}
