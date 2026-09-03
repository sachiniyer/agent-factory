package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The archived-RESTORE half of the archive lifecycle: bringing a shelved session
// back, for both the local worktree-move route and the off-box re-provision one.
// Split out of archive.go, which had grown past the file-length limit holding
// both verbs (#1145); nothing here changed in the move.

// restoreRemoteSession restores an archived off-box session (docker/ssh/hook) by
// re-provisioning a fresh sandbox that clones the pushed branch back and
// relaunching the agent (#1592 Phase 4 PR6) — the remote analogue of
// RestoreArchived's worktree-move body, sharing its guards + locks. It reuses
// RestoreFromArchive unchanged: BeginRestore fences the restore, then the
// backend's Recover (recoverSandbox) re-provisions + relaunches + flips the
// session live. The code survives via origin; a fresh agent runs on the pushed
// branch (the pre-archive conversation lived only in the disposed sandbox).
func (m *Manager) restoreRemoteSession(repoID string, instance *session.Instance, title string) (string, error) {
	if err := instance.RestoreFromArchive(); err != nil {
		// On a re-provision/relaunch failure RestoreFromArchive left the instance
		// Lost; persist that and surface the failure (an explicit retry re-provisions
		// from the still-pushed branch).
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("failed to restore remote session %q (re-provisioning its sandbox): %w", title, err)
	}
	// A FRESH sandbox now backs this session, so its accumulated remote-loss
	// failures describe a sandbox that is gone (#1794). Reset BEFORE the persist
	// and log below, not after: RestoreFromArchive has already flipped the session
	// live and dropped OpRestoring, and the poll goroutine neither takes the
	// op-lock nor checks killsInFlight — so it can probe the new sandbox while
	// this call is still writing to disk, and a blip in that window would be
	// judged against the OLD sandbox's threshold-satisfying count and mark the
	// fresh runtime Lost. The instance keeps the same ID across the re-provision
	// (same session, new runtime), so nothing else can notice the swap; only this
	// site knows it happened.
	m.noteRuntimeReplaced(repoID, instance)
	m.persistRuntimeReplacement(repoID, title, instance)
	log.InfoLog.Printf("restored remote session %q (repo %s): fresh sandbox provisioned, branch cloned back, agent relaunched", title, repoID)
	return title, nil
}

// RestoreArchived restores an archived session (#1028): it moves the worktree
// back to where session creation would place it under the configured
// worktree_root (a free sibling path, or under $AF_HOME/worktrees for
// subdirectory users — #1540), re-registers it, re-spawns the agent, and marks
// the instance Running. It returns both the worktree path and the canonical
// stable identity it actually restored, so lifecycle events cannot be rebuilt
// from stale request display fields. The agent session is brought back, as are any web tabs
// (pure metadata: they were never torn down, so their URLs ride back on the
// record and render again, #1809); shell/process tabs were dropped at archive
// time. Returns the restored worktree path.
//
// Concurrency uses the same two gates as ArchiveSession/KillSession — the
// killsInFlight claim and the per-session op-lock — but in the opposite ORDER:
// the lock is taken first, so the claim is never held across the wait for it
// (#3600). On a repo-gone failure the archive is left intact with an actionable
// error; on a re-spawn failure the worktree is already back in place and the
// instance is left Lost so the #1108 restore loop heals it.
func (m *Manager) RestoreArchived(req RestoreArchivedRequest) (string, session.InstanceData, error) {
	instance, repoID, title, resolvedID, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	resolved := session.InstanceData{ID: resolvedID, Title: title}
	if instance == nil {
		return "", session.InstanceData{}, fmt.Errorf("cannot restore session %q: no such session", title)
	}
	path, restoreErr := m.restoreArchivedInstance(instance, repoID, title)
	return path, resolved, restoreErr
}

// restoreArchivedInstance performs the restore for an identity already resolved
// by either RestoreArchived or the liveness-agnostic RestoreSession route. Keeping
// one body avoids a second title lookup between those sibling paths.
func (m *Manager) restoreArchivedInstance(instance *session.Instance, repoID, title string) (string, error) {
	req := RestoreArchivedRequest{ID: instance.ID, Title: title, RepoID: repoID}
	if err := instance.ValidateRuntimeAction(session.RuntimeActionRestoreArchived); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}

	key := daemonInstanceKey(repoID, req.Title)

	// Registered BEFORE both the lock and the claim so LIFO runs it LAST — after
	// releaseRestoreOperation and after opLock.Unlock (the #3597 ordering). The
	// claim is what actually refuses Kill, so lowering the fence while
	// killsInFlight is still set would publish a row saying can_kill=true that the
	// admission gate would still refuse. Released last, the busy projection covers
	// exactly the claim's interval.
	//
	// fenceRaised is ownership, not bookkeeping: the returns above the raise include
	// a claim that FAILED because another restore holds it, and that restore may well
	// have OpRestoring up. Releasing on the bare op value would lower a fence
	// belonging to someone else's operation. It also keeps this inert on the REMOTE
	// route, which raises nothing.
	fenceRaised := false
	defer func() {
		if !fenceRaised {
			return
		}
		if instance.EndArchivedRestoreFence() {
			m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
		}
	}()

	// Op-lock BEFORE the claim, the #3600 order — see restoreLostOrDeadSession for
	// the full argument, which applies verbatim here because both routes share
	// claimRestoreOperation and therefore shared its window. Claiming first held
	// killsInFlight across a wait bounded at opLockTimeout (30s) while the row sat
	// at {LiveArchived, OpNone}, and canKillFor never consults liveness — so an
	// archived row advertised a Kill that the admission gate would only refuse with
	// "an operation is already in progress". Taking the lock first makes the claim
	// and the fence raise adjacent, so no observer sees the row claimed-but-unfenced
	// and the wait itself holds nothing.
	//
	// Sampled in front of the wait for the reason claimRestoreOperation spells
	// out, and this is the route that needs it most: DeleteProject leaves archived
	// rows in m.instances untouched, so a delete that completes inside this wait is
	// invisible to both the claim's projectDeletes read and the instance re-read
	// below.
	deleteSeq := m.projectDeleteSeqNow()
	beforeRestoreOperationLock()
	opLock, waited, err := m.lockSessionOperationWithin(key, "restore", req.Title)
	if err != nil {
		return "", err
	}
	defer opLock.Unlock()

	if err := m.claimRestoreOperation(repoID, key, req.Title, waited, deleteSeq); err != nil {
		return "", err
	}
	defer m.releaseRestoreOperation(key)

	// Re-verify under the op-lock (findSession released m.mu).
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance {
		return "", fmt.Errorf("session %q changed state before restore could start", req.Title)
	}
	// Re-run the shared runtime-entry guard under the operation lock, before the
	// worktree move. A durable kill tombstone is terminal intent: restoring
	// around it would appear to succeed, then finishUserKill would reap the
	// replacement on the next poll (#2208).
	if err := instance.ValidateRuntimeAction(session.RuntimeActionRestoreArchived); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}

	// An off-box session (docker/ssh/hook) restores by re-provisioning a fresh
	// sandbox that clones the pushed branch back, not by moving a worktree it does
	// not have
	// (#1592 Phase 4 PR6). Route it to the remote body, which shares this method's
	// guards + locks.
	if instance.Capabilities().Workspace == session.WorkspaceRemote {
		// Deliberately unfenced (#3596): RestoreFromArchive is the FIRST statement
		// there, so the prefix this fence would cover is a capability read. The local
		// route below is the one with a worktree relocate in front of its re-spawn.
		return m.restoreRemoteSession(repoID, instance, req.Title)
	}

	// Raise the fence HERE, at the top of the LOCAL route, so it is coextensive with
	// the claim above (#3596 — the archived half of #3586).
	//
	// claimRestoreOperation already made Kill fail: from that line a Kill is rejected
	// at the admission gate with "an operation is already in progress". canKillFor
	// hides the affordance and reads the op axis alone, and canKillFor never consults
	// liveness, so an ARCHIVED row advertises Kill — for the relocation claim, the
	// repo-gone guard, the destination derivation and the worktree relocate, all of
	// which run before RestoreFromArchive raises anything. A cross-device move of a
	// large worktree is a copy, so that is not a window with a small bound.
	//
	// MarkRestoring, not BeginRestore: the op axis alone, liveness stays LiveArchived.
	// That is what keeps the #1203 reconcile rebuild keyed on the Archived->live
	// transition that still happens later, inside RestoreFromArchiveHeldFenced. The
	// row does re-home into the live Instances section for the whole relocate, which
	// is #1210's eager feedback starting when the restore starts.
	if err := instance.BeginArchivedRestoreFence(); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}
	fenceRaised = true
	// ANNOUNCE it, the contract #2997 gave the limit resume and #3597 the lost
	// restore. Clients gate their controls on the projected op and perform no
	// optimistic update for a CLI- or web-initiated restore, so a fence nobody is
	// told about leaves an already-open client offering Kill for the whole relocate.
	// Nothing below lowers the fence early: every snapshot published while the claim
	// is held reports the row busy, which is what it is.
	m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())

	// Resolve relocation ownership before reading repo-derived restore context.
	relocationClaim, err := m.claimRestoreRelocation(repoID, req.Title, instance)
	if err != nil {
		return "", err
	}
	claimTransferred := false
	defer func() {
		if !claimTransferred {
			instance.PreserveWorktreeRelocationClaimForRetry(relocationClaim)
		}
	}()

	repoPath := instance.GetRepoPath()
	if repoPath == "" {
		return "", fmt.Errorf("cannot restore session %q: no repo path on record", req.Title)
	}
	// Repo-gone check up front: SiblingWorktreePath and the worktree move both
	// need the origin repo, so surface the actionable message (archive left
	// intact) before either fails with a generic error.
	if repoGone, err := m.guardRepoGoneRestore(repoID, req.Title, repoPath, instance, relocationClaim); err != nil {
		claimTransferred = repoGone
		return "", err
	}
	// Honor the configured worktree_root placement, exactly as session creation
	// does (#1540): a subdirectory user's worktree is restored under
	// $AF_HOME/worktrees/<branch>, not stranded beside the repo. The branch is
	// needed only for subdirectory placement.
	beforeRestoreWorktreePath()
	dest, err := sessiongit.RestoreWorktreePath(repoPath, req.Title, instance.GetBranch())
	if err != nil {
		claimTransferred = true
		return "", m.persistRestorePathFailure(repoID, req.Title, instance, relocationClaim, err)
	}
	beforeRestoreWorktreeUse()

	// Move the worktree back next to the repo. A repo-gone failure leaves the
	// archive intact (the git layer guarantees this) and surfaces an actionable
	// message; the instance stays Archived.
	claimTransferred = true
	if err := instance.RestoreArchivedWorktreeHeldFencedWithClaim(dest, relocationClaim); err != nil {
		if errors.Is(err, sessiongit.ErrRepoGone) {
			return "", m.persistRepoGoneAtRestoreUse(repoID, req.Title, repoPath, instance, err)
		}
		if errors.Is(err, sessiongit.ErrRelocateStateUnknown) {
			// The bounded move may have reached either pathname before it was
			// killed. The git layer retains destination + source with the captured
			// directory identity; neither is destructive authority until a retry
			// resolves it. Persist both handles before returning.
			//
			// Persist first, then report — and take the error-returning persist,
			// not the log-and-continue one. The whole point of this branch is that
			// the location must survive a restart, so a write that did NOT happen
			// (full or read-only disk) reproduces the exact stranding it exists to
			// prevent. Reporting the relocate error alone would tell the operator
			// their worktree is safely recorded when nothing recorded it.
			if perr := m.persistInstanceErr(repoID, instance); perr != nil {
				return "", fmt.Errorf("restore of %q was cut off mid-relocate AND its recovery candidates could not be written to disk (%v); worktree recovery location: %s — inspect those path(s) before restarting the daemon: %w", req.Title, perr, worktreeRecoveryLocation(instance), err)
			}
			return "", fmt.Errorf("restore of %q was cut off mid-relocate; both possible worktree locations are recorded (%s), destructive cleanup is blocked, and a retry will resolve the directory identity: %w", req.Title, worktreeRecoveryLocation(instance), err)
		}
		return "", m.persistUnresolvedRestoreFailure(repoID, req.Title, instance, relocationClaim, err)
	}

	// The relocate SUCCEEDED, so the worktree's new location is now certain — and
	// it exists only in memory, exactly as on the cut-off branch above.
	// setWorktreeLocation updated g.worktreePath; this write is the only thing that
	// carries it across a restart, so it takes the error-returning persist for the
	// reason that branch already spells out (#2880). A restore nothing recorded is
	// not a completed restore, however live the session is: disk would still say
	// Archived at an archive path that no longer exists, the next restore would
	// fail relocate's source-exists guard, and the user's work would sit at a path
	// nothing durable points at. No poll repairs it — persistPollChange writes on a
	// liveness/reset change and this row's liveness is already settled — and the
	// whole-state shutdown checkpoint is what an unclean exit skips.
	//
	// Both arms below persist through it. The re-spawn failure is the same
	// stranding with a Lost row instead of a Running one: the worktree has moved
	// either way.
	restoredPath := instance.GetWorktreePath()
	commitRestore := func() error {
		if perr := m.persistInstanceErr(repoID, instance); perr != nil {
			return fmt.Errorf("the worktree for %q was moved back to %s but that location could not be written to disk (%v); "+
				"nothing durable points at it — move it back or re-register it manually before restarting the daemon",
				req.Title, restoredPath, perr)
		}
		return nil
	}

	// Record the new location NOW, before the respawn — not only after it.
	// RestoreFromArchive spawns tmux, which can take seconds or time out, and a
	// daemon that exits anywhere in that window would leave disk naming an archive
	// path that no longer exists even though every write here succeeded. Moving the
	// commit ahead of the respawn closes the window to the relocate itself, which
	// is the shortest it can be.
	//
	// It writes a still-Archived row pointing at the restored path, which is
	// exactly the state the cut-off-relocate branch above persists and is already
	// covered: a later restore relocates off the occupied path rather than
	// colliding with itself (RestoreWorktreePath's collision suffix).
	if perr := commitRestore(); perr != nil {
		return failedRestoredArchiveResult(instance, restoredPath, perr)
	}

	// Worktree is back in place. Re-spawn the agent and flip Running. On a
	// re-spawn failure RestoreFromArchive leaves the instance started + Lost, so
	// the Lost-restore loop keeps retrying against the now-restored worktree.
	// The held-fence form: this route raised OpRestoring before the relocate, and
	// RestoreFromArchive re-validates RuntimeActionRestoreArchived, which requires
	// OpNone and would now refuse its own operation's fence. Its BeginRestore edge
	// keeps that strict guard; BeginRestoreUnderHeldFence is the entry for the row
	// that arrives already fenced.
	if err := instance.RestoreFromArchiveHeldFenced(); err != nil {
		if perr := commitRestore(); perr != nil {
			return failedRestoredArchiveResult(instance, restoredPath, fmt.Errorf("%w; its agent also failed to re-spawn: %v", perr, err))
		}
		return failedRestoredArchiveResult(instance, restoredPath, fmt.Errorf("restored worktree for %q but failed to re-spawn its agent (it will be retried): %w", req.Title, err))
	}
	m.noteRuntimeReplaced(repoID, instance)
	worktreePath := instance.GetWorktreePath()
	if perr := commitRestore(); perr != nil {
		return failedRestoredArchiveResult(instance, worktreePath, fmt.Errorf("re-spawned the agent for %q, but %w", req.Title, perr))
	}
	log.InfoLog.Printf("restored session %q (repo %s): worktree moved back to %s, agent re-spawned", req.Title, repoID, worktreePath)
	return restoredArchiveResult(instance, worktreePath)
}
