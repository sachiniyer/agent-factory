package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// archiveSessionSourceAbsent routes an archive whose relocation claim found the
// tracked worktree conclusively absent with no recovery record (#5102). Three
// histories can produce that, and they must not be confused:
//
//   - The worktree was deleted outside af. There is nothing to move; the row is
//     archived as missing and restore rebuilds from the kept branch. (A user's
//     own `git worktree move` elsewhere looks the same from here, so the
//     branch's live registration is checked before concluding deletion.)
//   - An earlier archive's move landed at dest, and the daemon died before it
//     recorded the new path. The bytes are all at dest. Archiving that as
//     "missing" would commit a row pointing at nothing and orphan them.
//   - A cross-device archive move was cut off between its quarantine rename and
//     its publish rename. The bytes are all stranded at a record-free
//     `.af-source-<32hex>` sibling of the source (with a staged copy beside the
//     destination), the destination is absent, and no relocation record names
//     either. Archiving that as "missing" would commit the row Archived with the
//     worktree-missing stamp and let a later restore rebuild from the branch,
//     silently discarding the uncommitted work stranded under the unguessable
//     private name. af's own residue (`.af-source-`/`.af-copy-` plus 32 hex) is
//     detected before the deletion route so the retry refuses and points at it
//     instead.
//
// dest is deterministic for a title, so its state separates the first two. Absent
// with no af residue: a deletion. Occupied and proven by git to be this
// session's own worktree: the landed move, adopted in place. Occupied by
// anything else, or unanswerable: refuse, because af cannot tell which history
// it is looking at and both wrong answers lose track of someone's bytes. Every
// refusal here is before teardown, so it cancels the fence and leaves the
// session as it was.
//
// The landed-move route carries one more obligation (#7816): a cross-device
// archive publishes dest BEFORE persisting the new path and its ArchiveReport,
// and when skipUnreadable omitted files it also retains the COMPLETE original
// as a `.af-source-<32hex>` sibling of the source — a tree only that unwritten
// report references. A daemon death in that window lands here too: dest is
// occupied and provable and the source is absent, exactly like a complete
// landed move, but the retained original still survives beside the source with
// nothing naming it. Committing an empty report would orphan the only complete
// copy and let a later restore from dest silently lack the skipped files, so
// the dest-present branch first establishes whether such a quarantine survives
// — by the same binding the dest-absent branch uses — and adopts as "nothing
// retained" only on proven absence.
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
			// Moving it back terminates: the claim then finds the session's own
			// checkout at the recorded path, which git proves even on a row
			// already flagged missing (see ArchiveSession's missing guard).
			return refuse(fmt.Errorf(
				"cannot archive session %q: its worktree was moved outside af to %s (git has its branch checked out there) — move it back to %s with 'git worktree move' and retry, or remove it with 'git worktree remove' and retry to archive it as deleted",
				title, moved, source))
		}
		// Before classifying the absent source as a deletion outside af, reject
		// the record-free footprint of a cross-device archive move cut off
		// between its quarantine rename and its publish rename. The source is
		// gone (renamed to a `.af-source-<32hex>` sibling) and the destination is
		// absent (the publish rename never ran), but the COMPLETE worktree —
		// including any uncommitted changes — is stranded at that hidden
		// sibling and a staged copy sits at a `.af-copy-<32hex>` sibling of the
		// destination. No relocation record names either (the recovery record is
		// materialized only after the publish), so a bare ENOENT here lets the
		// retry commit the row Archived with the worktree-missing "deleted
		// outside af" stamp and lets a later restore rebuild from the branch,
		// silently discarding the stranded work behind an unguessable name no
		// record, log, or sweep references. Detect the residue and refuse,
		// pointing at it, so it is not archived over.
		residue, resErr := sessiongit.ListAFRelocationResidue(filepath.Dir(source), filepath.Dir(dest))
		if resErr != nil {
			return refuse(fmt.Errorf(
				"cannot archive session %q: its worktree path %s is gone and af could not scan for relocation residue beside it, so it cannot tell a deletion outside af from its own interrupted cross-device move: %w",
				title, source, resErr))
		}
		// The source parent and the archive root are shared by every
		// session, so residue blocks this archive only when it is proven to
		// be THIS session's worktree. Another session's interrupted move,
		// or a tree another session's archive retained on purpose, is
		// foreign and must not block this archive — and the refusal must
		// never tell the operator to move another session's tree into this
		// session's path. Verify each candidate's git binding against this
		// session's recorded source; a candidate whose binding cannot be
		// read still refuses (fail-closed), but the message names it as
		// unverified rather than this session's stranded work.
		//
		// The proven-ours residue is split by purpose: `.af-source-*` is the
		// renamed source — the COMPLETE worktree, including any uncommitted
		// changes — so recovery is directed from it. `.af-copy-*` is the
		// staged copy beside the destination, which under skipUnreadable may
		// intentionally omit unreadable paths; it must never be described as
		// the complete worktree, and the operator is never told to move it
		// back as the worktree (doing so and then removing the source
		// quarantine as redundant would silently discard the skipped files).
		var oursSource, oursCopy, unverified []string
		for _, candidate := range residue {
			binding, bErr := sessiongit.BindAFRelocationResidue(candidate, source, instance.GetRepoPath())
			if bErr != nil || binding == sessiongit.ResidueUnknown {
				unverified = append(unverified, candidate)
				continue
			}
			if binding == sessiongit.ResidueOurs {
				if strings.HasPrefix(filepath.Base(candidate), ".af-source-") {
					oursSource = append(oursSource, candidate)
				} else {
					oursCopy = append(oursCopy, candidate)
				}
			}
		}
		if len(oursSource) > 0 || len(oursCopy) > 0 || len(unverified) > 0 {
			named := append(append(append([]string{}, oursSource...), oursCopy...), unverified...)
			m.warn().Printf("archive of session %q refused: its worktree path %s is gone but af relocation residue is present at %s — a cross-device archive move appears to have been cut off between its quarantine and publish renames; the complete worktree may be stranded there and was not archived as deleted",
				title, source, strings.Join(named, ", "))
			if len(oursSource) > 0 || len(oursCopy) > 0 {
				// Direct recovery only from the `.af-source-*` quarantine
				// (the complete original); describe the `.af-copy-*` staging
				// copy as possibly incomplete and never as the worktree to
				// move back.
				var recovery strings.Builder
				if len(oursSource) > 0 {
					fmt.Fprintf(&recovery,
						"the complete worktree (including any uncommitted changes) is stranded at the .af-source-* quarantine %s — inspect it and move it back to %s with 'mv' (or remove the residue once you have verified the work is saved)",
						strings.Join(oursSource, ", "), source)
				} else {
					fmt.Fprintf(&recovery,
						"a staged copy proven to be this session's is present at %s but its .af-source-* quarantine is absent, and the copy may be incomplete if the interrupted archive skipped unreadable files — inspect %s and do not move it into %s unless you have verified it is complete",
						strings.Join(oursCopy, ", "), strings.Join(oursCopy, ", "), source)
				}
				if len(oursSource) > 0 && len(oursCopy) > 0 {
					fmt.Fprintf(&recovery,
						"; a staged copy is also present at the .af-copy-* sibling %s beside the destination and may be incomplete if the interrupted archive skipped unreadable files, so do not move it into %s as the worktree — recover from the .af-source-* quarantine instead",
						strings.Join(oursCopy, ", "), source)
				}
				return refuse(fmt.Errorf(
					"cannot archive session %q: its worktree path %s is gone, but af relocation residue proven to be this session's worktree is present at %s — a cross-device archive move was cut off between its quarantine and publish renames and %s before retrying the archive: %w",
					title, source, strings.Join(named, ", "), recovery.String(), sessiongit.ErrRelocateStateUnknown))
			}
			return refuse(fmt.Errorf(
				"cannot archive session %q: its worktree path %s is gone, but af relocation residue is present at %s and af could not verify whether it is this session's worktree — it may belong to another session, or its git binding could not be read; inspect %s, then move it out of %s to somewhere safe (or remove it once you have confirmed it holds nothing you need) and retry the archive; do not move it into %s unless you have proven it is this session's work (see #7819): %w",
				title, source, strings.Join(unverified, ", "), strings.Join(unverified, ", "), filepath.Dir(unverified[0]), source, sessiongit.ErrRelocateStateUnknown))
		}
		// The deletion route promises restore can rebuild from the kept branch,
		// which holds only if the branch is still there: a user who deleted the
		// worktree may have deleted the branch with it. That row is still worth
		// archiving — its tmux is torn down, nothing is moved, and archive is the
		// one action that resolves it — but it must say what it is rather than
		// promise a rebuild that can never run. Its reason records the deleted
		// branch, the outcome messages drop the "branch kept" claim, and restore
		// refuses with the same fact (rebuildAbsentArchivedWorktree checks the
		// branch again, since the user may recreate it). A probe that cannot
		// answer refuses: af will not guess which of the two it is archiving.
		branch, branchKept, berr := instance.RecordedBranchExists()
		if berr != nil {
			return refuse(fmt.Errorf(
				"cannot archive session %q: its worktree path %s is gone, and af could not tell whether its branch %s still exists, so it cannot say whether the archive could be restored: %w",
				title, source, branch, berr))
		}
		return m.archiveSessionWorktreeGone(repoID, title, instance, false, branchKept)
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
	// Before adopting dest, establish whether a quarantine of THIS session's
	// source survives beside the source (#7816). A cross-device archive
	// publishes dest and repairs its registration BEFORE the new path and
	// ArchiveReport are persisted; a daemon death in that window lands the
	// retry here, where dest looks like an ordinary landed move. When
	// skipUnreadable omitted files, though, the COMPLETE original was retained
	// as a `.af-source-<32hex>` sibling of the source — referenced only by the
	// report the crash never wrote. Adopting dest with an empty report would
	// leave that only complete tree untracked, and a later restore from dest
	// would silently lack the skipped files. The scan runs on the source
	// parent only: that is the one place a quarantine of this source can sit.
	//
	// Only `.af-source-*` candidates answer the question. The `.af-copy-*`
	// staging copy is never the retained original — it is what dest's own
	// publication consumed — so it is not even bound.
	//
	// Attribution shares the dest-absent branch's mechanism, anchored at dest
	// rather than the recorded source: the quarantine this adoption could
	// strand is one whose registration backpoints at the worktree git now
	// proves is this session's, and adoption can only reach this point after
	// repair re-aimed that backpointer at dest — so a candidate binding to
	// `dest/.git` is this session's stranded original. One binding to another
	// path is foreign and is never claimed. A candidate that cannot be bound
	// is NOT absence: the read that would classify it failed, so the adoption
	// refuses — it may be this session's retained tree that af cannot see —
	// but without claiming it is this session's work, exactly like the
	// dest-absent branch's unverified arm.
	residue, resErr := sessiongit.ListAFRelocationResidue(filepath.Dir(source))
	if resErr != nil {
		return refuse(fmt.Errorf(
			"cannot archive session %q: its worktree path %s is gone and archive destination %s already exists, but af could not scan for a retained source quarantine beside %s, so it cannot prove the landed destination is the only complete tree: %w",
			title, source, dest, filepath.Dir(source), resErr))
	}
	var oursQuarantine, unverifiedQuarantine []string
	for _, candidate := range residue {
		if !strings.HasPrefix(filepath.Base(candidate), ".af-source-") {
			continue
		}
		binding, bErr := sessiongit.BindAFRelocationResidue(candidate, dest, instance.GetRepoPath())
		if bErr != nil || binding == sessiongit.ResidueUnknown {
			unverifiedQuarantine = append(unverifiedQuarantine, candidate)
			continue
		}
		if binding == sessiongit.ResidueOurs {
			oursQuarantine = append(oursQuarantine, candidate)
		}
	}
	if len(oursQuarantine) > 0 {
		m.warn().Printf(
			"archive of session %q refused: its worktree path %s is gone and destination %s is present, but the .af-source-* source quarantine %s survives beside the source — the complete original an interrupted cross-device archive retained, which adopting the destination would leave untracked",
			title, source, dest, strings.Join(oursQuarantine, ", "))
		return refuse(fmt.Errorf(
			"cannot archive session %q: its worktree path %s is gone and destination %s already holds this session's worktree, but the .af-source-* quarantine %s of that same source survives — the complete original tree an interrupted cross-device archive retained beside the vacated source (the copy at %s may have skipped unreadable files); inspect %s, restore into %s anything the published copy lacks, then move the quarantine aside or remove it once the destination is verified complete, and retry the archive — af will not adopt %s while its retained original is untracked: %w",
			title, source, dest, strings.Join(oursQuarantine, ", "), dest, strings.Join(oursQuarantine, ", "), dest, dest, sessiongit.ErrRelocateStateUnknown))
	}
	if len(unverifiedQuarantine) > 0 {
		m.warn().Printf(
			"archive of session %q refused: its worktree path %s is gone and destination %s is present, but .af-source-*-shaped residue is present at %s beside the source and af could not verify whether it is this session's retained original",
			title, source, dest, strings.Join(unverifiedQuarantine, ", "))
		return refuse(fmt.Errorf(
			"cannot archive session %q: its worktree path %s is gone and destination %s already exists, but af relocation residue shaped like a source quarantine is present at %s beside the vacated source and af could not verify whether it is this session's retained complete original — it may belong to another session, or its git binding could not be read; inspect %s (if it is this session's quarantine, recover into %s anything the published copy lacks), then move it out of %s or remove it once you have confirmed it holds nothing you need, and retry the archive: %w",
			title, source, dest, strings.Join(unverifiedQuarantine, ", "), strings.Join(unverifiedQuarantine, ", "), dest, filepath.Dir(unverifiedQuarantine[0]), sessiongit.ErrRelocateStateUnknown))
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
	return m.archiveSessionWorktreeGone(repoID, title, instance, true, true)
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
func (m *Manager) archiveSessionWorktreeGone(repoID, title string, instance *session.Instance, adopted, branchKept bool) (string, session.InstanceData, error) {
	origPath := instance.GetWorktreePath()
	// What the archived row says about the worktree, and what the outcome
	// messages may claim about the branch (#5102).
	archivedReason := session.WorktreeMissingArchivedReason(origPath)
	branchOutcome := "branch kept"
	if !branchKept {
		archivedReason = session.WorktreeMissingBranchGoneReason(origPath)
		branchOutcome = "its branch was already deleted too, so restore has nothing to rebuild it from"
	}
	// Settle the worktree-missing flag to what this route has just proven,
	// BEFORE any exit below persists the row — the cancel paths included.
	if adopted {
		// AdoptLandedArchiveMove proved, by git registration and back-binding,
		// that the directory at the re-aimed path is this session's worktree.
		// A flag the poll set while the record still named the vacated source
		// is stale; persisting it beside the adopted path would make the
		// sticky-flag archive guard refuse this verified worktree forever.
		instance.ClearWorktreeMissing()
	} else {
		// The claim just proved the tracked path absent with no relocation
		// behind it — the probe's own evidence. Record it now, not only at the
		// commit: if teardown then finds a directory back at the path and fences
		// the row, the flag is what makes every retry refuse that unverified
		// directory instead of moving it (#5102).
		instance.SetWorktreeMissing(session.WorktreeMissingDeletedReason(origPath))
	}
	vscodeKey := daemonInstanceKey(repoID, title)
	if err := m.stopVSCodeForInstance(vscodeKey, instance.ID); err != nil {
		return "", session.InstanceData{}, m.cancelArchiveWorktreeGone(repoID, instance, origPath, adopted, fmt.Errorf("cannot archive session %q because its VS Code editor teardown could not be confirmed; no session teardown was started: %w", title, err))
	}
	// The worktree is gone, but a post-worktree hook runner may still be alive
	// with its cwd in the unlinked directory. Join it before teardown for the
	// same #2770/#3650 reason the relocating route does: nothing downstream
	// re-checks that a process from it has stopped.
	if worktree, wtErr := instance.GetGitWorktree(); wtErr == nil && worktree != nil {
		if err := worktree.CancelAndJoinHooks(); err != nil {
			return "", session.InstanceData{}, m.cancelArchiveWorktreeGone(repoID, instance, origPath, adopted, fmt.Errorf("cannot archive session %q because its post-worktree hooks could not be confirmed stopped; no session teardown was started: %w", title, err))
		}
	}

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
	// its dest, and runs in it. A deletion lands nothing anywhere, so it stays
	// empty there rather than naming a directory that will never exist, and the
	// hook runs in a scratch directory (see onArchiveHookContext.worktreeGone).
	archivePath := ""
	if adopted {
		archivePath = origPath
	}
	hookErr, err := archiveGoneTeardown(instance, func() error {
		return runOnArchiveHook(onArchiveHookContext{
			sessionID:    instance.ID,
			title:        title,
			repoRoot:     instance.GetRepoPath(),
			worktree:     origPath,
			archivePath:  archivePath,
			worktreeGone: !adopted,
		})
	}, true, adopted)
	if err != nil {
		// Same recovery as the relocating route's teardown failure: drop to Lost
		// with started kept, fenced off the automatic restore loop (#7752 — the
		// user asked for this agent to stop). An explicit restore re-spawns it
		// in the adopted worktree where it now is, or, for a deletion, in one
		// its rebuild recreates from the branch; retrying the archive finishes
		// the job.
		if adopted && errors.Is(err, os.ErrNotExist) {
			// The directory git proved vanished during teardown. Record that
			// durably, exactly as a deletion outside af is recorded: the sticky
			// flag is what keeps a directory recreated at this path later from
			// being accepted as the session's by respawn, archive or rename —
			// the stall the teardown installed settles once the path is absent.
			instance.SetWorktreeMissing(session.WorktreeMissingDeletedReason(origPath))
		}
		m.abortRequestedArchiveToLost(instance, err)
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
			"failed to archive session %q (%s): %w", title, archiveAbortedDetail, err), hookErr))
	}

	_ = instance.Transition(session.CommitArchive())
	if !adopted {
		instance.SetWorktreeMissing(archivedReason)
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
		m.info().Printf("archived session %q (repo %s): tmux torn down; worktree was already absent at %s (deleted outside af); %s", title, repoID, origPath, branchOutcome)
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

// refuseUnverifiedArchiveSource refuses an archive of a worktree-missing row
// whose claim found a directory git does not prove is the session's (#5102).
// The claim goes back unverified, so the refused directory is never recorded as
// the worktree's identity. The durable fence is the persisted missing flag
// itself (and its rollback projection, see ForStorage): it is what keeps every
// later archive, restore and respawn refusing the directory, so the refusal is
// reported together with any failure to write it rather than left to a
// best-effort persist that could lose it across a daemon exit.
func (m *Manager) refuseUnverifiedArchiveSource(repoID, title string, instance *session.Instance, claim sessiongit.RelocationClaim, cause error) error {
	_ = instance.Transition(session.CancelArchive())
	instance.ReturnWorktreeClaimUnverified(claim)
	refusal := fmt.Errorf(
		"cannot archive session %q: its tracked worktree %s was recorded as deleted outside af, and the directory now at that path is not one git proves is the session's checkout (%v) — af will not run the on-archive hook in it or move it into the archive; inspect %s, remove it or move it aside if it is not the session's work, then retry (the session then archives as deleted)",
		title, claim.Path, cause, claim.Path)
	if perr := m.persistInstanceErr(repoID, instance); perr != nil {
		return errors.Join(refusal, fmt.Errorf("could not persist the session's worktree-missing fence: %w", perr))
	}
	return refusal
}

// cancelArchiveWorktreeGone settles a pre-teardown cancellation of the gone
// route (#5102). Both arms have already settled the missing flag to what the
// route proved, so this persist is what carries that to disk: it takes the
// error-returning persist, and a write that did not land is reported with the
// cancellation instead of swallowed — a lost stamp is the fence a restart
// would no longer have.
//
// The adopted arm persists a path it re-aimed on git's proof, with the flag
// cleared. That proof is bound to the directory identity AdoptLandedArchiveMove
// pinned, and the editor and hook teardown ran since, so the pin is reconfirmed
// before the path is written as authoritative. A mismatch has fenced the
// record (ReconfirmAdoptedWorktree); a directory that vanished is recorded as
// missing again. Either way the fence is what is persisted, not the path.
func (m *Manager) cancelArchiveWorktreeGone(repoID string, instance *session.Instance, origPath string, adopted bool, cause error) error {
	_ = instance.Transition(session.CancelArchive())
	if adopted {
		if rerr := instance.ReconfirmPlacedWorktree(); rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				instance.SetWorktreeMissing(session.WorktreeMissingDeletedReason(origPath))
			}
			cause = fmt.Errorf("%w; the adopted worktree at %s is also no longer the directory git proved, so its record is fenced and af will not start an agent in or remove it: %v", cause, origPath, rerr)
		}
	}
	if perr := m.persistInstanceErr(repoID, instance); perr != nil {
		return errors.Join(cause, fmt.Errorf("could not record the session's worktree state on disk: %w", perr))
	}
	return cause
}
