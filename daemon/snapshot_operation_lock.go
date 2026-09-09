package daemon

import "sync"

// snapshotOperationLockHeld projects whether a session operation has already
// been admitted but may not yet have raised its lifecycle fence. Snapshot only
// needs an instant observation: a free lock is acquired and released immediately,
// while a failed TryLock conservatively reports ownership even if the holder is
// concurrently finishing.
func snapshotOperationLockHeld(lock *sync.Mutex) *bool {
	held := false
	if lock != nil {
		if lock.TryLock() {
			lock.Unlock()
		} else {
			held = true
		}
	}
	return &held
}
