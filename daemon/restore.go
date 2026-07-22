package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
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
	if err := instance.ValidateRuntimeAction(session.RuntimeActionRestoreLostOrDead); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}
	if session.IsReservedTitle(instance.Title) {
		return "", fmt.Errorf("cannot manually restore reserved session %q", req.Title)
	}
	if !instance.Capabilities().Recover {
		return "", fmt.Errorf("cannot restore remote session %q: reconnect is not supported", req.Title)
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
	view := instance.LifecycleView()
	if err := view.ValidateRuntimeAction(session.RuntimeActionRestoreLostOrDead); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}
	switch view.Liveness {
	case session.LiveLost:
	case session.LiveDead:
		_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
	default:
		return "", fmt.Errorf("session %q changed state before restore could start", req.Title)
	}

	// The same live recheck the automatic loop runs before re-provisioning
	// (lostrestore.go), for the same reason: a remote Recover is not a reconnect
	// but a fresh sandbox cloned from origin, and this row's Lost mark may be
	// minutes stale — the automatic loop backs off to 5 minutes, and the user can
	// hit restore at any moment, including while the transport is healing.
	//
	// Being user-initiated makes the recheck MORE important, not less. "Restore"
	// asks for a working session; it does not ask to discard a running sandbox and
	// everything it never pushed. If the sandbox answers, the session was never
	// really lost and healing the row delivers exactly what was asked for, without
	// the destruction. A user who genuinely wants a new sandbox kills and
	// recreates (#1794).
	if m.remoteSandboxAnswersAlive(instance) {
		log.InfoLog.Printf("not re-provisioning session %q: its sandbox answers as alive, so it was never lost — clearing the Lost mark instead (re-provisioning would orphan it and discard unpushed work)", req.Title)
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning))
		m.clearRemoteLoss(remoteLossKey(repoID, instance))
		m.persistInstance(repoID, instance)
		m.mu.Lock()
		delete(m.lostRestoreStates, key)
		m.mu.Unlock()
		return instance.GetWorktreePath(), nil
	}

	if err := instance.Recover(); err != nil {
		m.persistInstance(repoID, instance)
		m.recordLostRestoreFailure(key, repoID, instance, err, lostRestoreManual)
		return "", err
	}
	// Reset FIRST, before the persist below: recovery replaced the runtime the
	// debounce's failures were about, and the poll goroutine can probe the fresh
	// sandbox the moment Recover clears the restore fence — while this call is
	// still writing to disk. A blip in that window would be judged against the
	// dead sandbox's count. A manual restore is the same lifecycle event as an
	// automatic one; only the trigger differs (#1794).
	m.noteRuntimeReplaced(repoID, instance)
	m.persistInstance(repoID, instance)
	// A manual restore is the same lifecycle event as an automatic one; only the
	// trigger differs (#1794) — so it must arm the SAME confirm-alive gate #1923 put
	// on the auto path, NOT clear the retry state on spawn success. The unconditional
	// delete that used to be here reset the exponential backoff, so manually restoring
	// a flapping session (whose agent exits on startup) re-opened the very hot-loop the
	// auto path now prevents (#1976). consecutiveFailures is CARRIED; RestoreLostSessions
	// clears the state once a poll observes the runtime alive, and the auto loop charges
	// an immediate re-loss against the same episode.
	m.armRestoreConfirmation(key, repoID, instance)
	return instance.GetWorktreePath(), nil
}
