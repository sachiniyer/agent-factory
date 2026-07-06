package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Session teardown core (#1195 Phase 2, audit item #5).
//
// Every kill/archive/discard path used to open-code the same "snapshot each
// tab's tmux under i.mu → close it OUTSIDE the lock → re-lock and clear the
// refs" skeleton, each with its own local tabSession struct and four silent
// divergences (worktree handling, the #802 pane-exit-before-worktree ordering,
// the started fence, and the ref-clear identity guard). teardownTabs collapses
// them into ONE parameterized core so a fix to one reaches all: the mode decides
// the tmux verb, the worktree action, and the tab retention, and nothing else
// forks.
//
// Phase 2a introduces the KILL and RELEASE-PTY modes and migrates
// LocalBackend.Kill / LocalBackend.CloseAttachOnly. Phase 2b adds the ARCHIVE
// mode (kill-session + wait, then MoveWorktree folded into the core immediately
// after the pane-exit wait — closing the #802 duplication) and migrates
// Instance.ArchiveTeardown.

// teardownMode selects teardownTabs' behavior. It is a small sealed set — the
// three physical teardown shapes — rather than a bag of booleans, so a caller
// picks an intent and the core owns the mechanics.
type teardownMode interface {
	isTeardownMode()
}

// teardownKill tears the session fully down: kill-session on every tab (waiting
// for each pane to exit, #802), DELETE the worktree, clear every tmux ref and
// the worktree pointer. started is cleared. Best-effort — a stuck tmux or a
// failed worktree cleanup only logs, so the caller can still drop the record
// (#478).
type teardownKill struct{}

func (teardownKill) isTeardownMode() {}

// teardownReleasePTY releases only this instance's hold on its tmux sessions —
// the attach PTYs and `tmux attach-session` children — WITHOUT running
// kill-session. The server-side tmux sessions and the worktree are left intact.
// Used to discard a duplicate Instance built from disk that lost a race to the
// canonical tracked one (#867/#1065): it must surrender every tab's PTY without
// tearing down the live sessions the canonical Instance shares. Per-tab close
// errors are collected and returned (not merely logged).
type teardownReleasePTY struct{}

func (teardownReleasePTY) isTeardownMode() {}

// teardownTabs runs the one teardown skeleton for the given mode. It snapshots
// each tab's tmux session under i.mu, tears them down OUTSIDE the lock (closing
// under i.mu would stall every reader while a pane drains), performs the mode's
// worktree action while no pane is cwd'd in the worktree, then re-locks and
// clears the tmux refs. Clearing is identity-guarded (only a ref that is still
// the session we closed is cleared) so a concurrent Start that swapped in a
// fresh session is never nil'd out from under it.
func (i *Instance) teardownTabs(mode teardownMode) error {
	type tabSession struct {
		tab *Tab
		ts  *tmux.TmuxSession
	}
	i.mu.Lock()
	sessions := make([]tabSession, 0, len(i.Tabs))
	for _, tab := range i.Tabs {
		if tab.tmux != nil {
			sessions = append(sessions, tabSession{tab: tab, ts: tab.tmux})
		}
	}
	gw := i.gitWorktree
	title := i.Title
	i.started = false
	i.mu.Unlock()

	// Tear down every tab's tmux session. For the kill mode this waits for each
	// pane's process to actually exit BEFORE the worktree cleanup below: a
	// process still flushing state mid-shutdown races git's recursive delete and
	// leaks a half-deleted directory ("Directory not empty", #802). The release-
	// PTY mode only drops the local attach and never touches the worktree, so it
	// needs no wait and surfaces its per-tab errors to the caller.
	var errs []error
	for _, s := range sessions {
		switch mode.(type) {
		case teardownReleasePTY:
			if err := s.ts.CloseAttachOnly(); err != nil {
				errs = append(errs, fmt.Errorf("tab %q: %w", s.tab.Name, err))
			}
		default: // teardownKill
			if err := s.ts.CloseAndWaitForPaneExit(); err != nil {
				log.WarningLog.Printf("kill %q: tmux cleanup for tab %q failed: %v", title, s.tab.Name, err)
			}
		}
	}

	// Worktree action, once every pane has exited.
	if _, ok := mode.(teardownKill); ok && gw != nil {
		if err := gw.Cleanup(); err != nil {
			log.WarningLog.Printf("kill %q: git worktree cleanup failed: %v", title, err)
		}
	}

	i.mu.Lock()
	for _, s := range sessions {
		// Only clear a ref that is still the session we closed: a concurrent
		// Start may have swapped in a fresh session while the lock was released.
		if s.tab.tmux == s.ts {
			s.tab.tmux = nil
		}
	}
	if _, ok := mode.(teardownKill); ok && i.gitWorktree == gw {
		i.gitWorktree = nil
	}
	i.mu.Unlock()

	return errors.Join(errs...)
}
