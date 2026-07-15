package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
)

// RestoreSession restores a user-restorable session regardless of how it became
// unavailable: archived rows use the archive restore path, while Lost/Dead rows
// run the same Recover path the daemon's automatic Lost loop uses.
func (m *Manager) RestoreSession(req RestoreSessionRequest) (string, error) {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("cannot restore session %q: no such session", req.Title)
	}

	switch instance.GetLiveness() {
	case session.LiveArchived:
		return m.RestoreArchived(RestoreArchivedRequest{Title: req.Title, RepoID: repoID})
	case session.LiveLost, session.LiveDead:
		return m.restoreLostOrDeadSession(req, repoID, instance)
	default:
		return "", fmt.Errorf("session %q is not archived, lost, or dead", req.Title)
	}
}

func (m *Manager) restoreLostOrDeadSession(req RestoreSessionRequest, repoID string, instance *session.Instance) (string, error) {
	if instance.UserKilled() {
		return "", fmt.Errorf("cannot restore session %q: it is being deleted", req.Title)
	}
	if session.IsReservedTitle(instance.Title) {
		return "", fmt.Errorf("cannot manually restore reserved session %q", req.Title)
	}
	if !instance.Capabilities().Recover {
		return "", fmt.Errorf("cannot restore remote session %q: reconnect is not supported", req.Title)
	}
	if !instance.Started() {
		return "", fmt.Errorf("cannot restore session %q: it is not started", req.Title)
	}

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return "", fmt.Errorf("an operation is already in progress for session %q", req.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance {
		return "", fmt.Errorf("session %q changed state before restore could start", req.Title)
	}
	switch instance.GetLiveness() {
	case session.LiveLost:
	case session.LiveDead:
		_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
	default:
		return "", fmt.Errorf("session %q changed state before restore could start", req.Title)
	}

	if err := instance.Recover(); err != nil {
		m.persistInstance(repoID, instance)
		return "", err
	}
	m.persistInstance(repoID, instance)
	// Same lifecycle reset as the automatic loop (lostrestore.go): recovery
	// REPLACED the runtime the debounce's failures were about, so the count now
	// describes a sandbox that no longer exists. Left behind it stays
	// threshold-satisfying, and the first transport blip against the sandbox this
	// restore just provisioned would re-satisfy it instantly and re-provision
	// AGAIN — orphaning the one the user just asked for. A manual restore is the
	// same lifecycle event as an automatic one; only the trigger differs (#1794).
	m.noteRuntimeReplaced(repoID, instance)
	m.mu.Lock()
	delete(m.lostRestoreStates, key)
	m.mu.Unlock()
	return instance.GetWorktreePath(), nil
}
