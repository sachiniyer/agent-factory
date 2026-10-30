package daemon

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The archived-RESTORE half of the archive lifecycle: bringing a shelved session
// back, for both the local worktree-move route and the off-box re-provision one.
// Split out of archive.go, which had grown past the file-length limit holding
// both verbs (#1145); nothing here changed in the move.

// restoreRemoteSession restores an archived off-box session (docker/ssh/hook) —
// the remote analogue of RestoreArchived's worktree-move body, sharing its guards
// + locks. It reuses RestoreFromArchive unchanged: BeginRestore fences the
// restore, then the backend's Recover (recoverSandbox) brings the runtime back
// and flips the session live.
//
// An ssh/sandbox row archived without a push (#5571 W8.5) keeps its workspace on
// its host, and the restore REATTACHES it (W8.6): a new agent-server over the
// existing worktree, the agent relaunched with its recorded conversation, nothing
// cloned, pushed or re-hooked. Only a workspace the host proves gone, or a row
// that was pushed (and docker/hook, which always push), gets a fresh sandbox that
// clones the pushed branch back (#1592 Phase 4 PR6).
func (m *Manager) restoreRemoteSession(repoID string, instance *session.Instance, title string) (string, error) {
	what := "re-provisioning its sandbox"
	if instance.ArchivedWorkspace() != nil {
		what = "restoring its preserved workspace"
	}
	if err := instance.RestoreFromArchive(); err != nil {
		// On a re-provision/relaunch failure RestoreFromArchive left the instance
		// Lost; persist that and surface the failure (an explicit retry re-provisions
		// from the still-pushed branch).
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("failed to restore remote session %q (%s): %w", title, what, err)
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
	// The archive restore is the same explicit off-ramp the manual heal is: the
	// pre-archive confirmations described the shelved runtime's cycle, and the
	// poll sweep can sit between archive and restore forever — the flap
	// episode ends here or a later loss inherits it (#5238 review). A pending
	// awaitingConfirm arm retires with it: the fresh runtime's answers would
	// otherwise confirm the STALE arm and charge it one flap datapoint
	// (#5436).
	m.clearLostRestoreEpisode(stableSessionKey(repoID, instance))
	m.persistRuntimeReplacement(repoID, title, instance)
	m.info().Printf("restored remote session %q (repo %s): runtime back, agent relaunched", title, repoID)
	return restoredArchiveResult(instance, instance.GetWorktreePath())
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
	// A prune tombstone has no worktree to move back (#5189): refuse by naming the
	// branch the work was kept on, instead of failing later on a missing path.
	if at := instance.PrunedAt(); !at.IsZero() {
		return "", session.PrunedRestoreError(title, instance.KeptBranch(), at)
	}
	if err := m.pruneRowFence(repoID, instance); err != nil {
		return "", fmt.Errorf("cannot restore session %q: %w", title, err)
	}
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
	// Join create's repository admission BEFORE claiming or fencing this row.
	// A create can hold admission across an unbounded worktree add, so this wait
	// must remain bounded; timing out here leaves the archive entirely untouched
	// and, because no claim or fence exists yet, still honestly killable.
	worktreeAdmission, err := m.lockLocalWorktreeAdmissionWithin(repoID, req.Title, "restore", instance)
	if err != nil {
		return "", err
	}
	worktreeAdmissionHeld := worktreeAdmission != nil
	defer func() {
		if worktreeAdmissionHeld {
			worktreeAdmission.Unlock()
		}
	}()

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
	// Prune holds this same operation lock for its whole run, so a tombstone (or an
	// interrupted prune) is visible here even if it landed during the wait (#5189).
	if at := instance.PrunedAt(); !at.IsZero() {
		return "", session.PrunedRestoreError(title, instance.KeptBranch(), at)
	}
	if err := m.pruneRowFence(repoID, instance); err != nil {
		return "", fmt.Errorf("cannot restore session %q: %w", title, err)
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

	// Admission stays held from before destination selection through the move
	// that registers it. Without this hold, create can select the same absent
	// sibling path, restore can populate it, and create can act on its stale
	// selection. The git layer still verifies branch ownership at its destructive
	// remove boundary; this lock removes the AF-vs-AF interval that git's
	// path-only `worktree remove` cannot make atomic by itself.

	// Resolve relocation ownership before reading repo-derived restore context.
	// A conclusive ENOENT with no relocation record behind it (a recovery-path
	// claim error joins ErrRelocateStateUnknown and never wraps a bare ENOENT)
	// is a worktree deleted outside af — the archived-missing row the no-move
	// archive commits (#5102). There are no bytes to move back; the kept branch
	// is rebuilt into the restore destination instead.
	placement := "worktree moved back to"
	// placedByProof is true when the worktree was rebuilt or adopted rather than
	// moved back under a claim: its directory was proven and its identity pinned
	// (rebuildAbsentArchivedWorktree), and that pin is re-checked below at each
	// boundary that would make the directory authoritative.
	placedByProof := false
	relocationClaim, err := m.claimRestoreRelocation(repoID, req.Title, instance)
	switch {
	case err == nil:
		if err := m.moveArchivedWorktreeBack(repoID, req.Title, instance, relocationClaim); err != nil {
			return "", err
		}
	case errors.Is(err, os.ErrNotExist) && !errors.Is(err, sessiongit.ErrRelocateStateUnknown):
		if err := m.rebuildAbsentArchivedWorktree(repoID, req.Title, instance); err != nil {
			return "", err
		}
		placement = "worktree rebuilt from its kept branch at"
		placedByProof = true
	default:
		return "", err
	}
	// The directory af just placed is the only thing a missing row's restore may
	// record and start the agent in. Rebuild and adoption both proved it and
	// pinned its device/inode, but the hooks a rebuild starts, and anything else
	// running as this user, can swap the name between that proof and the commit
	// below — so the pin is reconfirmed before the commit and again before the
	// respawn (#5102). A failure leaves the row archived, still flagged, and
	// fenced by the git layer, and that state is persisted before the refusal is
	// reported.
	//
	// The worktree-missing flag does NOT clear here, though the worktree is back
	// where af put it. The pin lives only in memory, so the flag is the one
	// durable fence on a placed directory, and the commit below writes the
	// restored path to disk before the respawn-boundary reconfirmation runs.
	// Cleared first, that write claimed the path authoritative while its identity
	// was still unverified at the boundary that matters: a daemon that exited
	// before the second reconfirmation restarted on a row that trusted whatever
	// a same-UID process had swapped in, and a later restore would relocate its
	// bytes or start the agent there as the session's worktree. Kept set, the
	// commit persists the "restore was placing…" reason the rebuild and adoption
	// stamped for the restored path. A crash before a later commit carries the
	// clear therefore restarts on a flagged row: the next restore finds a
	// directory at the path and must re-prove it through moveArchivedWorktreeBack's
	// flagged-row guard, or finds none and rebuilds. The only record that drops the
	// flag names a directory verified at the respawn boundary. The clear is just
	// before the respawn, below.
	if placedByProof {
		if err := m.reconfirmPlacedRestoreWorktree(repoID, req.Title, instance, "before recording it"); err != nil {
			return "", err
		}
	}

	// The relocate SUCCEEDED, so the worktree's new location is now certain — and
	// it exists only in memory, exactly as on moveArchivedWorktreeBack's cut-off
	// branch.
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
		// Under worktree admission the start lock must not be taken: it sits to the
		// LEFT of admission in the documented order, and create holds it while it
		// waits on admission (#6197). Admission is released right after the respawn,
		// so the commits after it take the ordinary path.
		persist := m.persistInstanceErr
		if worktreeAdmissionHeld {
			persist = m.persistUnderAdmission
		}
		if perr := persist(repoID, instance); perr != nil {
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
	if placedByProof {
		if err := m.reconfirmPlacedRestoreWorktree(repoID, req.Title, instance, "before starting the agent in it"); err != nil {
			return failedRestoredArchiveResult(instance, restoredPath, err)
		}
	}
	// The worktree is back where af put it — moved back, or rebuilt or adopted
	// and now verified at the respawn boundary — so a worktree-missing flag on
	// this row is stale, and the respawn and the commits after it must not carry
	// it (#5102). The flag is sticky by design: only af re-placing the worktree
	// clears it, and both routes above are such a placement. It needs no persist
	// of its own: either commit below carries it, and a crash during the respawn
	// leaves the safer Missing=true from the first commit. Unconditional, because
	// it is a no-op on a row that was never flagged.
	instance.ClearWorktreeMissing()

	// RestoreFromArchive reaches LocalBackend's Recover → respawn, which can
	// rebuild the restored path if it vanishes before being inspected — and a
	// rebuild arms a stale-pane teardown obligation that must be durable at
	// both its transitions (#5371 review). Install the checkpoint the setter
	// requires.
	instance.SetRebuildTeardownNotify(m.rebuildTeardownNotify(repoID))
	instance.SetRebuildTeardownRuntimesStop(func(i *session.Instance) error {
		return m.stopVSCodeForInstance(key, i.ID)
	})
	restoreRuntimeErr := instance.RestoreFromArchiveHeldFenced()
	// LocalBackend's Recover can rebuild if the restored path vanishes before it
	// inspects it. Keep the same admission through that one rebuild choke point,
	// then release as soon as the runtime call returns; persistence and reporting
	// below cannot register a worktree and must not delay a waiting create.
	if worktreeAdmissionHeld {
		worktreeAdmissionHeld = false
		worktreeAdmission.Unlock()
	}
	if err := restoreRuntimeErr; err != nil {
		if perr := commitRestore(); perr != nil {
			return failedRestoredArchiveResult(instance, restoredPath, fmt.Errorf("%w; its agent also failed to re-spawn: %v", perr, err))
		}
		return failedRestoredArchiveResult(instance, restoredPath, fmt.Errorf("restored worktree for %q but failed to re-spawn its agent (it will be retried): %w", req.Title, err))
	}
	m.noteRuntimeReplaced(repoID, instance)
	// Same off-ramp as the remote arm above: a successful archive restore
	// closes the flap episode the pre-archive confirmations counted, and
	// retires a pending awaitingConfirm arm with it (#5436).
	m.clearLostRestoreEpisode(stableSessionKey(repoID, instance))
	worktreePath := instance.GetWorktreePath()
	if perr := commitRestore(); perr != nil {
		return failedRestoredArchiveResult(instance, worktreePath, fmt.Errorf("re-spawned the agent for %q, but %w", req.Title, perr))
	}
	m.info().Printf("restored session %q (repo %s): %s %s, agent re-spawned", req.Title, repoID, placement, worktreePath)
	return restoredArchiveResult(instance, worktreePath)
}

// moveArchivedWorktreeBack is the restore route for an archived worktree whose
// relocation claim resolved a directory: it moves that directory back next to
// the repo and re-registers it, reporting every failure already persisted and
// worded for the caller to return.
func (m *Manager) moveArchivedWorktreeBack(repoID, title string, instance *session.Instance, relocationClaim sessiongit.RelocationClaim) error {
	claimTransferred := false
	defer func() {
		if !claimTransferred {
			instance.PreserveWorktreeRelocationClaimForRetry(relocationClaim)
		}
	}()

	// A persisted worktree-missing flag means the directory af tracked was
	// deleted (#5102). A claim that now succeeds found SOMETHING at that path,
	// and by default nothing proves it is the session's worktree — a mkdir, an
	// unrelated checkout, a restore from backup all look the same from here.
	// Moving it back and starting the agent in it would hand its files to the
	// agent, so it may continue only on git's own proof that the claimed
	// directory is this session's checkout (VerifyClaimedWorktree) — the user
	// putting the archive back, which the rebuild route's refusal asks for.
	// Anything else refuses closed: once the path is cleared, the retry takes the
	// rebuild route. The flag is sticky exactly so a reappearing path cannot
	// quietly re-qualify the row; the caller clears it once the restore commits.
	if missing, _ := instance.WorktreeMissing(); missing {
		verifyErr := instance.VerifyClaimedWorktree(relocationClaim)
		if verifyErr != nil {
			claimTransferred = true
			instance.ReturnWorktreeClaimUnverified(relocationClaim)
			refusal := fmt.Errorf(
				"cannot restore session %q: its archived worktree %s was recorded as deleted outside af, and the directory now at that path is not one git proves is the session's checkout (%v) — af will not move it back or start the agent in it; inspect %s, remove it or move it aside if it is not the session's work, then retry (restore then rebuilds the worktree from its kept branch)",
				title, relocationClaim.Path, verifyErr, relocationClaim.Path)
			if perr := m.persistUnderAdmission(repoID, instance); perr != nil {
				return errors.Join(refusal, fmt.Errorf("could not persist its unverified-worktree fence: %w", perr))
			}
			return refusal
		}
		m.info().Printf("restore of session %q: git proves the directory at %s is the session's own checkout (put back after it was recorded as deleted); moving it back", title, relocationClaim.Path)
	}

	repoPath := instance.GetRepoPath()
	if repoPath == "" {
		return fmt.Errorf("cannot restore session %q: no repo path on record", title)
	}
	// Repo-gone check up front: SiblingWorktreePath and the worktree move both
	// need the origin repo, so surface the actionable message (archive left
	// intact) before either fails with a generic error.
	if repoGone, err := m.guardRepoGoneRestore(repoID, title, repoPath, instance, relocationClaim); err != nil {
		claimTransferred = repoGone
		return err
	}
	// Honor the configured worktree_root placement, exactly as session creation
	// does (#1540): a subdirectory user's worktree is restored under
	// $AF_HOME/worktrees/<branch>, not stranded beside the repo. The branch is
	// needed only for subdirectory placement.
	beforeRestoreWorktreePath()
	dest, err := sessiongit.RestoreWorktreePath(repoPath, title, instance.GetBranch())
	if err != nil {
		claimTransferred = true
		return m.persistRestorePathFailure(repoID, title, instance, relocationClaim, err)
	}
	beforeRestoreWorktreeUse()

	// Move the worktree back next to the repo. A repo-gone failure leaves the
	// archive intact (the git layer guarantees this) and surfaces an actionable
	// message; the instance stays Archived.
	claimTransferred = true
	restoreWorktreeErr := instance.RestoreArchivedWorktreeHeldFencedWithClaim(dest, relocationClaim)
	if err := restoreWorktreeErr; err != nil {
		if errors.Is(err, sessiongit.ErrRepoGone) {
			return m.persistRepoGoneAtRestoreUse(repoID, title, repoPath, instance, err)
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
			if perr := m.persistUnderAdmission(repoID, instance); perr != nil {
				return fmt.Errorf("restore of %q was cut off mid-relocate AND its recovery candidates could not be written to disk (%v); worktree recovery location: %s — inspect those path(s) before restarting the daemon: %w", title, perr, worktreeRecoveryLocation(instance), err)
			}
			return fmt.Errorf("restore of %q was cut off mid-relocate; both possible worktree locations are recorded (%s), destructive cleanup is blocked, and a retry will resolve the directory identity: %w", title, worktreeRecoveryLocation(instance), err)
		}
		return m.persistUnresolvedRestoreFailure(repoID, title, instance, relocationClaim, err)
	}
	return nil
}
