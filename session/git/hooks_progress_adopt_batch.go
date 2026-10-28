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
	abandon   bool
}

var hookProgressBatchReadFinished = func() {}

// Restore probes every journal concurrently and spends one identity deadline
// across the batch. A read that misses the budget is installed as a pending
// watcher before restore publishes the worktree; the watcher retries under the
// worktree's lifecycle context without delaying unrelated restored sessions.
func reconcileHookProgressBatch(worktrees, terminal []*GitWorktree) map[*GitWorktree]bool {
	adopted := make(map[*GitWorktree]bool, len(worktrees))
	total := len(worktrees) + len(terminal)
	if total == 0 {
		return adopted
	}
	ctx, cancel := context.WithTimeout(context.Background(), relocationIdentityTimeout)
	defer cancel()
	results := make(chan hookProgressAdoptionRead, total)
	pending := make(map[*GitWorktree]hookProgressAdoptionRead, total)
	startRead := func(g *GitWorktree, abandon bool) {
		read := hookProgressAdoptionRead{worktree: g, path: g.worktreePath, sessionID: g.hookScopeSessionID, abandon: abandon}
		pending[g] = read
		go func() {
			defer hookProgressBatchReadFinished()
			if read.abandon {
				read.progress, _, read.err = readOwnedHookProgress(read.path, read.sessionID)
			} else {
				read.progress, read.err = readPendingHookProgress(read.path, read.sessionID)
			}
			results <- read
		}()
	}
	for _, g := range worktrees {
		startRead(g, false)
	}
	for _, g := range terminal {
		startRead(g, true)
	}
	for len(pending) > 0 {
		select {
		case read := <-results:
			if _, waiting := pending[read.worktree]; !waiting {
				continue
			}
			delete(pending, read.worktree)
			installHookProgressRestoreRead(adopted, read)
		case <-ctx.Done():
			for _, read := range pending {
				read.err = fmt.Errorf("shared hook journal restore budget expired: %w", ctx.Err())
				installHookProgressRestoreRead(adopted, read)
			}
			return adopted
		}
	}
	return adopted
}

func installHookProgressRestoreRead(adopted map[*GitWorktree]bool, read hookProgressAdoptionRead) {
	if read.abandon {
		read.worktree.installHookProgressAbandonment(read.path, read.sessionID, read.progress, read.err)
		return
	}
	adopted[read.worktree] = read.worktree.installHookProgressAdoption(read.path, read.sessionID, read.progress, read.err)
}
