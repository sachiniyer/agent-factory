package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
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
		// An agent that outlived its deleted worktree is a stale writer: it keeps
		// a cwd inode that is gone, and anything it writes by absolute path lands
		// while git recreates the checkout. Stop it, and confirm it is stopped,
		// BEFORE the rebuild — the same order every other rebuild keeps (see
		// respawnWithConversation). The directory is conclusively absent, so
		// there is no work in it to lose; launch re-spawns the agent in the new
		// checkout. An unconfirmed stop refuses: retry rather than rebuild under
		// a writer.
		if err := i.stopSurvivingAgentBeforeRebuild(); err != nil {
			return err
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

// stopSurvivingAgentBeforeRebuild stops the agent tmux session this instance is
// bound to (none on a first adoption) and returns an error unless the stop was
// confirmed by every tmux command answering.
func (i *Instance) stopSurvivingAgentBeforeRebuild() error {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return nil
	}
	state, err := ts.CloseAndWaitForPaneExit()
	if err != nil {
		return fmt.Errorf("the worktree is gone but the agent that outlived it could not be stopped before the rebuild: %w", err)
	}
	if state != tmux.PaneStateKnown {
		return fmt.Errorf("the worktree is gone but whether the agent that outlived it has stopped could not be established; not rebuilding under a possible writer")
	}
	return nil
}
