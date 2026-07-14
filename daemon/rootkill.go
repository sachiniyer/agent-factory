package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

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
// failure mid-kill is never classified Lost and restored. Best-effort by
// design: a failed write only degrades to the pre-tombstone crash window.
func (m *Manager) persistKillTombstone(repoID string, instance *session.Instance, data *session.InstanceData) {
	var d session.InstanceData
	switch {
	case instance != nil:
		instance.MarkUserKilled()
		d = instance.ToInstanceData()
	case data != nil:
		d = *data
		d.UserKilled = true
	default:
		return
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	err := persistInstanceData(repoID, d)
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("failed to persist kill tombstone for %q: %v", d.Title, err)
	}
}

// finishUserKill completes the teardown of a session whose record carries the
// kill-intent tombstone (#1108): the previous KillSession was interrupted by a
// daemon crash or a teardown error after the tombstone write. Mirrors the tail
// of KillSession — best-effort Kill, targeted record delete, map removal — and
// retries on the next poll if the record delete fails. Skips while an explicit
// KillSession for the same session is still in flight.
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
	// Best-effort: the backing tmux session is typically already gone; Kill
	// failures here only mean there is less left to tear down.
	if err := instance.Kill(); err != nil {
		log.WarningLog.Printf("finishing kill of %q: teardown reported: %v", instance.Title, err)
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
	m.mu.Unlock()
}
