package daemon

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// archiveSessionSourceAbsent routes an archive whose relocation claim found the
// tracked worktree conclusively absent with no recovery record (#5102). Two
// histories produce that, and they must not be confused:
//
//   - The worktree was deleted outside af. There is nothing to move; the row is
//     archived as missing and restore rebuilds from the kept branch. (A user's
//     own `git worktree move` elsewhere looks the same from here, so the
//     branch's live registration is checked before concluding deletion.)
//   - An earlier archive's move landed at dest, and the daemon died before it
//     recorded the new path. The bytes are all at dest. Archiving that as
//     "missing" would commit a row pointing at nothing and orphan them.
//
// dest is deterministic for a title, so its state separates the two. Absent:
// a deletion. Occupied and proven by git to be this session's own worktree:
// the landed move, adopted in place. Occupied by anything else, or
// unanswerable: refuse, because af cannot tell which history it is looking at
// and both wrong answers lose track of someone's bytes. Every refusal here is
// before teardown, so it cancels the fence and leaves the session as it was.
func (m *Manager) archiveSessionSourceAbsent(repoID, title string, instance *session.Instance, dest string) (string, session.InstanceData, error) {
	refuse := func(err error) (string, session.InstanceData, error) {
		_ = instance.Transition(session.CancelArchive())
		return "", session.InstanceData{}, err
	}
	// Hold dest for the whole decision and archive: a concurrent archive of a
	// same-leaf title must not claim it between the probe and the commit.
	if err := m.reserveArchiveDestination(repoID, instance, dest); err != nil {
		return refuse(err)
	}
	defer m.releaseArchiveDestination(repoID, instance, dest)

	source := instance.GetWorktreePath()
	info, err := sessiongit.BoundedLstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		// Source and destination both absent is also exactly the footprint of a
		// user's own `git worktree move` to some other path, and that worktree is
		// intact: archiving it as missing would strand it, and restore would then
		// refuse it as a foreign checkout. git's registration follows such a move,
		// so ask where the branch lives before calling this a deletion. A
		// deletion leaves either no registration or a stale one at a path that
		// no longer exists; an unreadable listing is no answer at all.
		moved, live, lerr := instance.LiveBranchCheckout()
		if lerr != nil {
			return refuse(fmt.Errorf(
				"cannot archive session %q: its worktree path %s is gone, and af could not read where git has its branch checked out, so it cannot tell a deletion from a move: %w",
				title, source, lerr))
		}
		if live {
			return refuse(fmt.Errorf(
				"cannot archive session %q: its worktree was moved outside af to %s (git has its branch checked out there) — move it back to %s, remove it with 'git worktree remove', or kill the session",
				title, moved, source))
		}
		return m.archiveSessionWorktreeGone(repoID, title, instance, false)
	}
	if err != nil {
		return refuse(fmt.Errorf(
			"cannot archive session %q: its worktree path %s is gone and archive destination %s cannot be inspected, so af cannot tell a deletion outside af from its own interrupted move: %w",
			title, source, dest, err))
	}
	// Another session's recorded archive is never ours to adopt, whatever git
	// says about branches.
	owner, err := m.archiveDestinationOwner(repoID, instance, dest, info)
	if err != nil {
		return refuse(fmt.Errorf("cannot archive session %q: destination %s already exists; cannot determine its owner: %w", title, dest, err))
	}
	if owner != "" {
		return refuse(fmt.Errorf("cannot archive session %q: its worktree path %s is gone, and destination %s already exists and belongs to session %q", title, source, dest, owner))
	}
	if err := instance.AdoptLandedArchiveMove(dest); err != nil {
		return refuse(fmt.Errorf(
			"cannot archive session %q: its worktree path %s is gone, but archive destination %s already exists and is not provably this session's worktree (%v) — a previous archive's move may have landed there before the daemon could record it; inspect %s and remove or restore it, then retry",
			title, source, dest, err, dest))
	}
	// Not persisted yet, deliberately: a write now would record the OpArchiving
	// fence. Every exit below persists after settling the op, and a crash before
	// any of them reloads the old path — whose retry lands back here and
	// re-adopts on the same proof.
	m.info().Printf("archive of session %q: %s was gone and git proves an earlier interrupted archive moved it to %s; adopting that location", title, source, dest)
	return m.archiveSessionWorktreeGone(repoID, title, instance, true)
}

// archiveSessionWorktreeGone archives a local session whose source worktree is
// already absent, by one of the two routes archiveSessionSourceAbsent chose
// (#5102), still holding the op-lock, the kill claim and the OpArchiving fence.
//
// It is the relocating route's tail with the move taken out: the editor and
// post-worktree hooks are stopped, tmux is torn down, the on-archive hook runs,
// and the row commits Archived. The branch is never touched. With adopted
// false the worktree was deleted outside af: the recorded path stays the
// deleted one and the row is stamped missing, so restore rebuilds from the
// branch rather than moving bytes back. With adopted true an earlier archive's
// move had landed at the destination, the record already names it, and the row
// commits exactly as a completed move would — no missing stamp.
//
// Because nothing moved, nothing can roll back. That removes the undo the
// relocating route performs on a late failure, but not the rule it protects
// (#3448/#3335): a committed outcome is claimed only once it is durable.
func (m *Manager) archiveSessionWorktreeGone(repoID, title string, instance *session.Instance, adopted bool) (string, session.InstanceData, error) {
	vscodeKey := daemonInstanceKey(repoID, title)
	if err := m.stopVSCodeForInstance(vscodeKey, instance.ID); err != nil {
		_ = instance.Transition(session.CancelArchive())
		m.persistInstance(repoID, instance)
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q because its VS Code editor teardown could not be confirmed; no session teardown was started: %w", title, err)
	}
	// The worktree is gone, but a post-worktree hook runner may still be alive
	// with its cwd in the unlinked directory. Join it before teardown for the
	// same #2770/#3650 reason the relocating route does: nothing downstream
	// re-checks that a process from it has stopped.
	if worktree, wtErr := instance.GetGitWorktree(); wtErr == nil && worktree != nil {
		if err := worktree.CancelAndJoinHooks(); err != nil {
			_ = instance.Transition(session.CancelArchive())
			m.persistInstance(repoID, instance)
			return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q because its post-worktree hooks could not be confirmed stopped; no session teardown was started: %w", title, err)
		}
	}

	origPath := instance.GetWorktreePath()
	// What the outcome messages say happened to the worktree.
	whereabouts := fmt.Sprintf("its worktree was already absent at %s", origPath)
	if adopted {
		whereabouts = fmt.Sprintf("its worktree was already at %s from an earlier interrupted archive", origPath)
	}
	// Trusting, for the same reason as the relocating route: the op-lock and
	// killsInFlight claim held by ArchiveSession rule out a same-name
	// replacement mid-teardown (#3413).
	// AF_ARCHIVE_PATH names where the worktree's bytes land in the archive. On
	// the adopted route they are already there — the adopted path IS the archive
	// destination — so the hook gets it exactly as the relocating route passes
	// its dest. A deletion lands nothing anywhere, so it stays empty there rather
	// than naming a directory that will never exist.
	archivePath := ""
	if adopted {
		archivePath = origPath
	}
	hookErr, err := archiveGoneTeardown(instance, func() error {
		return runOnArchiveHook(onArchiveHookContext{
			sessionID:   instance.ID,
			title:       title,
			repoRoot:    instance.GetRepoPath(),
			worktree:    origPath,
			archivePath: archivePath,
		})
	}, true, adopted)
	if err != nil {
		// Same recovery as the relocating route's teardown failure: drop to Lost
		// with started kept, so the Lost loop re-spawns the agent — in the
		// adopted worktree where it now is, or, for a deletion, in one its
		// rebuild recreates from the branch, the same self-heal a restore would
		// perform.
		_ = instance.Transition(session.AbortArchiveToLost())
		if perr := m.persistInstanceErr(repoID, instance); perr != nil {
			return failedArchiveResult(instance, failedArchiveWithHook(title, fmt.Errorf(
				"failed to archive session %q AND could not record its recovered state on disk (%v); %s: %w",
				title, perr, whereabouts, err), hookErr))
		}
		if errors.Is(err, sessiongit.ErrRelocateStateUnknown) {
			// The adopted directory could not be re-confirmed as the one git
			// proved, and the teardown fenced the record: recovery will not start
			// an agent there or clean it up until a retry re-establishes it.
			return failedArchiveResult(instance, failedArchiveWithHook(title, fmt.Errorf(
				"failed to archive session %q: %w — its worktree record is fenced, so af will not start an agent in or remove %s; inspect that path, then retry the archive",
				title, err, origPath), hookErr))
		}
		return failedArchiveResult(instance, failedArchiveWithHook(title, fmt.Errorf(
			"failed to archive session %q (its agent will be restored in place): %w", title, err), hookErr))
	}

	_ = instance.Transition(session.CommitArchive())
	if adopted {
		// git proved the bytes are at the path the record now names; any flag
		// a probe set while the record still named the vacated source is stale.
		instance.ClearWorktreeMissing()
	} else {
		instance.SetWorktreeMissing(session.WorktreeMissingArchivedReason(origPath))
	}
	// Revocation follows the committed state exactly as on the relocating route
	// (#2999/#3012). There is no rollback here to disarm it.
	archiveCommitted := true
	defer func() {
		if archiveCommitted {
			m.sandboxTokens.revoke(instance.ID)
		}
	}()
	if stopErr := m.stopVSCodeForInstance(vscodeKey, instance.ID); stopErr != nil {
		// The relocating route would move the worktree home and drop to Lost
		// here. This route moved nothing, so there is no home move to undo and
		// the committed archive is the only state available — claimed committed
		// only if it reached disk.
		if perr := archivePersist(m, repoID, instance); perr != nil {
			return "", session.InstanceData{}, failedArchiveWithHook(title, fmt.Errorf(
				"archived session %q in memory but could not confirm its final VS Code editor teardown (%v) or write the archive durably (%v); nothing was moved (%s)",
				title, stopErr, perr, whereabouts), hookErr)
		}
		archived := instance.ToInstanceData()
		m.publishEvent(agentproto.EventSessionArchived, archived)
		committedErr := archiveCommittedWarning(instance, hookErr, fmt.Errorf("final VS Code editor teardown was not confirmed: %w", stopErr))
		m.warn().Printf("%v", committedErr)
		return origPath, archived, committedErr
	}
	if perr := archivePersist(m, repoID, instance); perr != nil {
		return "", session.InstanceData{}, failedArchiveWithHook(title, fmt.Errorf(
			"archived session %q in memory but could not write it durably; nothing was moved (%s) so there is no rollback — a retry after a daemon restart takes this same route again: %w",
			title, whereabouts, perr), hookErr)
	}
	if adopted {
		m.info().Printf("archived session %q (repo %s): tmux torn down; adopted worktree at %s, where an earlier interrupted archive had moved it; branch kept", title, repoID, origPath)
	} else {
		m.info().Printf("archived session %q (repo %s): tmux torn down; worktree was already absent at %s (deleted outside af); branch kept", title, repoID, origPath)
	}
	archived := instance.ToInstanceData()
	// Inside opLock, for the event-ordering reason on the relocating route.
	m.publishEvent(agentproto.EventSessionArchived, archived)
	if committedErr := archiveCommitWarning(instance, hookErr); committedErr != nil {
		m.warn().Printf("%v", committedErr)
		return origPath, archived, committedErr
	}
	return origPath, archived, nil
}

// archiveGoneTeardown is archiveTeardown's counterpart for the missing-worktree
// route (#5102): the same tmux teardown and hook, no move. Indirected for the
// same reason.
var archiveGoneTeardown = (*session.Instance).ArchiveTeardownWorktreeGone
