package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// beforeRestoreOperationLock fires immediately before either manual restore path
// waits for the session's per-session operation lock. No-op in production; the
// #3600 tests substitute a blocking stand-in so they can assert what the daemon
// advertises and admits while a restore is parked in front of that lock.
var beforeRestoreOperationLock = func() {}

// claimRestoreOperation makes restore admission atomic with DeleteProject's
// per-repo lifecycle fence. If restore wins, killsInFlight makes deletion see
// the session even while an archived row has not yet entered OpRestoring. If
// deletion wins, projectDeletes refuses the restore before any state/worktree
// mutation. The legacy killsInFlight name covers all exclusive lifecycle ops.
//
// Both callers hold the session's operation lock by the time they get here
// (#3600), which is what makes the claim adjacent to the fence raise that
// follows it. The atomicity above is unchanged — this still runs under m.mu and
// still reads projectDeletes there — but it is now decided at the END of a wait
// that may have taken up to opLockTimeout, so `waited` is folded into the
// refusals. A delay that says nothing about itself reads as a hang.
func (m *Manager) claimRestoreOperation(repoID, key, title string, waited time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, deleting := m.projectDeletes[repoID]; deleting {
		return fmt.Errorf("cannot restore session %q%s: project %s is being deleted; retry after deletion finishes", title, afterOpLockWait(waited), repoID)
	}
	if _, busy := m.killsInFlight[key]; busy {
		return fmt.Errorf("an operation is already in progress for session %q%s", title, afterOpLockWait(waited))
	}
	if m.restoresInFlight == nil {
		m.restoresInFlight = make(map[string]struct{})
	}
	m.killsInFlight[key] = struct{}{}
	m.restoresInFlight[key] = struct{}{}
	return nil
}

// afterOpLockWait renders the operation-lock wait a restore refusal was decided
// after. Empty when the lock was uncontended, so the ordinary refusal keeps the
// wording it has always had — lockWithin reports zero only for an acquisition
// nobody was ahead of, never for a rounded-down real wait.
func afterOpLockWait(waited time.Duration) string {
	if waited <= 0 {
		return ""
	}
	return fmt.Sprintf(" after waiting %s for another operation on it to finish", waited.Round(time.Millisecond))
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

	// Registered BEFORE both the lock and the claim so LIFO runs it LAST — after
	// releaseRestoreOperation and after opLock.Unlock (Codex on #3597).
	//
	// Order matters because the claim is what actually refuses Kill. Lowering the
	// fence while killsInFlight is still set publishes a row saying can_kill=true to
	// every client while the admission gate would still answer "an operation is
	// already in progress" — the same false affordance this change exists to remove,
	// just narrower. Released last, the busy projection covers exactly the claim's
	// interval, and the one event that says the row is killable is published when it
	// genuinely is.
	//
	// fenceRaised is ownership, not bookkeeping: the early returns above the raise
	// include a claim that FAILED because another restore holds it, and that restore
	// may well have OpRestoring up. Releasing on the bare op value would lower a
	// fence belonging to someone else's operation.
	fenceRaised := false
	defer func() {
		if !fenceRaised {
			return
		}
		if instance.EndRecoverFence() {
			m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
		}
	}()

	// The op-lock is taken BEFORE the claim, and that order is the whole of #3600.
	//
	// Claiming first put killsInFlight up for the length of this wait — up to
	// opLockTimeout (30s), and the holder in front is routinely the automatic
	// Lost-restore loop, which owns this same lock across its own UNFENCED
	// probe/preserve phase (lostrestore.go, bounded at sandboxPushTimeout). From
	// the claim onward a Kill is refused at the admission gate with "kill already
	// in progress" — about an operation the user never started — while the row sat
	// at {LiveLost, OpNone} and canKillFor, which reads the op axis alone, went on
	// advertising that Kill. #3597 made the fence coextensive with the claim from
	// this line onward; the wait in FRONT of it was the remainder.
	//
	// Raising the fence earlier is the fix that cannot be taken: BeginRecoverFence
	// validates RuntimeActionRecoverLost, which requires OpNone, so a manual
	// restore that fenced ahead of the wait would make the automatic loop's own
	// raise REFUSE — charging a failure against that session's #1108 backoff
	// episode for a session that was not failing. Projecting a second "claimed but
	// not yet fenced" axis is the other rejected direction: the TUI rebuilds an
	// Instance through session.FromInstanceData and recomputes CanKill from the op
	// axis, so a projection-side fix either leaves TUI and web disagreeing or
	// requires making the derived InstanceData.CanKill authoritative on rebuild —
	// which storage.go's scrub exists to forbid.
	//
	// Taking the lock first removes the window instead of hiding it: the claim and
	// the raise become adjacent, so the row genuinely reaches OpRestoring before
	// any observer can see it claimed-but-unfenced, and during the wait itself it
	// is honestly unclaimed — the Kill it advertises is admitted, and bounded by
	// whoever really holds the lock.
	//
	// Nothing about that inverts a lock order. The canonical one is
	// target-before-op (#2006), and no restore path takes a target lock at all;
	// m.mu is never held across an op-lock acquisition, and holding the op-lock
	// while briefly taking m.mu is what every re-verify below this line already
	// does. The claim's atomicity with DeleteProject's projectDeletes fence is
	// preserved because the claim still reads that fence under m.mu — it just
	// reads it after the wait. What changed is which side of a delete race
	// refuses: a delete that STARTS during the wait now proceeds and this restore
	// refuses (naming the wait), where before the delete was the one turned away.
	beforeRestoreOperationLock()
	opLock, waited, err := m.lockSessionOperationWithin(key, "restore", title)
	if err != nil {
		return "", err
	}
	defer opLock.Unlock()

	if err := m.claimRestoreOperation(repoID, key, title, waited); err != nil {
		return "", err
	}
	defer m.releaseRestoreOperation(key)

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

	// Raise the restore fence HERE, not around the backend call at the bottom, so
	// it is COEXTENSIVE with the claim above (#3586).
	//
	// claimRestoreOperation already made Kill fail: from that line on, a Kill is
	// rejected at the daemon's ADMISSION gate with "kill already in progress for
	// session X" — a message about an operation the user never started. canKillFor
	// is what hides the affordance, and it reads the op axis alone, so raising the
	// fence inside the backend call — where RecoverFencedWithLiveBoundary raised it
	// before this change — left the row at {LiveLost, OpNone} through both network
	// phases below: a 5s liveness probe and a pre-reap push bounded at 3m30s. For up
	// to ~3m35s the TUI and the web UI advertised a Kill that could only fail
	// (#3533, whose fenced suffix #3534 fixed). The claim's interval and the fence's
	// interval are now the same one.
	//
	// It is raised AFTER the LiveDead -> LiveLost normalization above because the
	// fence's own precondition is LiveLost: raising it first would refuse every Dead
	// row. And it is the recover fence itself, validated and raised under one lock
	// (#3555) — not a second, weaker overlay that the backend call would then have
	// to re-validate around.
	if err := instance.BeginRecoverFence(); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}
	// ANNOUNCE the raise, the same contract #2997 gave the limit resume next door.
	// Clients gate their controls on the projected op and perform no optimistic
	// update for a CLI- or web-initiated restore, so a fence nobody is told about
	// leaves an already-connected client offering Kill for the whole operation —
	// which is the bug, seen from the client rather than from the row. The TUI's
	// snapshot tick would repair it eventually; the events plane is what makes it
	// immediate, and it is the only repair a web client gets.
	m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
	// The release registered above now owns this fence. A DEFER rather than a
	// statement on each arm, because every exit below has to lower it and a missed
	// one is worse than the bug: the poll skips any session with an op in flight and
	// every runtime action and lifecycle control refuses it, so the row would be
	// permanently busy. The arms are the probe-alive heal (settled and
	// settle-failed), the indeterminate refusal, the two durable-branch refusals,
	// the failed pre-reap push, the two failed-Recover exits, and the successful
	// restore — and the defer covers whatever is added next to that list, which an
	// enumeration would not.
	//
	// A no-op on the success path, where ConfirmLive has already cleared the op, and
	// it never disturbs an op another owner raised.
	//
	// Nothing between here and there lowers the fence early, deliberately. Every
	// snapshot published while the claim is held — preserveSandboxBeforeReap's
	// settlement, the heal arm's — therefore reports the row as busy, which is what
	// it is: Kill would be refused at the admission gate. The release announces the
	// settled, killable row once, at the end, when that is true.
	fenceRaised = true

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
		instance.ClearLostRestoreFailure()
		// Both of these are per-runtime state and both are filed under the session's
		// stable identity, never under the repo/title `key` above (#2868).
		stateKey := stableSessionKey(repoID, instance)
		m.clearRemoteLoss(stateKey)
		m.mu.Lock()
		delete(m.lostRestoreStates, stateKey)
		m.mu.Unlock()
		if settleErr := m.persistSettlement(repoID, key, instance); settleErr != nil {
			failure := fmt.Errorf(
				"session %q was confirmed alive, but its healed state could not be written to disk: %w",
				title, settleErr)
			return restoredArchiveResult(instance, instance.GetWorktreePath(), failure)
		}
		return restoredArchiveResult(instance, instance.GetWorktreePath())
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

	// Settle predecessor evidence at the exact ConfirmLive edge: late enough that
	// a failed recovery leaves its evidence intact, but before the backend can
	// lower the restore fence and expose the replacement. A failed write remains
	// owed and does not veto a replacement that is already running (#2883).
	// The fence this runs under was raised at the top of the operation, so it has
	// been hiding Kill since before the probe (#3586). The held variant re-checks
	// that precondition through the shared ledger (RuntimeActionRecoverFenced)
	// instead of re-entering RuntimeActionRecoverLost, which still requires OpNone
	// and would now refuse its own operation's fence. ConfirmLive clears the fence
	// on success; the deferred EndRecoverFence above lowers it on failure.
	if err := instance.RecoverHeldFencedWithLiveBoundary(func() {
		if perr := m.prepareRuntimeReplacement(repoID, key, instance); perr != nil {
			log.WarningLog.Printf("restore of %q reached its live boundary before predecessor evidence was durable: %v", title, perr)
		}
	}); err != nil {
		// The fence stays up through the bookkeeping below and comes down in the
		// deferred release, which is what announces it. Nothing here is distorted by
		// that ordering — persistInstanceData scrubs the in-flight op before it
		// writes, and recordLostRestoreFailure touches only the retry state.
		//
		// Error-returning, not the logging wrapper: the committed arm below must
		// not claim "recorded" for a write that failed (#3353 review), so the
		// outcome of this persist is part of the message. The plain arm keeps the
		// prior best-effort behavior, logged with persistInstance's own wording.
		persistErr := m.persistInstanceErr(repoID, instance)
		if persistErr != nil {
			log.WarningLog.Printf("failed to persist instance %q: %v", instance.Title, persistErr)
		}
		m.recordLostRestoreFailure(key, repoID, instance, err, lostRestoreManual)
		// Recovery that rebuilt the missing worktree (and possibly recreated its
		// branch) before failing has already mutated durable workspace state — the
		// git side lives on disk regardless of the record write. The session is
		// NOT restored — the message says retry — but a raw error with an empty
		// worktree_path would read as failed-nothing-committed about a mutation
		// that landed (#3236). A pre-mutation failure stays the plain error below.
		var rebuilt *session.RecoverRebuiltWorkspaceError
		if errors.As(err, &rebuilt) {
			failure := fmt.Errorf(
				"restore of session %q failed after recovery rebuilt its worktree at %s; the rebuilt state is recorded — retry the restore: %w",
				title, instance.GetWorktreePath(), err)
			if persistErr != nil {
				failure = fmt.Errorf(
					"restore of session %q failed after recovery rebuilt its worktree at %s, and the rebuilt state could not be written to disk (%v) — retry the restore once the cause clears: %w",
					title, instance.GetWorktreePath(), persistErr, err)
			}
			return instance.GetWorktreePath(), &mutationCommittedError{err: failure}
		}
		return "", err
	}
	// Reset FIRST, before the persist below: recovery replaced the runtime the
	// debounce's failures were about, and the poll goroutine can probe the fresh
	// sandbox the moment Recover clears the restore fence — while this call is
	// still writing to disk. A blip in that window would be judged against the
	// dead sandbox's count. A manual restore is the same lifecycle event as an
	// automatic one; only the trigger differs (#1794).
	// The live-boundary retirement above made the fence drop crash safe. This reset is
	// still required for transport observations accumulated while Recover ran.
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
		failure := fmt.Errorf(
			"session %q was restored and its agent is running, but its recovered state could not be written to disk: %w",
			title, settleErr)
		return restoredArchiveResult(instance, instance.GetWorktreePath(), failure)
	}
	return restoredArchiveResult(instance, instance.GetWorktreePath())
}
