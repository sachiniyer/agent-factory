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

// admissionReleaser returns an idempotent release for an admission lock, so a
// restore can hand admission back right after the rebuild choke point and still
// defer the same release for its early returns (#6197). A nil lock is a no-op.
func admissionReleaser(lock *sync.Mutex) func() {
	var once sync.Once
	return func() { once.Do(func() { unlockWorktreeAdmission(lock) }) }
}

func unlockWorktreeAdmission(lock *sync.Mutex) {
	if lock != nil {
		lock.Unlock()
	}
}

// rebuildTeardownNotify returns the synchronous durable-checkpoint callback
// every respawn entry point installs before Recover can arm or discharge the
// rebuild-teardown obligation. It deliberately bypasses persistInstanceErr's
// repoStartLock: every entry point that reaches the notify holds this repo's
// worktree-admission lock, while CreateSession takes repoStartLock BEFORE
// waiting on that admission — the reverse order here closes an ABBA cycle,
// with recovery blocked on the start lock (unbounded) while holding the
// admission create needs, so the create's bounded wait fails it ~opLockTimeout
// later and stalls the poll's own recovery lane (#5371 review).
//
// persistInstanceData is safe here for the same reason the lock contract
// names it for callers under m.mu: it never re-enters m.mu or repoStartLock
// and takes only the instances.json file lock, which is what actually
// serializes writers of the row. No stale whole-row snapshot can clobber the
// flag either — the notify fires only under the recover/respawn op fence
// (OpRespawning/OpRestoring), the op axis already excludes the poll's
// whole-row persist (the data.InFlightOp gate in settleInstancePoll), and
// every mutation writer for the row is action-gated while the op is up.
func (m *Manager) rebuildTeardownNotify(repoID string) func(*session.Instance) error {
	return func(i *session.Instance) error {
		return persistInstanceData(repoID, i.ToInstanceData())
	}
}
