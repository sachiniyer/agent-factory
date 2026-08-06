package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// claimRestoreOperation makes restore admission atomic with DeleteProject's
// per-repo lifecycle fence. If restore wins, killsInFlight makes deletion see
// the session even while an archived row has not yet entered OpRestoring. If
// deletion wins, projectDeletes refuses the restore before any state/worktree
// mutation. The legacy killsInFlight name covers all exclusive lifecycle ops.
func (m *Manager) claimRestoreOperation(repoID, key, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, deleting := m.projectDeletes[repoID]; deleting {
		return fmt.Errorf("cannot restore session %q: project %s is being deleted; retry after deletion finishes", title, repoID)
	}
	if _, busy := m.killsInFlight[key]; busy {
		return fmt.Errorf("an operation is already in progress for session %q", title)
	}
	if m.restoresInFlight == nil {
		m.restoresInFlight = make(map[string]struct{})
	}
	m.killsInFlight[key] = struct{}{}
	m.restoresInFlight[key] = struct{}{}
	return nil
}

func (m *Manager) releaseRestoreOperation(key string) {
	m.mu.Lock()
	delete(m.killsInFlight, key)
	delete(m.restoresInFlight, key)
	m.mu.Unlock()
}

// RestoreSession restores a user-restorable session regardless of how it became
// unavailable: archived rows use the archive restore path, while Lost/Dead rows
// run the same Recover path the daemon's automatic Lost loop uses. It returns the
// canonical stable identity that the operation resolved for event publication.
// It returns the canonical identity resolved for the operation so the lifecycle
// event cannot be reconstructed from stale request display fields. A non-empty
// stable ID is authoritative: a stale queued action fails instead of falling
// through to a same-title row.
func (m *Manager) RestoreSession(req RestoreSessionRequest) (string, session.InstanceData, error) {
	instance, repoID, title, resolvedID, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	resolved := session.InstanceData{ID: resolvedID, Title: title}
	if instance == nil {
		return "", session.InstanceData{}, fmt.Errorf("cannot restore session %q: no such session", title)
	}

	switch instance.GetLiveness() {
	case session.LiveArchived:
		path, restoreErr := m.restoreArchivedInstance(instance, repoID, title)
		return path, resolved, restoreErr
	case session.LiveLost, session.LiveDead:
		path, restoreErr := m.restoreLostOrDeadSession(repoID, title, instance, req.ForceReap)
		return path, resolved, restoreErr
	default:
		return "", session.InstanceData{}, fmt.Errorf("session %q is not archived, lost, or dead", title)
	}
}

func (m *Manager) restoreLostOrDeadSession(repoID, title string, instance *session.Instance, force bool) (string, error) {
	if err := instance.ValidateRuntimeAction(session.RuntimeActionRestoreLostOrDead); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}
	if session.IsReservedTitle(instance.Title) {
		return "", fmt.Errorf("cannot manually restore reserved session %q", title)
	}
	if !instance.Capabilities().Recover {
		return "", fmt.Errorf("cannot restore remote session %q: reconnect is not supported", title)
	}

	key := daemonInstanceKey(repoID, title)
	if err := m.claimRestoreOperation(repoID, key, title); err != nil {
		return "", err
	}
	defer m.releaseRestoreOperation(key)

	opLock, err := m.lockSessionOperationWithin(key, "restore", title)
	if err != nil {
		return "", err
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance {
		return "", fmt.Errorf("session %q changed state before restore could start", title)
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
		return "", fmt.Errorf("session %q changed state before restore could start", title)
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
	switch m.remoteSandboxLiveness(instance) {
	case probeAlive:
		log.InfoLog.Printf("not re-provisioning session %q: its sandbox answers as alive, so it was never lost — clearing the Lost mark instead (re-provisioning would orphan it and discard unpushed work)", title)
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning))
		// Both of these are per-runtime state and both are filed under the session's
		// stable identity, never under the repo/title `key` above (#2868).
		stateKey := stableSessionKey(repoID, instance)
		m.clearRemoteLoss(stateKey)
		m.persistInstance(repoID, instance)
		m.mu.Lock()
		delete(m.lostRestoreStates, stateKey)
		m.mu.Unlock()
		return instance.GetWorktreePath(), nil
	case probeUnknown:
		// Unreachable is not gone. Refuse, and NAME the release — a guard that blocks
		// without saying how to get past it is #2917. Which means honoring that
		// release HERE: a refusal whose advertised retry lands on the same branch and
		// refuses again is the same defect wearing a helpful message.
		if !force {
			return "", refuseIndeterminateReap(instance)
		}
		// Forced past an unanswerable probe: the sandbox may well be alive behind a
		// broken path, so a replacement still must not land on the default branch and
		// strand work it had already PUSHED — which is not what the flag offered to
		// discard. Unlike probeAbsent below, "gone" was never established here.
		if err := requireDurableSandboxBranch(repoID, instance); err != nil {
			return "", err
		}
		log.WarningLog.Printf("restore of %q: --force-reap given past an indeterminate probe; af could not reach the sandbox to push it, so anything it holds unpushed is discarded", title)
	case probeAbsent:
		// af's own not-provisioned sentinel: nothing to preserve, so replacement is
		// unconditional. The only arm that licenses that.
	case probeAnsweredDead:
		// It ANSWERED: the agent is gone, the sandbox is not. Push its work to origin
		// before anything replaces it, and refuse outright if that push does not
		// land — the same order and the same refusal ArchiveSandbox uses.
		//
		// --force-reap still ATTEMPTS the push. The flag means "a failed push must
		// not stop you", not "do not try": the push is also the only thing that
		// learns this session's branch from the sandbox, and skipping it would make
		// the replacement clone the default branch and strand work the operator had
		// already pushed — which the flag never offered to discard.
		if force {
			// Do NOT push. The flag's promise is that the sandbox is replaced without
			// publishing what it holds, and archive does not merely push commits — it
			// snapshots uncommitted files into one first. Pushing here would upload
			// exactly the material the operator chose to discard, which is worse than
			// the data loss the default path prevents.
			//
			// The branch therefore has to be known already, and the guard below is what
			// makes that a refusal rather than a default-branch clone.
			if err := requireDurableSandboxBranch(repoID, instance); err != nil {
				return "", err
			}
			log.WarningLog.Printf("restore of %q: --force-reap given, replacing its reachable sandbox without pushing; anything it has not pushed is discarded", title)
			break
		}
		if err := m.preserveSandboxBeforeReap(repoID, key, instance, forceReapSuggestionFor(instance)); err != nil {
			return "", err
		}
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
	// A SETTLEMENT, not a checkpoint (#2883). Recover can have rebuilt the
	// worktree and recreated the branch, flipping branchCreatedByUs — the flag
	// that authorizes deleting it later. No poll rewrites it (the row is already
	// LiveRunning, so persistPollChange sees no change), so a lost write reverts
	// the flag on the next start and the branch is orphaned for good.
	//
	// Unlike the automatic loop, this path HAS a caller, so the failure is
	// RETURNED rather than logged. The retry set is a backstop, not a reason to
	// stay quiet: it only helps if the daemon lives long enough to drain it, and
	// the user is the one who can free the disk. The session really is restored,
	// so the message has to say both things — arming the confirm-alive gate below
	// first, because the runtime is live either way and the restore-state
	// bookkeeping must not depend on whether the write landed.
	settleErr := m.persistSettlement(repoID, key, instance)
	// A manual restore is the same lifecycle event as an automatic one; only the
	// trigger differs (#1794) — so it must arm the SAME confirm-alive gate #1923 put
	// on the auto path, NOT clear the retry state on spawn success. The unconditional
	// delete that used to be here reset the exponential backoff, so manually restoring
	// a flapping session (whose agent exits on startup) re-opened the very hot-loop the
	// auto path now prevents (#1976). consecutiveFailures is CARRIED; RestoreLostSessions
	// clears the state once a poll observes the runtime alive, and the auto loop charges
	// an immediate re-loss against the same episode.
	m.armRestoreConfirmation(repoID, instance)
	if settleErr != nil {
		return instance.GetWorktreePath(), fmt.Errorf(
			"session %q was restored and its agent is running, but its recovered state could not be written to disk: %w",
			title, settleErr)
	}
	return instance.GetWorktreePath(), nil
}
