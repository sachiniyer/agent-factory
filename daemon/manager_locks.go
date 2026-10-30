package daemon

import "sync"

// startLockForRepo returns the per-repo lock serializing session/tab creation
// against other mutations of that repo, lazily creating it.
//
// LOCK CONTRACT (#2106): it takes m.mu, so it must NEVER be called with m.mu
// already held — sync.Mutex is not reentrant and the goroutine would deadlock on
// the manager lock, stalling every other operation behind it. That rules out
// calling it, m.persistInstance, or m.persistInstanceErr from any `...Locked`
// helper or other code running under m.mu; persist from there with the lock-free
// persistInstanceData instead, which takes only the instances.json file lock.
//
// Acquiring the returned lock while holding m.mu is likewise forbidden: the
// established order is repoStartLock BEFORE m.mu (CreateSession holds the start
// lock across its body and takes m.mu under it), so the reverse closes an ABBA
// cycle — the #2006 lock-inversion class.
//
// GLOBAL LOCK ORDER for session lifecycle work. Take left to right, release in
// reverse, and never take a lock to the LEFT of one already held; a wait on a lock
// to the right must be bounded:
//
//	per-session operation lock (opLockFor)
//	  -> repository start lock (this lock)
//	    -> repository worktree admission lock (worktreeAdmissionLockForRepo)
//	      -> m.mu (never held while waiting for any lock to its left)
//
// Audit, path by path (the order each actually takes; (!) = deviates):
//
//	CreateSession        start -> admission (bounded) -> m.mu
//	kill                 claim -> op -> start (persists)
//	archive              op -> start (persists)
//	prune apply, resume  op -> start -> claim; takes NO admission (it selects no
//	                     branch or path), every wait bounded
//	title reuse/rename   inside CreateSession: start -> m.mu
//	archived restore (!) op -> admission (bounded) -> claim; its body then persists
//	                     (start) while still holding admission: admission -> start
//	automatic Lost restore (!) try-admission (non-blocking) -> persists (start)
//
// The two (!) paths predate prune and invert admission/start against CreateSession;
// the cycle is broken today only because CreateSession's admission wait is bounded.
// Prune is outside that cycle by construction (no admission). Giving restore the
// canonical order means holding the start lock across its body and persisting
// lock-free — a change to that path's contract, tracked separately (#6197).
func (m *Manager) startLockForRepo(repoID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock := m.repoStartLocks[repoID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.repoStartLocks[repoID] = lock
	}
	return lock
}

// opLockFor returns the per-session operation lock serializing kill teardown
// against Lost-recovery and prompt writes for one daemon instance key (#1108 PR
// 2, #1473).
func (m *Manager) opLockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.instanceOpLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.instanceOpLocks[key] = lock
	}
	return lock
}
