package session

import "sync"

// HoldHookProgressUntilCreateSettled bridges local worktree provisioning and
// the daemon's later owner-row commit. The returned release is safe to call on
// both the success path and deferred abort cleanup.
func (i *Instance) HoldHookProgressUntilCreateSettled() func() {
	i.mu.RLock()
	worktree := i.gitWorktree
	i.mu.RUnlock()
	if worktree == nil {
		return func() {}
	}
	worktree.BeginHookCreatePersistence()
	var once sync.Once
	return func() {
		once.Do(worktree.SettleHookCreatePersistence)
	}
}
