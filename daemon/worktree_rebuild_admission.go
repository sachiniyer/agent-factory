package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// lockWorktreeAdmissionWithin is the waiting form of repository worktree
// admission. Every holder may run an intentionally unbounded Git worktree add
// and its operator hook, so no peer may use a bare Mutex.Lock and inherit that
// unbounded wait. Callers acquire before their own mutation or reservation and
// can therefore return untouched when this bound expires.
func (m *Manager) lockWorktreeAdmissionWithin(repoID string) (*sync.Mutex, time.Duration, bool) {
	lock := m.worktreeAdmissionLockForRepo(repoID)
	acquired, waited := lockWithin(lock, opLockTimeout)
	return lock, waited, acquired
}

// lockLocalWorktreeAdmissionWithin joins a user-requested local restore to the
// same per-repository admission boundary as create. The mutex is acquired only
// while the caller holds the target session's operation lock and before it
// claims or fences that session, so a stalled create can delay this request only
// up to opLockTimeout and cannot leave the row advertised as busy indefinitely.
// A non-local backend cannot register a local worktree and needs no admission.
func (m *Manager) lockLocalWorktreeAdmissionWithin(
	repoID, title, operation string,
	instance *session.Instance,
) (*sync.Mutex, error) {
	if instance.Capabilities().Workspace != session.WorkspaceLocalWorktree {
		return nil, nil
	}
	lock, waited, acquired := m.lockWorktreeAdmissionWithin(repoID)
	if !acquired {
		return nil, fmt.Errorf(
			"cannot %s session %q: timed out after %s waiting for another worktree operation in this repository; retry after that operation finishes",
			operation, title, waited,
		)
	}
	return lock, nil
}

// tryLocalWorktreeAdmission is the non-blocking form for daemon poll work.
// LocalBackend has exactly one missing-worktree rebuild choke point,
// respawnWithConversation. Its callers are Lost recovery and limit/account
// resume; each daemon entry takes this admission before it can reach that
// choke point. A contended poll attempt skips and retries on a later tick rather
// than delaying unrelated status and self-healing work.
func (m *Manager) tryLocalWorktreeAdmission(repoID string, instance *session.Instance) (*sync.Mutex, bool) {
	if instance.Capabilities().Workspace != session.WorkspaceLocalWorktree {
		return nil, true
	}
	lock := m.worktreeAdmissionLockForRepo(repoID)
	return lock, lock.TryLock()
}

func unlockWorktreeAdmission(lock *sync.Mutex) {
	if lock != nil {
		lock.Unlock()
	}
}
