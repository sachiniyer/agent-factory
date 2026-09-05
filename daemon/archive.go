package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// ErrAlreadyArchived is returned by ArchiveSession when its target is already in
// the archived state. It is a SENTINEL — not just prose — because "already
// archived" is the one archive rejection that means the caller's goal is already
// met: a bulk caller that just wants the session archived (DeleteProject, #2108)
// must be able to tell it apart from a genuine failure (busy, op in flight,
// teardown error) with errors.Is, and count it as success instead of reporting a
// misleading partial failure. Only in-process callers can match it; over the
// control RPC it degrades to its (unchanged) message.
var ErrAlreadyArchived = errors.New("already archived")

// beforeArchiveOperationLock fires immediately before archiveSession waits for
// the session's operation lock, and beforeArchiveWorktreePath immediately before
// the local route derives its destination — the first step the archive fence has
// to already cover. Both are no-ops in production; the #3715 tests substitute
// stand-ins so they can assert what the daemon advertises and admits while an
// archive is parked in front of that lock, and that the fence is up before the
// route touches anything.
var (
	beforeArchiveOperationLock = func() {}
	beforeArchiveWorktreePath  = func() {}
)

func describeTargetTasks(tasks []task.Task) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if strings.TrimSpace(t.Name) == "" {
			parts = append(parts, fmt.Sprintf("task %s", t.ID))
			continue
		}
		parts = append(parts, fmt.Sprintf("%q (task %s)", t.Name, t.ID))
	}
	return strings.Join(parts, ", ")
}

// ArchiveSession archives a session (#1028): it tears down the session's tmux
// (agent + shell/process tabs; web tabs have none and are preserved with their
// URLs, #1809) while PRESERVING the record, relocates the
// worktree to the global archive dir (<AF_HOME>/archived/<repoID>/<title>/), and
// marks the instance Archived. The instance stays in the manager map as an inert
// row (unlike Kill, which deletes it); a later RestoreArchived brings it back.
//
// Concurrency uses the same two gates as KillSession — the killsInFlight claim
// (so a concurrent kill or a second archive is rejected, and the Lost-restore /
// finish-kill passes skip it) and the per-session op-lock (so archive, kill and
// Lost-recovery never interleave) — but takes the LOCK FIRST, so the claim is
// never held across the wait for it (#3715). What the claim used to hold across
// that wait, the re-read under the lock now establishes. Returns the relocated
// worktree's new path.
// ArchiveSession archives the resolved session and returns the relocated worktree
// path AND the full committed projection of the session it ACTUALLY resolved and
// acted on. It publishes that projection before releasing the per-session operation
// lock so a later restore cannot overtake it, and clients learn Archived directly
// instead of depending on a second Snapshot request (#2680). Its stable id still
// prevents cross-repo title misrouting (#1592 Phase 5).
func (m *Manager) ArchiveSession(req ArchiveSessionRequest) (string, session.InstanceData, error) {
	return m.archiveSessionGuarded(req, nil)
}

// archiveSessionGuarded is ArchiveSession with a sessionTeardownGuard evaluated
// inside the archive fence. See sessionTeardownGuard; a nil guard is plain
// ArchiveSession.
func (m *Manager) archiveSessionGuarded(req ArchiveSessionRequest, guard sessionTeardownGuard) (string, session.InstanceData, error) {
	m.taskTargetMu.Lock()
	defer m.taskTargetMu.Unlock()
	return m.archiveSession(req, nil, guard)
}

// archiveSession performs ArchiveSession while taskTargetMu is already held.
// DeleteProject uses it inside its wider, preflight-to-commit task transaction
// and passes the project-wide target index it loaded once during preflight.
// A nil taskTargets means the caller has not supplied a serialized snapshot.
func (m *Manager) archiveSession(req ArchiveSessionRequest, taskTargets map[string][]task.Task, guard sessionTeardownGuard) (string, session.InstanceData, error) {
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	// Canonicalize to the resolved session's title so every guard, the
	// killsInFlight key, and the relocation key off the id-resolved identity,
	// not the request's title. req is a value copy, so this is local.
	req.Title = title
	if session.IsReservedTitle(req.Title) {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive the reserved %q session", req.Title)
	}
	if instance == nil {
		// A ghost disk record with no live instance has no in-memory worktree to
		// relocate; there is nothing coherent to archive.
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: it is not currently active", req.Title)
	}
	if instance.StartupStateUnknown() {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: its startup state is unknown, so af cannot safely move its workspace through an unconfirmed runtime binding; inspect it and explicitly remove it instead", req.Title)
	}
	if !instance.Capabilities().Archive {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive remote session %q: it has no local worktree to relocate", req.Title)
	}
	if instance.IsExternalWorktree() {
		// An in-place/external worktree (`af sessions create --here`, #1107 — also
		// how root is set up) IS the user's own working tree; archive relocates
		// the worktree, which MoveWorktree refuses for external worktrees. Reject
		// it HERE, in the upfront guard, so nothing is torn down for a session
		// that can never be archived — otherwise the rejection would only surface
		// in the move step, after tmux is already down, leaving a broken
		// half-archive that rolls back to Lost.
		return "", session.InstanceData{}, fmt.Errorf("cannot archive an in-place/external worktree session %q — archive relocates the worktree, which isn't supported for in-place sessions", req.Title)
	}
	if instance.GetLiveness() == session.LiveArchived {
		// Sentinel-wrapped, and returned WITH the resolved identity: a caller that
		// only wants the session archived (DeleteProject, #2108) can treat this as
		// idempotent success and still report the right {id, title}, while a caller
		// archiving one named session — the CLI/TUI verbs — keeps the same message
		// and the same failure it has always shown.
		return "", instance.ToInstanceData(), fmt.Errorf("session %q is %w", req.Title, ErrAlreadyArchived)
	}
	if instance.GetInFlightOp() != session.OpNone {
		return "", session.InstanceData{}, fmt.Errorf("session %q is busy (%v); try again in a moment", req.Title, instance.GetStatus())
	}

	// Archiving is deliberate quiescence. Never auto-restore the target behind
	// the user's back, and never silently disable an automation as an archive
	// side effect. Refuse once, before the archive fence or teardown, and name
	// every enabled task the caller must disable or retarget. This also makes
	// DeleteProject fail visibly for the affected session rather than leaving an
	// enabled task in the same permanent retry loop (#2646).
	targeted := taskTargets[req.Title]
	if taskTargets == nil {
		var taskErr error
		targeted, taskErr = m.enabledTasksTargetingSession(repoID, req.Title)
		if taskErr != nil {
			return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: could not determine whether enabled tasks target it; nothing was changed: %w", req.Title, taskErr)
		}
	}
	if len(targeted) > 0 {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: enabled task(s) target it: %s; disable or retarget them, then archive again", req.Title, describeTargetTasks(targeted))
	}
	archiveTargetTasksChecked()

	key := daemonInstanceKey(repoID, req.Title)

	// The op-lock is taken BEFORE the claim, and that order is the whole of #3715
	// — the same order #3713 gave both manual restore routes, for the same reason.
	//
	// Claiming first put killsInFlight up for the length of this wait, bounded at
	// opLockTimeout (30s). From the claim onward a Kill is refused at the admission
	// gate with "kill already in progress" — about an operation the user never
	// started — while the row sat at {live, OpNone} and canKillFor, which reads the
	// op axis alone, went on advertising that Kill. Taking the lock first makes the
	// wait hold nothing: the row is honestly unclaimed and unfenced for its whole
	// duration, so the Kill it advertises is admitted and bounded by whoever really
	// holds the lock.
	//
	// Nothing here inverts a lock order. The canonical one is target-before-op
	// (#2006) and archive takes no target lock; m.mu is never held across an
	// op-lock acquisition, and holding the op-lock while briefly taking m.mu is
	// what the re-verify below already does. taskTargetMu is held by both callers
	// across this whole function either way, so its relationship to the op-lock is
	// unchanged.
	beforeArchiveOperationLock()
	opLock, waited, err := m.lockSessionOperationWithin(key, "archive", req.Title)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	defer opLock.Unlock()

	// Re-verify under the op-lock: findSession released m.mu, so a racing kill
	// may have torn the session down (map entry gone/replaced) or tombstoned it.
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance || instance.UserKilled() {
		return "", session.InstanceData{}, fmt.Errorf("session %q changed state before archive could start%s", req.Title, afterOpLockWait(waited))
	}
	// The archived and in-flight-op guards are re-read HERE, and with #3715 that is
	// load-bearing rather than defensive. Their up-front copies above ran before the
	// wait, and it was the CLAIM that used to hold them across it: a second archive
	// was refused at the claim before it ever queued. With the claim behind the lock
	// both callers queue, one wins, and the loser arrives here at a row its peer has
	// already shelved. Unchecked it would reach BeginArchive from Archived, which the
	// transition layer rejects outright — so the guards have to be asked again, of
	// the state that is actually current, under the lock that made it stable.
	//
	// Sentinel-wrapped exactly as the up-front copy is, so DeleteProject's
	// idempotent-success path (#2108) reads the same answer whichever side of the
	// wait it is decided on.
	if instance.GetLiveness() == session.LiveArchived {
		return "", instance.ToInstanceData(), fmt.Errorf("session %q is %w", req.Title, ErrAlreadyArchived)
	}
	if instance.GetInFlightOp() != session.OpNone {
		return "", session.InstanceData{}, fmt.Errorf("session %q is busy (%v); try again in a moment%s", req.Title, instance.GetStatus(), afterOpLockWait(waited))
	}

	// The claim, now adjacent to the fence each route raises immediately below.
	// Refusals decided after a real wait say so, or a 30s delay reads as a hang.
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return "", session.InstanceData{}, fmt.Errorf("an operation is already in progress for session %q%s", req.Title, afterOpLockWait(waited))
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	// The caller's own precondition, asked with BOTH the op-lock and the claim
	// held and before anything is torn down — the same placement the kill path
	// uses, and the reason a stale decision cannot be acted on here (#3865).
	if err := m.runSessionTeardownGuard(guard, key); err != nil {
		return "", session.InstanceData{}, err
	}

	// An off-box session (docker/ssh/hook) archives by pushing its branch to origin
	// and reaping the sandbox, not by relocating a worktree it does not have (#1592
	// Phase 4 PR6). Route it to the remote body, which shares this method's guards,
	// locks, and archive fence.
	if instance.Capabilities().Workspace == session.WorkspaceRemote {
		archivedPath, rerr := m.archiveRemoteSession(repoID, instance, req.Title)
		if rerr != nil && !isMutationCommitted(rerr) {
			return "", session.InstanceData{}, rerr
		}
		// A committed rerr still reaches here: the branch is on origin and the
		// sandbox is reaped, so callers get the location and projection, and
		// clients reconcile the completed transition off this event (#3235).
		archived := instance.ToInstanceData()
		m.publishEvent(agentproto.EventSessionArchived, archived)
		return archivedPath, archived, rerr
	}

	// Raise the archive fence HERE, at the top of the local route, so it is
	// ADJACENT to the claim above (#3715) rather than sitting behind the
	// destination derivation and the relocation claim.
	//
	// claimRestoreOperation's sibling reasoning applies: the claim is what makes a
	// Kill fail, canKillFor hides the affordance and reads the op axis alone, so
	// every statement between the claim and this raise was a stretch where the row
	// advertised a Kill that could only come back "an operation is already in
	// progress". The steps below are bounded and touch nothing destructive, but
	// they are not free, and the fence costs nothing to raise first.
	//
	// The remote route above is deliberately left to raise its own: BeginArchive is
	// the FIRST statement in archiveRemoteSession, so the prefix a hoist would cover
	// there is a capability read — the same call #3596 made for the archived-restore
	// remote route.
	// What the fence itself buys, unchanged by the move (#1195 Phase 2d): BeginArchive
	// sets OpArchiving (I4) so the status poll skips this instance — and the
	// checkpoint save skips persisting a mid-op row — while tmux is down and the
	// worktree is mid-move, never misread as Lost. started stays true throughout so
	// a move failure self-heals via the Lost loop, and OpArchiving is a DISTINCT op
	// from a kill so the fence can never be confused with a TUI optimistic kill
	// (#1187). The guards re-read under the op-lock above guarantee this edge is
	// legal; a rejection surfaces here, before any teardown.
	if err := instance.Transition(session.BeginArchive()); err != nil {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: %w", req.Title, err)
	}

	beforeArchiveWorktreePath()
	dest, err := archivedWorktreePath(repoID, req.Title)
	if err != nil {
		// CancelArchive, not AbortArchiveToLost: nothing has touched the runtime or
		// the worktree yet, so the session keeps the liveness it had. Dropping it to
		// Lost here would tell the #1108 loop to re-spawn an agent that never stopped.
		_ = instance.Transition(session.CancelArchive())
		return "", session.InstanceData{}, err
	}
	// A prior bounded move may have left two durable pathname candidates. Resolve
	// their captured directory identity before deriving any worktree-dependent
	// context: the operator hook must run in the directory that actually holds
	// the user's files, and a persist failure must roll back to that same origin.
	// Resolution is bounded and read-only; failure leaves panes and the archive
	// fence untouched.
	relocationClaim, err := instance.ClaimWorktreeRelocationForRetry()
	if err != nil {
		// Same pre-teardown cancel as the arm above, and it runs BEFORE the persist
		// below so the record this writes carries the row's real op, not the fence.
		_ = instance.Transition(session.CancelArchive())
		if errors.Is(err, sessiongit.ErrRelocateStateUnknown) {
			// Claim can create the first durable stall record. This return is before
			// every later archive persist, so write it now; otherwise a daemon restart
			// turns the newly recorded unknown back into an absent, destructive default.
			if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
				return "", session.InstanceData{}, fmt.Errorf(
					"cannot resolve archive source for session %q and could not persist its recovery record (%v); inspect %s before restarting the daemon: %w",
					req.Title, persistErr, worktreeRecoveryLocation(instance), err,
				)
			}
		}
		return "", session.InstanceData{}, fmt.Errorf(
			"cannot retry archive of session %q until its interrupted worktree move is resolved: %w",
			req.Title, err,
		)
	}
	// The pre-archive worktree location, captured before the move, so a persist
	// failure after the commit can roll the worktree back home (#1538).
	origPath := relocationClaim.Path

	// Stop this session's VS Code editor BEFORE the worktree moves. Ordering is
	// load-bearing for the same reason the pane-exit wait is: the editor's cwd is
	// the worktree, and moving a directory out from under a live code-server
	// leaves it serving a path that no longer exists. It is daemon-owned
	// infrastructure, not a tab, so ArchiveTeardown does not cover it. No-ops when
	// the session never had a vscode tab.
	//
	// The tab itself is KEPT by ArchiveTeardown (it is metadata-only: stopping the
	// editor destroys nothing the tab holds) and persisted with the archived
	// record, so a restore renders it again and the next proxy request lazily
	// respawns an editor on a fresh port. That is what makes stopping here safe —
	// it is a stop, not a delete.
	//
	// The final sweep is the belt to that brace: the webtab proxy may spawn an
	// editor without this op-lock (a spawn is too slow to hold it), so a request
	// racing this teardown could start one after the stop below. BeginArchive above
	// already makes ensureVSCodeServer refuse, so this should never fire — it is
	// here so the "archived ⇒ no editor" invariant does not depend on that being
	// true forever. Its result is checked below before the durable archive commit.
	vscodeKey := daemonInstanceKey(repoID, req.Title)
	if err := m.stopVSCodeForInstance(vscodeKey, instance.ID); err != nil {
		_ = instance.Transition(session.CancelArchive())
		instance.PreserveWorktreeRelocationClaimForRetry(relocationClaim)
		m.persistInstance(repoID, instance)
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q because its VS Code editor teardown could not be confirmed; no session teardown was started: %w", req.Title, err)
	}

	// Cancel and JOIN the in-process post-worktree hook runner before anything
	// touches the tree. Archive relocates the checkout, so this is the #2770
	// ordering the rebuild and cleanup paths already keep, and it must happen
	// before the on-archive hook's own survivor sweep: stopping a live runner's
	// current SCOPE without cancelling its context makes it treat the dead command
	// as an ordinary failure and start the next configured command in a fresh
	// scope — into a tree that is about to move (#3650 review). It also stops any
	// survivor from a previous daemon generation, which has no in-process handle.
	//
	// A failure here cancels the archive the same way the editor teardown above
	// does, and for the same reason: it is not "the hook failed", which teardown
	// deliberately treats as best-effort, but "a process may still be writing into
	// the tree we are about to move", which nothing downstream re-checks.
	if worktree, wtErr := instance.GetGitWorktree(); wtErr == nil && worktree != nil {
		if err := worktree.CancelAndJoinHooks(); err != nil {
			_ = instance.Transition(session.CancelArchive())
			instance.PreserveWorktreeRelocationClaimForRetry(relocationClaim)
			m.persistInstance(repoID, instance)
			return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q because its post-worktree hooks could not be confirmed stopped; no session teardown was started: %w", req.Title, err)
		}
	}

	// Tear down tmux and relocate the worktree in one call: the move is folded
	// into the teardown core immediately after the pane-exit wait (#1195 Ph2b),
	// so no live pane is cwd'd in the worktree during the move (previously a
	// separate MoveArchivedWorktree step relying on duplicated ordering prose).
	// Trusting: this call sits inside the per-session op-lock + killsInFlight
	// acquired by ArchiveSession/archiveSession above, which rules out a
	// same-name replacement appearing mid-teardown (#3413) — see
	// session.Instance.ArchiveTeardownWithClaim.
	hookErr, err := archiveTeardown(instance, dest, relocationClaim, func() error {
		return runOnArchiveHook(onArchiveHookContext{
			sessionID:   instance.ID,
			title:       req.Title,
			repoRoot:    instance.GetRepoPath(),
			worktree:    origPath,
			archivePath: dest,
		})
	}, true)
	if err != nil {
		// The worktree is still at a valid location (the git layer guarantees
		// worktreePath points at the actual bytes even on a repair failure).
		// Roll the fence back to Lost — started is still true and the agent tmux
		// binding was kept — so the Lost-restore loop re-spawns the agent in place.
		// Persist the recovery-eligible state, then surface the failure.
		//
		// The error-returning persist, for the same reason as the restore path
		// below: since the relocate became bounded, this branch is reachable with
		// the worktree already moved to dest and only the in-memory object knowing
		// it. A logged-and-swallowed write failure would leave the record pointing
		// at the pre-archive path, so a restart would send the Lost-restore loop to
		// rebuild a worktree whose bytes are sitting in the archive directory.
		//
		// The hook ran BEFORE the move, so this branch is reachable with both
		// failing, and the move error alone is not the whole outcome (#2763).
		// failedArchiveWithHook is what keeps the hook failure attached and logged;
		// this path never reaches the committed-archive warning below.
		_ = instance.Transition(session.AbortArchiveToLost())
		if perr := m.persistInstanceErr(repoID, instance); perr != nil {
			return failedArchiveResult(instance, failedArchiveWithHook(req.Title, fmt.Errorf(
				"failed to archive session %q AND could not record its recovered state on disk (%v); worktree recovery location: %s — inspect those path(s) before restarting the daemon: %w",
				req.Title, perr, worktreeRecoveryLocation(instance), err), hookErr))
		}
		return failedArchiveResult(instance, failedArchiveWithHook(req.Title, fmt.Errorf(
			"failed to archive session %q (its agent will be restored in place): %w", req.Title, err), hookErr))
	}

	// Success: worktree relocated, tmux down. Commit the inert Archived state
	// (started=false, op cleared) — reachable only from the fence (I2) — then
	// persist the new path + status DURABLY.
	//
	// Unlike the best-effort status poll, a swallowed persist failure here is
	// unsafe (#1538): the on-disk record would still point at the pre-archive
	// worktree with started=true, so after a daemon restart the Lost-restore loop
	// would try to rebuild the worktree at the old path and hit "branch already
	// checked out at <archivedPath>", orphaning the archive — the user can no
	// longer reach it via af. So on a persist failure, undo the physical archive:
	// move the worktree back home and drop the session to Lost, leaving the
	// on-disk record and reality consistent again and letting the #1108 loop heal
	// it in place. (The tiny window between the move and this persist completing —
	// a crash there — is inherent without a write-ahead journal; the reproducible
	// persist-error cause this issue reports is fully closed.)
	_ = instance.Transition(session.CommitArchive())
	archivedPath := instance.GetWorktreePath()
	// Revocation of this session's sandbox callback credential (#2999) follows the
	// COMMITTED STATE, not any one exit path (#3012 review).
	//
	// Armed here, at the commit, and disarmed only where a rollback genuinely
	// succeeded and left the session Lost — a Lost session is still drivable, so it
	// keeps its credential. Every other return below leaves the archive committed
	// and the runtime reaped, including the two that could not move the worktree
	// home and persist the committed archive best-effort; those returned above the
	// revocation when it was a single statement, so a token copied out of the
	// sandbox kept authenticating for an inert session until the daemon restarted.
	//
	// Deliberately a defer rather than a third call site. This is the third place in
	// this PR where revocation had to be anchored to a commit point, and the first
	// two were fixed by moving a statement — which is exactly the shape that breaks
	// again the next time someone adds an exit. Armed-and-disarmed fails CLOSED for
	// any return added later, matching how the create path already handles a mint
	// that is abandoned.
	archiveCommitted := true
	defer func() {
		if archiveCommitted {
			m.sandboxTokens.revoke(instance.ID)
		}
	}()
	if stopErr := m.stopVSCodeForInstance(vscodeKey, instance.ID); stopErr != nil {
		if !instance.GetArchiveReport().Empty() {
			return m.keepIncompleteArchiveCommitted(
				repoID, archivedPath, instance, hookErr,
				fmt.Errorf("final VS Code editor teardown was not confirmed: %w", stopErr),
			)
		}
		m.err().Printf("archive of session %q: final VS Code editor teardown was not confirmed (%v); rolling back the moved worktree", req.Title, stopErr)
		if rbErr := m.undoCommittedArchive(repoID, instance, origPath); rbErr != nil {
			// The worktree cannot be returned home. Keep the committed archive
			// rather than claiming a live Lost session at a path that no longer
			// exists, and report it as committed — with the location and
			// projection — like the keepIncompleteArchiveCommitted sibling (#3235).
			return m.keepUnrollableArchiveCommitted(repoID, archivedPath, instance, hookErr, fmt.Errorf(
				"archived session %q to %s but could not confirm its final VS Code editor teardown (%v) and could not roll the worktree back (%v); it may need manual recovery",
				req.Title, archivedPath, stopErr, rbErr))
		}
		// Rolled back: the session is Lost, still drivable, so it keeps its credential.
		archiveCommitted = false
		// A clean rollback is still reachable with a failed on-archive hook behind
		// it — the hook ran before the move — and the hook may already have mutated
		// external resources. Surface both (#3452): the teardown failure is what the
		// caller acts on, the hook failure is what the operator must fix.
		return "", session.InstanceData{}, failedArchiveWithHook(req.Title, fmt.Errorf(
			"could not confirm final VS Code editor teardown for session %q; rolled the archive back and left it Lost for retry: %w",
			req.Title, stopErr), hookErr)
	}
	if perr := archivePersist(m, repoID, instance); perr != nil {
		if !instance.GetArchiveReport().Empty() {
			return m.keepIncompleteArchiveCommitted(
				repoID, archivedPath, instance, hookErr,
				fmt.Errorf("its durable state write failed: %w", perr),
			)
		}
		m.err().Printf("archive of session %q: failed to durably record the Archived state (%v); rolling back to keep the on-disk record consistent", req.Title, perr)
		if rbErr := m.undoCommittedArchive(repoID, instance, origPath); rbErr != nil {
			// Could not move the worktree home: the committed archive is the
			// safest remaining state. Keep it and report it as committed — with
			// the location and projection — like the sibling above (#3235).
			return m.keepUnrollableArchiveCommitted(repoID, archivedPath, instance, hookErr, fmt.Errorf(
				"archived session %q to %s but failed to record it durably (%v) and could not roll it back (%v); it may need manual recovery",
				req.Title, archivedPath, perr, rbErr))
		}
		// Rolled back: the session is Lost, still drivable, so it keeps its credential.
		// The hook failure rides along for the same reason as the sibling above.
		archiveCommitted = false
		return "", session.InstanceData{}, failedArchiveWithHook(req.Title, fmt.Errorf(
			"failed to durably archive session %q; rolled it back and left it Lost to be restored in place: %w",
			req.Title, perr), hookErr)
	}
	m.info().Printf("archived session %q (repo %s): tmux torn down, worktree moved to %s", req.Title, repoID, archivedPath)
	archived := instance.ToInstanceData()
	// Still inside opLock: lifecycle event order must match committed operation
	// order. Publishing after this method returns lets an immediate restore finish
	// and publish before this older Archived projection (#2680 Codex review).
	m.publishEvent(agentproto.EventSessionArchived, archived)
	if committedErr := archiveCommitWarning(instance, hookErr); committedErr != nil {
		m.warn().Printf("%v", committedErr)
		return archivedPath, archived, committedErr
	}
	return archivedPath, archived, nil
}

// undoCommittedArchive rolls a committed-but-unpersisted archive back to a
// self-healing live state (#1538). It moves the worktree back to its pre-archive
// location (origPath) and returns the instance to a plain Lost with started=true,
// via the only legal edge path out of the committed Archived state: BeginRestore
// (Archived -> Lost + OpRestoring, started=true) then AbortRestoreToLost (op
// cleared). With the worktree home and the record dropped to Lost, the on-disk
// record — still the pre-archive one, since the archive persist failed — matches
// reality, so a daemon restart re-spawns at the right path and the #1108 restore
// loop heals the agent in place. The rolled-back state is persisted best-effort;
// even if that write also fails, the worktree being home already keeps disk and
// reality consistent. Returns an error only when the move home itself fails.
func (m *Manager) undoCommittedArchive(repoID string, instance *session.Instance, origPath string) error {
	if err := instance.RestoreArchivedWorktree(origPath); err != nil {
		return err
	}
	_ = instance.Transition(session.BeginRestore())
	_ = instance.Transition(session.AbortRestoreToLost())
	m.persistInstance(repoID, instance)
	return nil
}

// archiveRemoteSession archives an off-box session (docker/ssh/hook) by pushing
// its branch to origin then reaping the sandbox (#1592 Phase 4 PR6) — the remote
// analogue of ArchiveSession's worktree-move body, sharing its guards, locks, and
// archive fence. GitHub holds the durable branch, so unlike the local path there
// is no worktree to move and no rollback-home on a persist failure: an archived
// remote record is already recoverable from origin.
func (m *Manager) archiveRemoteSession(repoID string, instance *session.Instance, title string) (string, error) {
	// Raise the archive fence (OpArchiving) so the status poll skips this instance
	// while its sandbox is torn down, exactly as the local path does.
	if err := instance.Transition(session.BeginArchive()); err != nil {
		return "", fmt.Errorf("cannot archive session %q: %w", title, err)
	}

	branch, err := instance.ArchiveSandbox()
	if err != nil {
		// Push and/or teardown failed. Roll the fence back to Lost so the session
		// stays recovery-eligible, persist that, and surface the failure.
		_ = instance.Transition(session.AbortArchiveToLost())
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("failed to archive session %q: %w", title, err)
	}

	// Success: branch is durable on origin, sandbox reaped. Commit the inert
	// Archived state (started=false, op cleared) and persist it durably.
	_ = instance.Transition(session.CommitArchive())
	// THIS is the archive path that matters for the sandbox callback credential
	// (#2999), and the one the first fix missed. BackendKind.InjectsSandboxCallback
	// is ssh and sandbox only, and both are WorkspaceRemote — so every session that
	// can hold a credential archives through HERE, while the revocation added for
	// the earlier review round sat in the local-worktree body, past a branch that
	// returns at the call site above. It never fired for a single credential-holding
	// session.
	//
	// Armed at the commit, with no disarm: unlike the local body there is nothing to
	// roll back here — the sandbox is already reaped and the branch is already on
	// origin — so every return below this line leaves the session committed and
	// inert, including the durability failure. The rollback that keeps a credential
	// is the ArchiveSandbox failure above, which aborts to Lost before this point
	// and so never arms.
	defer m.sandboxTokens.revoke(instance.ID)
	if perr := archivePersist(m, repoID, instance); perr != nil {
		// The sandbox is already reaped and the branch is on origin, so there is
		// nothing to undo — the Archived record is recoverable from origin either
		// way. Persist best-effort and surface the durability failure; even a lost
		// best-effort write leaves the on-disk record naming the pushed branch, so a
		// restart loads the session Lost and an explicit restore re-provisions it.
		m.err().Printf("archive of remote session %q: failed to durably record the Archived state (%v); branch %q is on origin, so the session stays restorable", title, perr, branch)
		// The committed claim must itself be durable before it is made (#3335
		// review), exactly as keepUnrollableArchiveCommitted enforces for the
		// local body: ArchiveSandbox records the pushed branch only in memory, so
		// if no write ever lands, a restart loads a row with an empty or stale
		// branch — the Lost re-provision then clones the repo's default branch and
		// strands the pushed work — and DeleteProject would convert the committed
		// marker to a warning and deregister the project over that row. Retry the
		// durable write; only its success claims committed, with the pushed
		// branch preserved (#3235). A second failure keeps the plain shape whose
		// message still names the branch for manual recovery.
		if retryErr := archivePersist(m, repoID, instance); retryErr != nil {
			return "", fmt.Errorf("archived remote session %q (branch %q pushed to origin) but failed to record it durably: %w", title, branch, errors.Join(perr, retryErr))
		}
		return branch, &mutationCommittedError{err: fmt.Errorf("archived remote session %q (branch %q pushed to origin) but its durable record initially failed to write: %w", title, branch, perr)}
	}
	m.info().Printf("archived remote session %q (repo %s): branch %q pushed to origin, sandbox reaped", title, repoID, branch)
	return branch, nil
}

// archiveExclusiveTabLock serializes a tab spawn against an archive/kill/restore
// teardown for the session keyed by key (#1195). Those ops hold the per-session
// op-lock across their whole tmux teardown + worktree move, and — unlike Kill,
// which flips started=false — an archive keeps started=true throughout (so the
// #1108 rollback can self-heal a failed move to Lost), which means the
// instance-level #990 started guard never fires during archive. Without this, a
// CreateTab racing an ArchiveSession could spawn a tmux session into the worktree
// being moved out from under it, orphaning it.
//
// It takes the op-lock BEFORE any per-repo start lock the caller holds, matching
// the opLock→repoStartLock ordering the kill/archive paths use (persistInstance
// et al), so no ABBA deadlock is introduced. On success it returns the LOCKED
// op-lock — the caller must Unlock it. On rejection it releases the lock and
// returns an error: an archive/kill that beat us to the op-lock has fully
// completed by the time we acquire it (it held the lock across its entire
// teardown+move), so a mid-archive Deleting is never observed — only the terminal
// Archived. A restored kill tombstone has no in-flight op-lock state and may
// still be started, so UserKilled must be checked explicitly under this newly
// acquired lock rather than left to the instance-level started guard.
func (m *Manager) archiveExclusiveTabLock(key string, instance *session.Instance) (*sync.Mutex, error) {
	opLock := m.opLockFor(key)
	opLock.Lock()
	if instance.UserKilled() {
		opLock.Unlock()
		return nil, fmt.Errorf("cannot create a tab on killed session %q", instance.Title)
	}
	if err := instance.TabSpawnBlocked(); err != nil {
		opLock.Unlock()
		return nil, err
	}
	return opLock, nil
}

// persistInstanceErr writes one instance's authoritative data through the
// targeted per-repo writer under the repo start lock, mirroring
// refreshInstanceStatus's persist, and returns any write error. persistInstance
// wraps it for the best-effort callers; the archive commit uses this variant to
// make the persist durable (#1538).
//
// LOCK CONTRACT (#2106): it goes through startLockForRepo, which takes m.mu, so
// it must NEVER be called with m.mu held — see startLockForRepo. Under m.mu, call
// persistInstanceData directly instead.
func (m *Manager) persistInstanceErr(repoID string, instance *session.Instance) error {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	// Snapshot only AFTER serialization. Taking ToInstanceData as an argument to
	// a locking helper evaluates it before that helper acquires the lock, letting
	// a stale status write erase a kill tombstone (#1917).
	return persistInstanceData(repoID, instance.ToInstanceData())
}

// persistHandoffCheckpointErr is persistInstanceErr for the one writer whose
// durable and live projections differ ON PURPOSE: the post-swap handoff
// checkpoint. Disk takes the caller's atomic projection — the crash-recovery
// posture, which cannot retain a process-local operation — while memory keeps the
// OpReplacing delivery fence. It is intentionally separate from persistInstanceErr:
// ordinary writers must take their Instance snapshot only after acquiring this
// serialization lock, which is exactly why the caller's snapshot is a parameter here.
//
// The EVENT carries the live projection, not the checkpoint (#2782). Other clients
// have to render the truth — incoming agent, replacement still in flight — and the
// fence is what closes their action gates (CanHandoff/CanKill project off it).
// Announcing the checkpoint would tell them the swap had already settled and
// re-open gates the daemon is still holding shut.
func (m *Manager) persistHandoffCheckpointErr(repoID string, instance *session.Instance, checkpoint session.InstanceData) error {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	err := persistInstanceData(repoID, checkpoint)
	m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
	return err
}

// persistInstance is the best-effort form of persistInstanceErr: a failed write
// only logs. Used everywhere the persist is a checkpoint that the next poll/tick
// will re-attempt, never where the write's durability gates correctness.
//
// LOCK CONTRACT (#2106): inherits persistInstanceErr's — never call it with m.mu
// held. Under m.mu, call persistInstanceData directly.
func (m *Manager) persistInstance(repoID string, instance *session.Instance) {
	if err := m.persistInstanceErr(repoID, instance); err != nil {
		m.warn().Printf("failed to persist instance %q: %v", instance.Title, err)
	}
}

// persistAndPublishInstance is persistInstance plus the session.updated
// announcement a committed change owes every OTHER client (#1812, #2782).
//
// The two belong together wherever a mutation — not a poll — is what changed the
// session. A client that did not make the change learns of it ONLY from the events
// plane: it re-Snapshots after its own mutations, never after anyone else's. And
// the status poll cannot be relied on to repair the gap, because it deliberately
// skips a session with an operation in flight and, once the operation finishes,
// snapshots the already-final state as its own baseline — seeing no change to
// report.
//
// Published INSIDE the repo ordering lock, in the same critical section as the
// persist that produced it, for the reason spelled out at the poll's publish in
// limit.go: session.updated carries a WHOLE InstanceData and every client
// re-projects the session wholesale from it, so publishing after the unlock lets an
// older payload land last and revert newer state.
//
// The announcement goes out even when the write failed. That is deliberate and
// matches the poll: what the other clients must converge on is MEMORY, which has
// already changed. persistInstance is best-effort by contract — the next checkpoint
// retries the write — so a failed write is not a reason to leave every other client
// stale about a change that really happened.
//
// LOCK CONTRACT (#2106): inherits persistInstance's — never call it with m.mu held.
func (m *Manager) persistAndPublishInstance(repoID string, instance *session.Instance) {
	if err := m.persistAndPublishInstanceErr(repoID, instance); err != nil {
		m.warn().Printf("failed to persist instance %q: %v", instance.Title, err)
	}
}

// persistAndPublishInstanceErr is persistAndPublishInstance with the write error
// RETURNED rather than logged, for callers whose durability gates correctness
// rather than merely dating a checkpoint. Settlement writers that also maintain
// retry bookkeeping use persistSettlement so that bookkeeping remains in the
// same repo-ordered critical section as the write. The announcement stays
// unconditional either way — memory has already changed, so every other client
// must converge on it whether or not disk agreed yet.
func (m *Manager) persistAndPublishInstanceErr(repoID string, instance *session.Instance) error {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	// Snapshot only AFTER serialization, for the #1917 reason on persistInstanceErr:
	// a projection taken before the lock can erase state a writer committed while we
	// waited. The same snapshot is both persisted and announced, so disk and the
	// events plane can never disagree about which one this was.
	data := instance.ToInstanceData()
	err := persistInstanceData(repoID, data)
	m.publishEvent(agentproto.EventSessionUpdated, data)
	repoStartLock.Unlock()
	return err
}

// archivePersist is the durable persist ArchiveSession runs at its commit. A
// package var so tests can force a persist failure in isolation (exercising the
// rollback in #1538) without disturbing any other persist. Production points it
// at persistInstanceErr.
var archivePersist = (*Manager).persistInstanceErr

// archiveTeardown is the physical teardown+hook+move ArchiveSession runs before
// its durable commit. Indirected so race tests can install an editor in the
// exact post-move window without weakening the production ordering.
var archiveTeardown = (*session.Instance).ArchiveTeardownWithClaim

// archivedWorktreePath returns the global archive location for a session's
// relocated worktree: <AGENT_FACTORY_HOME>/archived/<repoID>/<safeTitle>/. The
// repoID namespace prevents cross-repo title collisions; the title is sanitized
// for filesystem safety (the same scheme NewGitWorktree uses for session dirs).
func archivedWorktreePath(repoID, title string) (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve archive directory: %w", err)
	}
	return filepath.Join(dir, "archived", repoID, sanitizeArchiveTitle(title)), nil
}

// sanitizeArchiveTitle makes a session title safe as a single path segment,
// mirroring NewGitWorktree's safeSessionName handling (strip "..", "/"→"-",
// trim leading separators), falling back to "session" when nothing remains.
func sanitizeArchiveTitle(title string) string {
	s := strings.ReplaceAll(title, "..", "")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.TrimLeft(s, "-.")
	if s == "" {
		s = "session"
	}
	return s
}
