package git

import (
	"context"
	"fmt"
)

type hookProgressAdoptionRead struct {
	worktree  *GitWorktree
	path      string
	sessionID string
	progress  *hookProgress
	err       error
}

// Restore probes every journal concurrently and spends one identity deadline
// across the batch. A read that misses the budget is installed as a pending
// watcher before restore publishes the worktree; the watcher retries under the
// worktree's lifecycle context without delaying unrelated restored sessions.
func adoptHookProgressBatch(worktrees []*GitWorktree) map[*GitWorktree]bool {
	adopted := make(map[*GitWorktree]bool, len(worktrees))
	if len(worktrees) == 0 {
		return adopted
	}
	ctx, cancel := context.WithTimeout(context.Background(), relocationIdentityTimeout)
	defer cancel()
	results := make(chan hookProgressAdoptionRead, len(worktrees))
	pending := make(map[*GitWorktree]hookProgressAdoptionRead, len(worktrees))
	for _, g := range worktrees {
		read := hookProgressAdoptionRead{worktree: g, path: g.worktreePath, sessionID: g.hookScopeSessionID}
		pending[g] = read
		go func() {
			read.progress, read.err = readPendingHookProgress(read.path, read.sessionID)
			results <- read
		}()
	}
	for len(pending) > 0 {
		select {
		case read := <-results:
			if _, waiting := pending[read.worktree]; !waiting {
				continue
			}
			delete(pending, read.worktree)
			adopted[read.worktree] = read.worktree.installHookProgressAdoption(read.path, read.sessionID, read.progress, read.err)
		case <-ctx.Done():
			for g, read := range pending {
				err := fmt.Errorf("shared hook journal restore budget expired: %w", ctx.Err())
				adopted[g] = g.installHookProgressAdoption(read.path, read.sessionID, nil, err)
			}
			return adopted
		}
	}
	return adopted
}
