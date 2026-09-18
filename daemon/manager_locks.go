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
func (m *Manager) startLockForRepo(repoID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.repoStartLocks == nil {
		// Lazily populated like refutedLedgerAccounts — a Manager built outside
		// the constructor (test shells) gets the same guarantee without a nil
		// map write.
		m.repoStartLocks = make(map[string]*sync.Mutex)
	}
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
