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
// Audit, path by path (the order each takes):
//
//	CreateSession        start -> admission (bounded) -> m.mu
//	kill                 claim -> op -> start (persists)
//	archive              op -> start (persists)
//	prune apply, resume  op -> start -> claim; takes NO admission (it selects no
//	                     branch or path), every wait bounded
//	title reuse/rename   inside CreateSession: start -> m.mu
//	archived restore     op -> admission (bounded) -> claim; persists made while
//	                     admission is held use persistUnderAdmission (no start
//	                     lock), the rest run after admission is released (#6198)
//	automatic Lost restore  try-admission (non-blocking), released right after the
//	                     rebuild; its persists under admission use
//	                     persistUnderAdmission (#6198)
//
// A path that needs the start lock (a persist through persistInstanceErr) must not
// be holding admission when it asks for it: that is the inversion CreateSession's
// start -> admission order deadlocks against (#6197, fixed by #6198).
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

// rowWriteLockForRepo returns the repository's row-write lock, the innermost
// leaf of the lock order: op lock -> start lock -> admission -> m.mu, and this
// lock below all of them. It is held only around "re-check the op fence,
// snapshot the row, persistInstanceData" and never while acquiring another
// daemon lock (only the instances.json file lock is taken inside it).
//
// It exists because the admission writers (persistUnderAdmission,
// persistSettlementUnderAdmission) cannot take the start lock, so they are not
// serialized against a poll that already passed its OpNone gate under the start
// lock: the poll's older whole-row snapshot could be persisted AFTER the
// restore's targeted write and regress it (#6210). The poll holds this lock from
// its final fence check through its write, so either it sees the restore's fence
// and cedes, or the restore's write queues behind it and lands newer.
func (m *Manager) rowWriteLockForRepo(repoID string) *sync.Mutex {
	if v, ok := m.repoRowLocks.Load(repoID); ok {
		return v.(*sync.Mutex)
	}
	v, _ := m.repoRowLocks.LoadOrStore(repoID, &sync.Mutex{})
	return v.(*sync.Mutex)
}
