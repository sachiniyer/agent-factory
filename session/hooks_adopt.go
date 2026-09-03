package session

import "github.com/sachiniyer/agent-factory/session/git"

// AdoptRunningHookRuns hands every restored session's worktree to the hook
// adopter, so a post_worktree_commands run that outlived the daemon generation
// that started it is reported as in flight instead of vanishing with that
// daemon (#3682).
//
// It is deliberately a restore-time call and not part of materializing an
// Instance. FromInstanceData also serves lookups and duplicate materializations,
// and it runs once per row; asking the systemd user manager there would put a
// round trip on paths that have nothing to restore. The adopter itself takes the
// whole set at once for the same reason.
//
// Callers must invoke it before the instances are published, which is what makes
// the worktree's own unsynchronized hooksDone write a happens-before edge rather
// than a race.
func AdoptRunningHookRuns(instances []*Instance) {
	worktrees := make([]*git.GitWorktree, 0, len(instances))
	for _, instance := range instances {
		if instance == nil {
			continue
		}
		instance.mu.RLock()
		gw := instance.gitWorktree
		instance.mu.RUnlock()
		if gw != nil {
			worktrees = append(worktrees, gw)
		}
	}
	git.AdoptRunningHooks(worktrees)
}
