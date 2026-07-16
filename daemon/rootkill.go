package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// killTombstonePersist is the durable write persistKillTombstone runs. A package
// var so tests can force the write to fail in isolation — exercising the abort
// that keeps a kill from destroying a session it could not record (#1917) —
// without disturbing any other persist. Mirrors archivePersist's precedent.
// Production points it at the real writer and never reassigns it.
var killTombstonePersist = persistInstanceData

func killTargetStableID(instance *session.Instance, data *session.InstanceData) string {
	if instance != nil {
		return instance.ID
	}
	if data != nil {
		return data.ID
	}
	return ""
}

func (m *Manager) currentInstanceReplaced(key string, target *session.Instance, targetID string) bool {
	if targetID == "" {
		return false
	}
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	return current != nil && current != target && current.ID != "" && current.ID != targetID
}

func stableIDMatchesForDaemon(recordID, expectedID string) bool {
	return expectedID == "" || recordID == "" || recordID == expectedID
}

// persistKillTombstone writes the kill-intent tombstone (#1108) for the session
// KillSession is about to tear down, so a record surviving a crash or teardown
// failure mid-kill is never classified Lost and restored. It returns the write
// error: the tombstone is the kill's COMMIT POINT, so the caller must abort
// before teardown rather than destroy a session whose kill it could not record.
//
// It was best-effort ("a failed write only degrades to the pre-tombstone crash
// window") — which is no longer a defensible trade now that the write can fail on
// lock contention rather than only on a disk fault (#1917). Without a durable
// tombstone, a daemon that dies between teardown and the record delete reloads a
// non-tombstoned record whose tmux is gone, classifies it Lost, and RESTORES it —
// resurrecting a session the user explicitly killed, in a worktree that teardown
// already deleted. Refusing a kill we cannot record is recoverable; that is not.
//
// The in-memory flag is set only AFTER the write lands, and the tombstone data is
// built without mutating the instance. Marking first would leave a session the
// poll's refreshInstanceStatus routes to finishUserKill — completing, on the next
// tick, the very kill this function just refused, and defeating the abort.
func (m *Manager) persistKillTombstone(repoID string, instance *session.Instance, data *session.InstanceData) error {
	var d session.InstanceData
	switch {
	case instance != nil:
		d = instance.ToInstanceData()
		d.UserKilled = true
	case data != nil:
		d = *data
		d.UserKilled = true
	default:
		return nil
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	err := killTombstonePersist(repoID, d)
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("failed to persist kill tombstone for %q: %v", d.Title, err)
		return err
	}
	// Durable now: the kill is committed, so the in-memory flag can safely follow.
	if instance != nil {
		instance.MarkUserKilled()
	}
	return nil
}

// finishUserKill completes the teardown of a session whose record carries the
// kill-intent tombstone (#1108): the previous KillSession was interrupted by a
// daemon crash or a teardown error after the tombstone write. Mirrors the tail
// of KillSession — best-effort Kill, targeted record delete, map removal, and
// the root kill grace window (#1844) — and retries on the next poll if the
// record delete fails. Skips while an explicit KillSession for the same session
// is still in flight. Anything KillSession's tail learns to do after the
// tombstone write belongs here too: the tombstone means the user's kill is
// already committed, so the two paths must reach the same end state.
func (m *Manager) finishUserKill(repoID string, instance *session.Instance) {
	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	// TryLock, not Lock: this runs on the poll goroutine, which must not
	// stall behind a concurrent slow operation on this session; the next
	// poll retries. (A KillSession in flight was already skipped above, so
	// contention here is only a still-releasing lock.)
	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance {
		return
	}

	log.WarningLog.Printf("finishing interrupted kill of session %q (tombstoned record survived its teardown)", instance.Title)
	// Kill's own best-effort handling already swallows every failure tmux or git
	// ANSWERED with, so anything that reaches here is a teardown that could not be
	// completed SAFELY — a pane whose liveness is unknown, or a worktree whose
	// removal was cut off mid-delete. Deleting the record anyway would strand the
	// worktree and take away the user's only handle on it, so keep the record and
	// let the next poll try again: this loop IS the retry, and it is the reason a
	// bounded teardown does not need a daemon restart to converge (#1917).
	if err := instance.Kill(); err != nil {
		log.WarningLog.Printf("finishing kill of %q: teardown could not complete safely; keeping the record and retrying next poll: %v", instance.Title, err)
		return
	}
	storage, err := session.NewStorage(config.LoadState(), repoID)
	if err != nil {
		log.WarningLog.Printf("finishing kill of %q: %v", instance.Title, err)
		return
	}
	deleted, err := storage.DeleteInstanceByStableID(instance.Title, instance.ID)
	if err != nil {
		log.WarningLog.Printf("finishing kill of %q: failed to delete record (will retry next poll): %v", instance.Title, err)
		return
	}
	if !deleted {
		log.InfoLog.Printf("finishing kill of %q skipped storage delete: current record has a different instance identity", instance.Title)
		return
	}
	m.mu.Lock()
	if m.instances[key] == instance {
		delete(m.instances, key)
	}
	if session.IsReservedTitle(instance.Title) {
		// Arm the grace window the interrupted KillSession never reached
		// (#1844). Without this the ensure loop sees no rootKilledAt and
		// re-creates the root on the next tick, so a kill that happened to be
		// interrupted is honored for zero seconds while an uninterrupted one is
		// honored for rootKillHealDelay. Timed from the finish, not the
		// original kill: the tombstone records intent, not when it was formed,
		// and re-arming here is what the surviving intent is owed. Still only a
		// delay — the loop self-heals a configured root afterwards (#1223).
		m.rootKilledAt[repoID] = nowFunc()
		log.InfoLog.Printf("root agent for repo %s: finished an interrupted user kill; the ensure loop will re-create it in ~%s unless the repo is removed from root_agents", repoID, rootKillHealDelay)
	}
	m.mu.Unlock()
}
