package session

import (
	"sync"

	"github.com/sachiniyer/agent-factory/session/git"
)

// HoldHookProgressUntilCreateSettled bridges local worktree provisioning and
// the daemon's later owner-row commit. The returned release is safe to call on
// both the success path and deferred abort cleanup.
func (i *Instance) HoldHookProgressUntilCreateSettled() func() {
	i.mu.Lock()
	i.hookCreatePersistencePending = true
	if i.gitWorktree != nil {
		i.gitWorktree.BeginHookCreatePersistence()
	}
	i.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			i.mu.Lock()
			i.hookCreatePersistencePending = false
			worktree := i.gitWorktree
			i.mu.Unlock()
			if worktree != nil {
				worktree.SettleHookCreatePersistence()
			}
		})
	}
}

// setGitWorktreeLocked publishes a newly provisioned worktree into the create
// transaction. The daemon asks for the pre-commit hold before local Provision
// constructs this object, so assignment is the point that must arm it.
func (i *Instance) setGitWorktreeLocked(worktree *git.GitWorktree) {
	if i.gitWorktree == worktree {
		return
	}
	i.gitWorktree = worktree
	if worktree != nil && i.hookCreatePersistencePending {
		worktree.BeginHookCreatePersistence()
	}
	i.touchLocked()
}
