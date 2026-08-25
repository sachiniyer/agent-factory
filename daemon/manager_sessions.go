package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// errSessionNotFound classifies only a resolver's authoritative miss. Its text
// is deliberately "not found" so wrapping it preserves the existing messages
// byte-for-byte. DeleteProject uses errors.Is on this in process because its own
// under-lock snapshot proves the target existed; normal KillSession callers
// still receive the same error, and the sentinel does not survive net/rpc.
var errSessionNotFound = errors.New("not found")

// KillSession tears down and deletes the resolved session. It returns the stable
// identity (id + title) of the session it ACTUALLY resolved and acted on, so the
// control server publishes the killed event for exactly that session — never the
// request's own (possibly stale) id under a cross-repo title collision (#1592
// Phase 5 follow-up).
func (m *Manager) KillSession(req KillSessionRequest) (session.InstanceData, error) {
	return m.killSessionRequestedBy(req, "internal daemon caller")
}

func (m *Manager) killSessionRequestedBy(req KillSessionRequest, requester string) (session.InstanceData, error) {
	instance, repoID, title, resolvedID, data, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return session.InstanceData{}, err
	}
	log.InfoLog.Printf("KillSession requested for session %q (id %s, repo %s) by %s", title, resolvedID, repoID, requester)
	// Canonicalize to the resolved session's title so the killsInFlight key,
	// storage delete, and event all key off the identity we actually resolved
	// (by id), not the request's title. req is a value copy, so this is local.
	req.Title = title
	resolved := session.InstanceData{ID: resolvedID, Title: title}
	targetID := killTargetStableID(instance, data)
	// Kill destroys the session unconditionally (#1579). The old unmerged-work
	// guard that refused kills with commits-not-on-base / a dirty worktree / a
	// branch mismatch was dropped by owner decision: it over-refused ordinary
	// cases — most notably squash-merged branches (whose landed commits aren't
	// ancestors of base) and worktrees checked out on a different branch than the
	// stored session branch — blocking routine cleanup. `af sessions archive`
	// remains the non-destructive, restorable default; kill just kills. The
	// worktree-ownership safety (never delete a checkout af doesn't own) is
	// unaffected — it lives in GitWorktree.Cleanup() (external/in-place worktrees
	// are a no-op there), independent of this dropped guard.

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return session.InstanceData{}, fmt.Errorf("kill already in progress for session %q", req.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		// An explicit kill supersedes any finishUserKill schedule for this session,
		// including a retired one — that is the "kill it again to retry" the
		// retirement log points the operator at (#2737).
		delete(m.killRetries, key)
		m.mu.Unlock()
	}()
	if instance == nil && data != nil && m.ghostCleanupStallActive(key, targetID) {
		return session.InstanceData{}, fmt.Errorf(
			"kill of ghost session %q was not retried because its prior descriptor cleanup is still running in this daemon process; its durable record is retained — restart the daemon before retrying",
			req.Title,
		)
	}

	// Serialize against a Lost-recovery in flight for this session (#1108
	// PR 2): a kill arriving mid-Recover waits for the recover attempt to
	// finish and then tears the (possibly just-restored) session down —
	// never an interleaved teardown-vs-respawn. killsInFlight is registered
	// BEFORE this acquire, so the restore loop's in-lock re-check sees the
	// kill intent and aborts instead of racing to go first.
	//
	// BOUNDED, not blocking (#1917). The exclusion above is kept exactly — this
	// is still a real mutex, and a kill still never interleaves with a respawn —
	// but the wait now has a deadline. An unbounded Lock() here sits inside the
	// killsInFlight guard, so a peer operation that wedges (a stuck Recover, a
	// hung tmux/git subprocess under it) does not just delay this kill: it makes
	// the session permanently undeletable, since every retry is then rejected by
	// the guard this call is holding. Failing cleanly is strictly better than
	// waiting forever, and it is safe to fail HERE specifically because we have
	// not committed to anything yet — the tombstone below is the commit point, so
	// a timeout at this line leaves the session bit-for-bit unchanged and the
	// retry the error asks for is a true retry.
	opLock := m.opLockFor(key)
	if !lockWithin(opLock, opLockTimeout) {
		log.WarningLog.Printf("kill of session %q could not acquire its operation lock within %s; another operation on this session is not releasing it", req.Title, opLockTimeout)
		return session.InstanceData{}, errKillBusy(req.Title, opLockTimeout)
	}
	defer opLock.Unlock()

	// From here the kill has exclusive admission. The tombstone below is its
	// commit point; until that write succeeds, every guard leaves the session
	// unchanged. Every later step is individually
	// bounded (the instances flock, and the tmux/git subprocesses under
	// instance.Kill), so this function terminates without needing to abandon work
	// mid-flight. The watchdog only reports a stage — and stacks — if a step
	// somehow outlives its own bound, so the next occurrence names its wedge
	// point instead of leaving it to be inferred from logs (#1917).
	stage := &killStage{}
	defer watchKill(req.Title, killWatchdogTabCount(instance, data), stage)()

	stage.set("checking instance identity")
	if m.currentInstanceReplaced(key, instance, targetID) {
		log.InfoLog.Printf("kill of session %q skipped: current instance identity changed before teardown", req.Title)
		return resolved, nil
	}
	// Admission belongs before the commit point and before AgentServer.Kill closes
	// brokers or the local backend tears panes down. Cleanup's later backstop can
	// retain a record, but it cannot undo teardown that already happened.
	if instance != nil {
		// A direct kill of an archived session may be the first operation to
		// learn its origin repo is gone (#3176). Nothing failed before this
		// kill, so no relocation record exists and the admission below would
		// read that absence as permission. Establish and persist the
		// identity-qualified cleanup authorization first — the same record a
		// failed restore leaves — or refuse before the tombstone, leaving at
		// most a conservative, non-destructive fence.
		stage.set("checking archived origin state")
		if prepErr := m.prepareDirectRepoGoneKillCleanup(repoID, req.Title, instance); prepErr != nil {
			return session.InstanceData{}, fmt.Errorf("kill of session %q was not started: %w", req.Title, prepErr)
		}
		stage.set("validating destruction admission")
		if admissionErr := instance.ValidateWorktreeDestructionAdmission(); admissionErr != nil {
			return session.InstanceData{}, fmt.Errorf(
				"kill of session %q was not started because its worktree relocation is unresolved; nothing was changed: %w",
				req.Title, admissionErr,
			)
		}
	} else if data != nil {
		// The ghost twin of the producer above (#3278 review): an archived,
		// record-free ghost cannot have cleanup authorization built for it, so
		// an origin that cannot be proven present refuses the kill instead of
		// letting ordinary ghost cleanup settle against it and orphan the
		// archive.
		if ghostErr := ghostDirectRepoGoneKillGuard(data); ghostErr != nil {
			return session.InstanceData{}, fmt.Errorf(
				"kill of ghost session %q was not started: %w", req.Title, ghostErr,
			)
		}
		if admissionErr := validateGhostWorktreeDestructionAdmission(data); admissionErr != nil {
			return session.InstanceData{}, fmt.Errorf(
				"kill of ghost session %q was not started because its persisted worktree recovery is not safe to consume; nothing was changed — retry archive or restore before destructive cleanup: %w",
				req.Title, admissionErr,
			)
		}
	}

	// Persist the kill-intent tombstone BEFORE teardown begins (#1108): if the
	// daemon dies or the teardown errors between here and DeleteInstanceByStableID, the
	// surviving record is provably a user kill — the status poll finishes the
	// teardown instead of classifying the vanished session Lost and restoring
	// it. Best-effort: a failed tombstone write degrades to today's crash
	// window, which must not block the kill itself.
	//
	// ABORTS the kill when it cannot be recorded (#1917). This is the commit point:
	// everything above is reversible and nothing has been destroyed yet, so a
	// failure here costs the user a retry. Proceeding instead would tear the
	// session down with no durable record that the user asked for it — and a daemon
	// that then died before the record delete would reload a non-tombstoned row,
	// call it Lost, and RESTORE the session into the worktree teardown had already
	// deleted. The bound that makes this write fail (rather than hang) is only safe
	// because the failure stops here.
	stage.set("persisting kill tombstone")
	if err := m.persistKillTombstone(repoID, instance, data); err != nil {
		return session.InstanceData{}, fmt.Errorf("kill of session %q was not started: its kill intent could not be recorded, and tearing the session down without that record risks it being restored later; the session was not torn down — retry the kill: %w", req.Title, err)
	}
	// Past the commit point. Every error below leaves the kill durably recorded
	// (and usually the tombstoned row retained as its retry handle), so each one
	// is wrapped as a mutationCommittedError carrying the resolved stable
	// identity — a plain error here tells control/HTTP clients
	// failed-nothing-committed about a session that IS marked killed (#3234).

	// Revoke this session's sandbox callback credential (#2999), anchored to the
	// commit point immediately above rather than to the top of the function
	// (#3012 review). Still before teardown, so the credential dies with the
	// session — but only once the kill is real.
	//
	// It used to sit above the op-lock wait, on the reasoning that intent alone
	// should stop a credential. That reasoning is wrong here, because revocation
	// is NOT reversible on this path: the registry mints only at provision time,
	// so a kill that aborts early — the op-lock timeout, a failed tombstone, or
	// the identity-changed skip, all of which return leaving the session
	// bit-for-bit unchanged and ask the caller to retry — left a still-running
	// sandbox permanently unable to call back. An operation that promises "nothing
	// was changed" must not have changed anything.
	//
	// This is the same rule the archive path already follows; the two placements
	// disagreed and the archive one was right.
	m.sandboxTokens.revoke(resolvedID)

	// Stop this session's VS Code editor before the worktree goes away: it is
	// daemon-owned infrastructure rather than a tab, so no tab teardown covers it,
	// and a killed session's editor would otherwise linger rooted at a directory
	// that is being removed. No-ops when the session never had a vscode tab.
	//
	// Checked TWICE, deliberately. The webtab proxy resolves (and may spawn) an
	// editor without this op-lock — it must, since a spawn blocks for seconds — so
	// a request racing this teardown could start one after the stop below.
	// ensureVSCodeServer refuses once the session is inert, which closes most of
	// that window; the second confirmed stop runs before the durable record is
	// deleted, so "a killed session has no editor" holds on ordering rather than
	// timing, and uncertainty retains the stable-id retry handle.
	vscodeKey := daemonInstanceKey(repoID, req.Title)
	stage.set("stopping vscode editor")
	if err := m.stopVSCodeForInstance(vscodeKey, targetID); err != nil {
		return resolved, &mutationCommittedError{err: fmt.Errorf("kill of session %q could not safely stop its VS Code editor, so its tombstoned record was kept for a retry: %w", req.Title, err)}
	}

	// Carried to the record delete below, which refuses on a non-nil teardown.
	var (
		teardownErr                   error
		settledDescriptorGhostCleanup bool
	)

	if instance != nil {
		stage.set("tearing down tmux + worktree")
		// Retain + retry ONLY when the teardown's outcome is UNKNOWN, gated on the
		// SAME session.TeardownStateUnknown classifier deleteSessionRecord uses below
		// (#2017). Returning here deliberately SKIPS the record delete: an unknown
		// state — tmux never confirmed a pane dead (ErrPaneMayBeLive), or git was cut
		// off mid-removal (ErrWorkspaceStateUnknown) — means the workspace may still be
		// on disk with this record as its only handle, so dropping the record would
		// orphan it and take away the only handle the user has to retry through. The
		// tombstone is already durable, so the kill stays committed: finishUserKill
		// retries it on every poll until it succeeds, with no daemon restart needed.
		//
		// A KNOWN-state error must NOT retain, or the message it prints is a lie. A
		// remote (docker/ssh/hook) session's Kill joins the in-sandbox /v1/agent/kill
		// REST result with the sandbox reap, so a session whose in-sandbox agent-server
		// already died — the common reason to kill it — returns the failed REST call
		// over a SUCCEEDED reap: a plain endpoint error whose subject is a dead
		// endpoint, not the workspace. The workspace is provably gone, so we fall
		// through to deleteSessionRecord, which logs that cause and deletes the row —
		// instead of the old any-non-nil return reporting "its workspace was left
		// intact … retried automatically" on a fully successful reap and flickering the
		// row for one poll. Mirrors the create-cleanup gate in manager_create.go.
		restoreCheckpoint := instance.SetRepoGoneFinalizationCheckpoint(func() error {
			return m.persistInstanceErr(repoID, instance)
		})
		teardownErr = instance.Kill()
		restoreCheckpoint()
		if session.TeardownStateUnknown(teardownErr) {
			log.WarningLog.Printf("kill of session %q could not complete its teardown; the record is kept and the daemon will retry it: %v", req.Title, teardownErr)
			return resolved, &mutationCommittedError{err: fmt.Errorf("kill of session %q could not finish tearing it down safely, so its workspace was left intact; the kill is recorded and will be retried automatically: %w", req.Title, teardownErr)}
		}
		// Checkpoint the settled live teardown BEFORE either fallible tail step.
		// CleanupClaimedRepoGone consumes its in-memory recovery record after the
		// identity-anchored delete; if the editor fence or row delete then fails,
		// leaving cleanup_ready on disk makes a restart reinterpret the now-absent
		// archive as a changed identity and strand the tombstone forever. Persisting
		// the instance's post-teardown projection (including the consumed worktree
		// handle) makes the surviving row an accurate retry handle. This applies to
		// every known live teardown so a failed checkpoint is retried by the poll even
		// after the in-memory recovery handle is gone.
		stage.set("persisting settled teardown")
		if err := m.persistInstanceErr(repoID, instance); err != nil {
			return resolved, &mutationCommittedError{err: fmt.Errorf("kill of session %q completed its live teardown but could not record that settlement; its tombstoned row was kept and will be retried automatically: %w", req.Title, err)}
		}
	} else if data != nil {
		stage.set("cleaning up ghost record")
		// Same gate as the live-instance branch above: retain + retry ONLY on an
		// UNKNOWN-state teardown, routed through the same session.TeardownStateUnknown
		// classifier (#1917/#2017). ghostCleanup only ever returns an ErrPaneMayBeLive/
		// ErrWorkspaceStateUnknown wrapper or nil today, so this is behavior-preserving —
		// but sharing the predicate keeps this branch from re-introducing the #2017
		// defect (a known-state error misreported as "workspace left intact"): any
		// future known-state error would fall through to deleteSessionRecord instead.
		decoded := data.RestoreArchiveRollbackFence()
		decoded, decodeErr := decoded.RestoreRelocationRecoveryOriginals()
		if decodeErr != nil {
			return resolved, &mutationCommittedError{err: fmt.Errorf("kill of ghost session %q could not decode its cleanup ownership after admission: %w", req.Title, decodeErr)}
		}
		recovery := decoded.Worktree.RelocationRecovery
		descriptorCleanup := recovery != nil && recovery.IdentityKnown &&
			(recovery.State == sessiongit.RelocationRecoveryCleanupReady ||
				recovery.State == sessiongit.RelocationRecoveryCleanupStalled ||
				recovery.State == sessiongit.RelocationRecoveryCleanupFinalizing)
		var lateCleanup <-chan error
		teardownErr, lateCleanup = ghostCleanup(data, req.Title, func(data *session.InstanceData) error {
			return m.persistGhostCleanupStall(repoID, data)
		})
		settledDescriptorGhostCleanup = descriptorCleanup && teardownErr == nil && lateCleanup == nil
		if session.TeardownStateUnknown(teardownErr) {
			recovery = data.Worktree.RelocationRecovery
			descriptorStalled := lateCleanup != nil ||
				recovery != nil && recovery.State == sessiongit.RelocationRecoveryCleanupStalled
			if descriptorStalled {
				m.markGhostCleanupStalled(key, targetID)
				if recovery != nil && recovery.State == sessiongit.RelocationRecoveryCleanupStalled {
					if persistErr := m.persistGhostCleanupStall(repoID, data); persistErr != nil {
						teardownErr = errors.Join(teardownErr, fmt.Errorf("persist ghost cleanup stall: %w", persistErr))
					}
				}
				if lateCleanup != nil {
					m.reconcileLateGhostCleanup(repoID, req.Title, key, targetID, lateCleanup)
				}
			}
			// A ghost has no live instance for the poll to visit. A descriptor worker
			// which merely outlived its deadline is not retried: its one definitive
			// result is reconciled above, while the process fence blocks duplicates.
			if descriptorStalled {
				log.WarningLog.Printf("kill of session %q could not complete its ghost teardown; the record is kept, and no second descriptor cleanup may start in this process — restart the daemon before retrying if the first worker does not settle: %v", req.Title, teardownErr)
				return resolved, &mutationCommittedError{err: fmt.Errorf("kill of session %q could not finish tearing it down safely, so its workspace was left intact and its record kept; this one is not retried automatically — restart the daemon before retrying once the cause clears: %w", req.Title, teardownErr)}
			}
			log.WarningLog.Printf("kill of session %q could not complete its ghost teardown; the record is kept, but nothing will retry it automatically (a ghost has no live instance for the poll to visit) — retry the kill once the cause clears: %v", req.Title, teardownErr)
			return resolved, &mutationCommittedError{err: fmt.Errorf("kill of session %q could not finish tearing it down safely, so its workspace was left intact and its record kept; this one is not retried automatically — run the kill again once the cause clears: %w", req.Title, teardownErr)}
		}
	}

	// Re-check before deleting the durable retry handle. A proxy request that
	// passed its pre-spawn fence before the tombstone was committed can finish
	// later; if its exact editor cannot be confirmed stopped, the record must stay
	// so the poll finisher can retry by stable id.
	stage.set("confirming vscode editor teardown")
	if err := m.stopVSCodeForInstance(vscodeKey, targetID); err != nil {
		if settledDescriptorGhostCleanup {
			m.reconcileSettledGhostCleanup(repoID, req.Title, key, targetID)
		}
		return resolved, &mutationCommittedError{err: fmt.Errorf("kill of session %q could not confirm its VS Code editor stopped, so its tombstoned record was kept for a retry: %w", req.Title, err)}
	}

	stage.set("deleting record from storage")
	// Through the one choke point (#1917): it refuses while the teardown's outcome
	// is unknown, so this call site cannot be the one that forgets.
	deleted, err := m.deleteSessionRecord(repoID, req.Title, targetID, teardownErr)
	if err != nil {
		if settledDescriptorGhostCleanup {
			m.reconcileSettledGhostCleanup(repoID, req.Title, key, targetID)
		}
		// A contended instances flock is retryable and must SAY so (#1917): the
		// tombstone is already durable, so the kill is committed and will be
		// finished either by the user's retry or — with no further input — by
		// finishUserKill on the next poll, which reaches this same delete once
		// this call returns and releases killsInFlight. What must not happen is
		// this call never returning: that is what starved the finisher and made
		// the session undeletable until a daemon restart.
		if errors.Is(err, config.ErrLockTimeout) {
			log.WarningLog.Printf("kill of session %q: the instances record is locked by another agent-factory process; the kill is committed and the daemon will finish it on a later poll: %v", req.Title, err)
			return resolved, &mutationCommittedError{err: fmt.Errorf("kill of session %q could not update its record because another agent-factory process is holding the repo's instances lock; the session is already marked killed and will be reaped automatically — retry if it lingers: %w", req.Title, err)}
		}
		return resolved, &mutationCommittedError{err: fmt.Errorf("failed to delete instance from storage: %w", err)}
	}
	if !deleted {
		log.InfoLog.Printf("kill of session %q skipped storage delete: current record has a different instance identity", req.Title)
		return resolved, nil
	}

	m.mu.Lock()
	if current := m.instances[key]; current == nil || current == instance || stableIDMatchesForDaemon(current.ID, targetID) {
		delete(m.instances, key)
		// Same critical section as the record's removal, so the runtime state
		// cannot outlive the session it describes (#3031).
		m.forgetSessionRuntimeStateLocked(repoID, instance)
	}
	if session.IsReservedTitle(req.Title) {
		// An explicit kill is honored only briefly: the ensure loop suppresses
		// re-creation for rootKillHealDelay, then self-heals a still-configured
		// root (#1223). Config (root_agents) is the source of truth — removing
		// the repo from it is the only permanent stop. Recorded even for
		// unconfigured repos (harmless — the loop never visits them — and it
		// keeps kill-vs-config-change ordering race-free).
		m.rootKilledAt[repoID] = nowFunc()
		log.InfoLog.Printf("root agent for repo %s killed by user; the ensure loop will re-create it in ~%s unless the repo is removed from root_agents", repoID, rootKillHealDelay)
	}
	m.mu.Unlock()
	return resolved, nil
}

func (m *Manager) SendPrompt(req SendPromptRequest) error {
	_, err := m.SendPromptWithStatus(req)
	return err
}

// SendPromptWithStatus preserves SendPrompt's error contract and additionally
// carries the runtime's closed delivery observation back to command callers.
// It performs no extra wait: local tmux and remote agent-server paths return
// the observation their existing submit already made.
func (m *Manager) SendPromptWithStatus(req SendPromptRequest) (session.PromptDeliveryStatus, error) {
	if req.Prompt == "" {
		return session.PromptCouldNotConfirm, fmt.Errorf("prompt is required")
	}
	// Every return below, up to the AgentServer().SendPrompt at the bottom, fails
	// BEFORE any byte reaches a pane — a resolution/restore failure, or the target
	// racing into teardown. So each is a #2501 pre-flight failure and is tagged
	// notAttempted, so the watch path refunds the rate slot the attempt reserved
	// instead of leaking the per-minute budget over an outage. Only the send
	// itself may have landed, so only it stays charged (see errNotAttempted).
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		// Carry the marker so the tag survives the RPC hop (#2501/#2512); the phrase
		// is natural user text, not an appended token.
		return session.PromptCouldNotConfirm, notAttempted(fmt.Errorf("%w; %s", err, notDeliveredMarker))
	}
	// Canonicalize to the resolved session's title so the killsInFlight gate and
	// delivery target key off the id-resolved identity, not the request's title.
	req.Title = title
	if instance == nil {
		return session.PromptCouldNotConfirm, notAttempted(fmt.Errorf("failed to restore instance %q; %s", req.Title, notDeliveredMarker))
	}

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return session.PromptCouldNotConfirm, notAttempted(fmt.Errorf("target session %q is being deleted; prompt not delivered", req.Title))
	}

	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	_, killing = m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return session.PromptCouldNotConfirm, notAttempted(fmt.Errorf("target session %q is being deleted; prompt not delivered", req.Title))
	}
	if current == nil {
		return session.PromptCouldNotConfirm, notAttempted(fmt.Errorf("target session %q was deleted; prompt not delivered", req.Title))
	}
	if current != instance {
		if instance.ID != "" && current.ID != "" && current.ID != instance.ID {
			return session.PromptCouldNotConfirm, notAttempted(fmt.Errorf("target session %q changed while preparing prompt; prompt not delivered", req.Title))
		}
		instance = current
	}
	if instance.IsTearingDown() {
		return session.PromptCouldNotConfirm, notAttempted(fmt.Errorf("target session %q is being deleted; prompt not delivered", req.Title))
	}
	if err := promptTargetLivenessError(req.Title, instance.GetLiveness()); err != nil {
		return session.PromptCouldNotConfirm, notAttempted(err)
	}
	// Deliver through the agent-server (#1592 Phase 2 PR4), not the tmux-shaped
	// Backend method — the daemon's delivery path is runtime-agnostic. SendPrompt
	// is the reliable command path automated deliveries need. This crosses the
	// socket, so its failure is ambiguous ("never sent" vs "sent, reply lost") and
	// is deliberately NOT tagged notAttempted — an ambiguous failure stays charged.
	status, err := instance.SendPromptWithEvidence(req.Prompt, nowFunc)
	// Delivery evidence changes the row even when liveness does not. Publish it
	// immediately so list clients can distinguish a confirmed miss from #3162's
	// honest could-not-confirm instead of waiting for the status poll.
	if perr := m.persistSettlement(repoID, key, instance); perr != nil {
		log.WarningLog.Printf("prompt delivery evidence for %q: %v", instance.Title, perr)
	}
	if err != nil {
		return status, fmt.Errorf("failed to send prompt: %w", err)
	}
	return status, nil
}

func promptTargetLivenessError(title string, liveness session.Liveness) error {
	switch liveness {
	case session.LiveLost:
		return fmt.Errorf("target session %q is Lost; prompt not delivered; recover it first", title)
	case session.LiveDead:
		return fmt.Errorf("target session %q is Dead; prompt not delivered; recover it first", title)
	case session.LiveArchived:
		// Archived sessions have no live tmux to deliver into (#1529): without
		// this case the prompt falls through to a confusing backend error. Point
		// at the off-ramp, mirroring the TUI's interactiveGuard message. The
		// restore command embeds the title, so shell-quote it — a title with
		// spaces or shell metacharacters must not turn a copy-pasted
		// `af sessions restore ...` into the wrong target or a second command.
		return fmt.Errorf("target session %q is Archived; prompt not delivered; restore it first (%s)", title, shellsuggest.Command("af", "sessions", "restore", title))
	}
	return nil
}

// agentServerForStream resolves the /v1/sessions/{id}/stream target to its cached
// agent-server for the WS PTY broker (#1592 Phase 2 PR5). The route multiplexes
// two authoritative request shapes: TUI/CLI send {title}+repo_id, while Web sends
// a globally unique stable id with no repo_id. Presence of repo_id selects the
// title namespace; only the unscoped shape tries stable identity first. Both
// return the tracked instance's cached agent-server singleton whose ring
// buffer/subscribers persist.
func (m *Manager) agentServerForStream(idOrTitle, repoID string) (session.AgentServer, *session.Instance, error) {
	instance, resolvedRepoID, title, err := m.resolveStreamSession(idOrTitle, repoID)
	if err != nil {
		return nil, nil, err
	}
	if instance == nil {
		return nil, nil, fmt.Errorf("session %q not found", idOrTitle)
	}
	// Reject a new subscription while a kill is in flight for this session, the
	// same killsInFlight gate SendPrompt checks (#1632). Streaming previously
	// skipped it, so a Subscribe could race KillSession's teardown; the
	// agent-server's closed latch is the structural backstop (it refuses to
	// resurrect a broker), and this makes the daemon reject the dial up front
	// rather than opening a stream that immediately EOFs.
	key := daemonInstanceKey(resolvedRepoID, title)
	m.mu.Lock()
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return nil, nil, fmt.Errorf("session %q is being deleted", title)
	}
	return instance.AgentServer(), instance, nil
}

// resolveStreamSession returns the instance, its resolved repoID, and its title —
// the last so the killsInFlight gate keys off the real title even when Web used a
// stable id. Both request shapes consult the daemon's already-restored map
// directly: Preview is a repaint hot path, so re-scanning every persisted session
// before each capture makes its latency grow with the whole session history. A
// miss refreshes exactly once, using the SAME streamTarget authority in both
// phases: repo-scoped title lookup never detours through the global ID namespace,
// while an unscoped Web ID gets first refusal after refresh before legacy title
// fallback (#2187/#2279 review).
func (m *Manager) resolveStreamSession(idOrTitle, repoID string) (*session.Instance, string, string, error) {
	target := authoritativeStreamTarget(idOrTitle, repoID)
	m.mu.Lock()
	if instance, rid, title := m.trackedStreamSessionLocked(target); instance != nil {
		m.mu.Unlock()
		return instance, rid, title, nil
	}
	m.mu.Unlock()
	instance, resolvedRepoID, _, err := m.findSessionByStableID(target.stableID, target.title, target.repoID)
	if instance == nil {
		return nil, resolvedRepoID, target.title, err
	}
	return instance, resolvedRepoID, instance.Title, err
}

// streamTarget makes the route's two authority shapes mutually exclusive. A
// scoped target can carry only {repo,title}; an unscoped target may carry the
// stable-id interpretation of the same opaque segment. No resolver phase can
// accidentally give a foreign global ID precedence over an explicit repo scope.
type streamTarget struct {
	stableID string
	title    string
	repoID   string
}

func authoritativeStreamTarget(idOrTitle, repoID string) streamTarget {
	target := streamTarget{title: idOrTitle, repoID: repoID}
	if repoID == "" {
		target.stableID = idOrTitle
	}
	return target
}

// trackedStreamSessionLocked resolves only facts already materialized in
// m.instances. The caller holds m.mu. The streamTarget, rather than this helper,
// decides the namespace so the hot path and post-refresh path cannot drift.
// trackedStreamSession resolves idOrTitle against the ALREADY-RESTORED instance map
// and nothing else — no disk read, no refresh, no fallback. It is for callers that
// must not let a lookup miss turn into I/O, because the lookup is reachable by an
// untrusted peer: /v1/preview-auth's mint guard is a GET that a cross-origin page can
// drive under the tokenless-loopback default, so a refreshing miss path there is an
// amplifier rather than a convenience (#2833 review).
//
// Callers that legitimately need a session the daemon has not tracked yet keep using
// resolveStreamSession; this is deliberately the weaker, cheaper question.
func (m *Manager) trackedStreamSession(idOrTitle string) *session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	instance, _, _ := m.trackedStreamSessionLocked(authoritativeStreamTarget(idOrTitle, ""))
	return instance
}

func (m *Manager) trackedStreamSessionLocked(target streamTarget) (*session.Instance, string, string) {
	if target.repoID != "" {
		if instance := m.instances[daemonInstanceKey(target.repoID, target.title)]; instance != nil {
			return instance, target.repoID, instance.Title
		}
		return nil, "", ""
	}
	if instance, rid := m.trackedSessionByIDLocked(target.stableID); instance != nil {
		return instance, rid, instance.Title
	}
	return nil, "", ""
}

// trackedSessionByIDLocked resolves one stable identity in the materialized
// instance map. The caller holds m.mu. Stream hot-path lookup and post-refresh
// lookup share it so the stable namespace cannot drift between the two phases.
func (m *Manager) trackedSessionByIDLocked(id string) (*session.Instance, string) {
	if id == "" {
		return nil, ""
	}
	for key, instance := range m.instances {
		if instance != nil && instance.ID == id {
			rid, _ := splitDaemonInstanceKey(key)
			return instance, rid
		}
	}
	return nil, ""
}

// resolveActionSession resolves a session mutation target by the caller-supplied
// stable id FIRST — the retained clients' collision-proof key — and falls back
// to {title, repoID} only when NO id is given (legacy/one-shot CLI callers).
// Resolving by id is what stops a duplicate title across repos from targeting the
// WRONG session on a destructive action: findSession with an empty repoID returns
// the first title match in nondeterministic map order (#1592 Phase 5 follow-up).
//
// A supplied id is AUTHORITATIVE — it uniquely names one session. If it is not
// tracked in memory (the session was killed/archived out from under a stale client
// rail), this returns a clear "not found" error rather than falling back to a title
// match: an empty-repoID title lookup could resolve a DIFFERENT same-title session
// in another repo and operate on it — the exact destructive cross-repo collision
// this fix closes, just re-entered through a stale id. Erroring keeps a stale id
// from ever silently retargeting; the id-less title path stays for legacy/CLI.
//
// Its ID-addressed arm mirrors the unscoped Web stream shape; repo-scoped stream
// callers deliberately take the title arm instead. It returns the resolved
// instance, its repoID, its canonical title, its stable id, and (for the title
// path) its on-disk data — so the caller keys teardown, storage, AND the lifecycle
// event off the session actually resolved, never the request's own (possibly
// stale) id/title.
func (m *Manager) resolveActionSession(id, title, repoID string) (*session.Instance, string, string, string, *session.InstanceData, error) {
	if id != "" {
		m.mu.Lock()
		if err := m.refreshLocked(); err != nil {
			m.mu.Unlock()
			return nil, "", "", "", nil, err
		}
		for key, instance := range m.instances {
			if instance.ID != "" && instance.ID == id {
				rid, _ := splitDaemonInstanceKey(key)
				resolvedTitle := instance.Title
				m.mu.Unlock()
				return instance, rid, resolvedTitle, instance.ID, nil, nil
			}
		}
		m.mu.Unlock()
		return nil, "", "", "", nil, fmt.Errorf("session with id %q %w", id, errSessionNotFound)
	}
	// Legacy/CLI path: no id supplied, resolve by {title, repoID}.
	instance, resolvedRepoID, data, err := m.findSession(title, repoID)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	resolvedID := ""
	if instance != nil {
		resolvedID = instance.ID
	} else if data != nil {
		resolvedID = data.ID
	}
	return instance, resolvedRepoID, title, resolvedID, data, nil
}

func (m *Manager) findSession(title, repoID string) (*session.Instance, string, *session.InstanceData, error) {
	return m.findSessionByStableID("", title, repoID)
}

// findSessionByStableID performs findSession's refresh exactly once, then gives
// stableID first refusal on the refreshed map before any title lookup. Passing an
// empty stableID preserves findSession's title-only behavior. The stream route
// supplies stableID only for its unscoped shape; this both prevents a newly
// restored Web ID from being reinterpreted as somebody else's title (#2187) and
// prevents a scoped TUI title from being reinterpreted as a foreign ID (#2279
// review).
func (m *Manager) findSessionByStableID(stableID, title, repoID string) (*session.Instance, string, *session.InstanceData, error) {
	if title == "" {
		return nil, "", nil, fmt.Errorf("session title is required")
	}

	m.mu.Lock()
	if err := m.refreshLocked(); err != nil {
		m.mu.Unlock()
		return nil, "", nil, err
	}
	if instance, rid := m.trackedSessionByIDLocked(stableID); instance != nil {
		m.mu.Unlock()
		return instance, rid, nil, nil
	}
	if repoID != "" {
		key := daemonInstanceKey(repoID, title)
		if instance := m.instances[key]; instance != nil {
			m.mu.Unlock()
			return instance, repoID, nil, nil
		}
	} else {
		// Unscoped: titles are unique per-repo, so collect every match rather
		// than returning the first one the map walk happens to reach. One match
		// resolves; several are ambiguous and must not be silently picked
		// between — a kill/archive would otherwise hit an arbitrary repo's
		// session (the collision resolveActionSession's id-first path avoids).
		var matched *session.Instance
		var matchedRepoID string
		var matchRepoIDs, repoPaths []string
		for key, instance := range m.instances {
			if instance == nil || instance.Title != title {
				continue
			}
			rid, _ := splitDaemonInstanceKey(key)
			if matched == nil {
				matched, matchedRepoID = instance, rid
			}
			matchRepoIDs = append(matchRepoIDs, rid)
			repoPaths = append(repoPaths, instance.Path)
		}
		if len(session.DedupeSorted(matchRepoIDs)) > 1 {
			m.mu.Unlock()
			return nil, "", nil, session.AmbiguousTitleError(title, repoPaths)
		}
		if matched != nil {
			m.mu.Unlock()
			// One live match is NOT proof the title is unique. A second repo's row
			// is skipped during refresh when it cannot be restored (worktree/tmux
			// gone), so it never reaches m.instances — and resolving here would let
			// an unscoped kill/archive hit this repo while the daemon-down disk path
			// would refuse to guess. Union the persisted rows before resolving.
			if paths, err := collectTitleRepoPathsOnDisk(title); err != nil {
				// Could not enumerate repos at all: prefer the live match over
				// failing a working lookup, but say so — this is the one window
				// where the ambiguity guard cannot be applied.
				log.WarningLog.Printf("could not check %q for cross-repo ambiguity, resolving the live match in repo %s: %v", title, matchedRepoID, err)
			} else {
				repos := map[string]string{matchedRepoID: matched.Path}
				for rid, p := range paths {
					repos[rid] = p
				}
				if len(repos) > 1 {
					all := make([]string, 0, len(repos))
					for _, p := range repos {
						all = append(all, p)
					}
					return nil, "", nil, session.AmbiguousTitleError(title, all)
				}
			}
			return matched, matchedRepoID, nil, nil
		}
	}
	m.mu.Unlock()

	data, rid, err := findInstanceDataByTitle(title, repoID)
	if err != nil {
		return nil, "", nil, err
	}
	instance, restoreErr := fromInstanceDataForRefresh(*data)
	if restoreErr != nil {
		return nil, rid, data, nil
	}
	// Give it the daemon-backed credential minter (#3068). A disk-loaded instance
	// has none — FromInstanceData cannot know about the daemon — and without this
	// a restore would provision its replacement sandbox with no callback at all,
	// silently, which is the ordinary case after a daemon restart.
	attachSandboxCredentials(m, instance)

	// We built `instance` from disk with m.mu released, so a concurrent
	// refresh (or another RPC) may have restored and registered the canonical
	// Instance for this session during the window (#867). Returning our freshly
	// built duplicate would hand the caller an *untracked* Instance: SendPrompt
	// would leak its restore-time attach PTY, and KillSession would call
	// instance.Kill() — tearing down the tmux session and worktree that the
	// canonical, still-tracked Instance shares. Re-acquire the lock and:
	//   - if a tracked Instance now exists, drop our duplicate (closing only
	//     its attach resources, never the shared session) and operate on the
	//     tracked one; otherwise
	//   - register our Instance so callers operate on a tracked Instance, just
	//     as the refresh loop would have, instead of an orphan.
	key := daemonInstanceKey(rid, title)
	m.mu.Lock()
	if tracked := m.instances[key]; tracked != nil {
		m.mu.Unlock()
		if err := instance.CloseAttachOnly(); err != nil {
			log.WarningLog.Printf("findSession %q: closing duplicate instance attach failed: %v", title, err)
		}
		return tracked, rid, data, nil
	}
	m.instances[key] = instance
	m.mu.Unlock()
	return instance, rid, data, nil
}
