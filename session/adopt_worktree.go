package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
)

// adoptExistingWorktree gives a restore-time instance the worktree handle a
// first-time setup would have created (#5571 W1.4). The headless agent-server
// builds a fresh Instance each time it starts, so after a restart its
// gitWorktree is nil and a restore (firstTimeSetup=false) never populates it:
// archive, push and agent swap would fail with "no worktree", and a tmux session
// that also died could not be respawned.
//
// Only a hosted instance takes this path; every other restore already carries
// its persisted handle. The workspace must be proven consistent BEFORE anything
// is adopted onto it, on the same proof model as the rest of recovery: the
// handle is derived exactly as at create, then
//   - git has the recorded branch checked out in a worktree that is present
//     → adopted at THAT path once git's registration and the directory's
//     back-binding prove it is this session's own (VerifyLandedMove);
//   - the worktree is conclusively absent (the one answer that licenses
//     replacement) → rebuilt from the recorded branch, which RebuildFromExistingBranch
//     never creates fresh, so a branch that is gone refuses instead of silently
//     starting empty work;
//   - anything else (present on another branch, an unreadable branch, a stat
//     that cannot answer) → refused, because uncertainty keeps the work.
func (i *Instance) adoptExistingWorktree() error {
	i.mu.RLock()
	hosted, have, inPlace := i.hostedRemotely, i.gitWorktree != nil, i.inPlace
	i.mu.RUnlock()
	if !hosted || have {
		return nil
	}
	gw, branch, err := i.newWorktreeHandle()
	if err != nil {
		return fmt.Errorf("cannot adopt the existing workspace: %w", err)
	}
	if !inPlace {
		// The worktree is wherever git has the session's branch, not wherever
		// placement would put a new one: the derivation avoids an occupied path.
		registered, listed, rerr := gw.RegisteredPathForBranch()
		if rerr != nil {
			return fmt.Errorf("cannot adopt the existing workspace: git could not list its worktrees: %w", rerr)
		}
		if listed {
			if gw, err = git.NewGitWorktreeFromStorage(gw.GetRepoPath(), registered, i.Title, branch, "", false, false); err != nil {
				return fmt.Errorf("cannot adopt the worktree at %s: %w", registered, err)
			}
		} else {
			// "Git lists no worktree on this branch" is an absence, and an absence
			// proves nothing about THIS session's workspace: the checkout may have
			// been switched to another branch (its work still on disk), or its
			// registration pruned. The handle newWorktreeHandle derived is also not
			// the original path (it avoids an occupied one), so probing it would read
			// "absent" for a workspace that is present elsewhere. Rebuilding on that
			// would stop the surviving agent and start empty work. Refuse, and leave
			// everything alone: the session needs attention, not a rebuild.
			return fmt.Errorf("cannot adopt the existing workspace: git lists no worktree on branch %q, "+
				"so whether this session's checkout is gone or merely on another branch is not established; "+
				"leaving it untouched", branch)
		}
	}
	switch gw.ProbeWorktreePresence() {
	case git.WorktreePresencePresent:
		if !inPlace {
			// git's registration plus the occupant's back-binding, the evidence kill
			// and archive trust, never the mere existence of a directory.
			if verr := gw.VerifyLandedMove(gw.GetWorktreePath()); verr != nil {
				return fmt.Errorf("cannot adopt the worktree at %s: %w", gw.GetWorktreePath(), verr)
			}
		}
	case git.WorktreePresenceAbsent:
		if inPlace {
			return fmt.Errorf("cannot adopt the in-place workspace: %s is gone", gw.GetWorktreePath())
		}
		// The daemon's durable row records the session's tabs even though this fresh
		// instance holds only its agent tab. A recorded sibling shell or process tab
		// has no proof it stopped (and its writes by absolute path from another
		// cwd are invisible to the occupancy check below), so the rebuild refuses
		// and the session needs attention. #6162 restores the roster.
		i.mu.RLock()
		siblings := i.persistedSiblingTabs
		i.mu.RUnlock()
		if siblings > 0 {
			return fmt.Errorf("the worktree at %s is gone, but the session records %d other tab(s) that cannot be proven stopped; "+
				"not rebuilding under them", gw.GetWorktreePath(), siblings)
		}
		// Anything that outlived the deleted worktree is a stale writer: a pane keeps
		// a cwd inode that is gone, and anything it writes by absolute path lands
		// while git recreates the checkout. The stale-pane teardown every other
		// rebuild runs does the whole job — arms the durable obligation first,
		// closes every bound session and WAITS for its process tree, stops the
		// supervised runtimes, and, when a close was blind (tmux forgot the
		// ancestry, so the marker sweep is vacuous for a markerless survivor),
		// refuses while anything still has a cwd in the tree. An unproven teardown
		// refuses the rebuild and leaves the session needing attention; nothing
		// is rebuilt under a possible writer.
		committed := i.RebuildTeardownPending()
		if err := i.SetRebuildTeardownPending(true); err != nil {
			return fmt.Errorf("the worktree at %s is gone, but the stale-pane teardown could not be made durable before rebuilding: %w", gw.GetWorktreePath(), err)
		}
		if err := i.teardownStalePanesForRebuild(gw.GetWorktreePath(), committed, true); err != nil {
			return fmt.Errorf("the worktree at %s is gone, but what outlived it is not proven stopped; not rebuilding: %w", gw.GetWorktreePath(), err)
		}
		// The teardown above only knows the tabs THIS instance holds, and a restarted
		// agent-server's fresh instance holds just its agent tab: surviving sibling
		// shell and process tabs are not enumerated, and the occupancy check inside
		// the teardown runs only after a BLIND close. So the proof that nothing of
		// this session can still write into the tree must not depend on the roster:
		// look at the tree itself, unconditionally, and refuse while anything has a
		// working directory in it (reported, never killed: it could be the
		// operator's own shell). The check also refuses when the process table
		// cannot be read.
		if err := CheckWorktreeOccupants(gw.GetWorktreePath()); err != nil {
			return fmt.Errorf("the worktree at %s is gone, but what outlived it is not proven stopped; not rebuilding: %w", gw.GetWorktreePath(), err)
		}
		if err := gw.RebuildFromExistingBranch(); err != nil {
			return fmt.Errorf("the worktree at %s is gone and could not be rebuilt from branch %q: %w", gw.GetWorktreePath(), branch, err)
		}
	default:
		return fmt.Errorf("cannot adopt the worktree at %s: whether it is present could not be established", gw.GetWorktreePath())
	}
	i.mu.Lock()
	i.setGitWorktreeLocked(gw)
	if i.Branch != branch {
		i.Branch = branch
		i.touchLocked()
	}
	i.mu.Unlock()
	return nil
}
