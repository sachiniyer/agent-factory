package session

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Process-tab lifecycle outside tab-create (#4479, #4506 review). A process
// tab's command runs once, so every path that meets one after creation either
// reattaches, records how the command ended, or stops it and records why. None
// of them re-runs it.

// restoreProcessTab reconnects a persisted process tab WITHOUT re-executing its
// command (#4479): a process command runs once, at tab-create, so the
// definitive-absence respawn every other tmux-backed kind relies on would
// re-fire deploys and migrations on every af restart. Its observable states map
// to:
//
//   - session live, command finished (a held dead pane): stamp Tab.Exit with
//     the status/time the pane reported, then rebind so its retained output
//     stays previewable.
//   - session live, command running: rebind, after healing remain-on-exit for
//     a tab persisted before #4479 so its eventual exit is observable too.
//   - session definitively absent: the pane is gone entirely (tmux-server
//     loss, or a stop). The tab is left inert — nothing respawns, and nothing
//     is stamped, because absence is not evidence of how the command ended.
//   - probe unanswered (a wedged has-session): neither of the above, and
//     treated as neither. Absence is not proven, so the tab is not made inert
//     and its scope flag stands. Liveness is not proven either, so the rebind
//     claims no answer (the monitor keeps its outgoing generation, #4473), and
//     the remain-on-exit heal and exit stamp wait for a restore that gets an
//     answer — each is another tmux command that would pay the same deadline
//     for the same non-answer.
//
// This is a positive gate in ExistsOrUnknown's sense, so it takes the
// tri-state probe (probeRestoredTabSession, ProbeSession in production) and
// handles !known itself (#1917/#1962). No state re-executes the command,
// which is the only outcome this path must exclude. All failures here are
// warnings — a process tab's failure mode is inert, and aborting setupTabs over
// one would strand the tabs behind it for nothing.
func restoreProcessTab(i *Instance, tab *Tab, worktreePath string) {
	exists, known := probeRestoredTabSession(tab.tmux)
	if !known {
		log.WarningLog.Printf("tmux did not answer for process tab %q of %q; rebinding without healing or stamping it", tab.Name, i.Title)
		if err := tab.tmux.ReattachOnly(worktreePath, false); err != nil {
			log.WarningLog.Printf("reattach process tab %q for %q failed: %v", tab.Name, i.Title, err)
		}
		return
	}
	if !exists {
		// Definitive absence also resolves an unknown-scope flag: whatever the
		// pane was running as, it is gone — whether the scope stop killed it or
		// the tmux server lost it. Clearing keeps the next restore from
		// re-entering the stop path for a pane that cannot exist.
		updateTabByID(i, tab.ID, func(copy *Tab) {
			copy.accountScopeProvenanceUnknown = false
			copy.inert = true
		})
		return
	}
	// Best-effort heal for a still-running pre-#4479 pane: with the option on,
	// tmux holds the pane when the command exits and pane_dead records it.
	tab.tmux.ApplyRemainOnExit()
	if tab.Exit == nil {
		stampProcessTabExit(i, tab)
	}
	if err := tab.tmux.ReattachOnly(worktreePath, true); err != nil {
		log.WarningLog.Printf("reattach process tab %q for %q failed: %v", tab.Name, i.Title, err)
	}
}

// stampProcessTabExit records a process tab's exit when its command is observed
// finished, and reports whether it did. A stamp is durable evidence discovered
// while restoring, so it also enrolls the row for the daemon's load checkpoint:
// nothing else writes a row whose runtime was not replaced, and a reboot before
// any later mutation would lose the pane and the only record of how its command
// ended (#4506 review).
func stampProcessTabExit(i *Instance, tab *Tab) bool {
	dead, status, statusKnown, at, known := tab.tmux.ProbePaneExit()
	if !known || !dead {
		return false
	}
	exit := &TabExit{Status: status, StatusKnown: statusKnown, At: at}
	stamped := updateTabByID(i, tab.ID, func(copy *Tab) {
		copy.Exit = exit
		i.touchLocked()
	})
	if stamped {
		i.markLoadRuntimeReplaced(false)
	}
	return stamped
}

// stampProcessTabStopped records that af stopped a process tab's running
// command, and why, unless the row already records how the command ended. The
// tab stays inert afterwards, and without this the row could not say why.
func stampProcessTabStopped(i *Instance, tab *Tab, reason string) {
	exit := &TabExit{At: time.Now(), StoppedBy: reason}
	stamped := false
	updateTabByID(i, tab.ID, func(copy *Tab) {
		if copy.Exit == nil {
			copy.Exit = exit
			i.touchLocked()
			stamped = true
		}
	})
	if stamped {
		i.markLoadRuntimeReplaced(false)
	}
}

// keepFinishedProcessPane records a process tab's exit if its command has
// finished, and reports whether the pane can be kept rather than stopped:
// the command finished and nothing it started is still running, so the pane
// carries no process on any identity and its output stays readable. A stop
// would destroy that output and the exit status for nothing (#4506 review).
func keepFinishedProcessPane(i *Instance, tab *Tab) bool {
	if tab.Exit == nil {
		stampProcessTabExit(i, tab)
	}
	return tab.tmux.FinishedAndQuiet()
}

// stopPreScopeProcessTab is the load-time account-scope stop for a process tab
// whose pane af did not record launching under the session's account. A
// running command is stopped and the row says why; a finished one with nothing
// left running is kept. Either way the tab is never re-run.
func stopPreScopeProcessTab(i *Instance, tab *Tab) error {
	if keepFinishedProcessPane(i, tab) {
		return nil
	}
	if _, err := tab.tmux.CloseAndWaitForPaneExit(); err != nil {
		return fmt.Errorf("restore account-scoped tab %q for %q: stop the pre-scope process: %w", tab.Name, i.Title, err)
	}
	stampProcessTabStopped(i, tab, TabStoppedByAccountScope)
	return nil
}

// recordSiblingLaunch records that af just launched the pane of the sibling
// with this id under the account ts carries, which also settles any
// unknown-scope flag.
func recordSiblingLaunch(i *Instance, id string, ts *tmux.TmuxSession) {
	scope := ts.Account()
	updateTabByID(i, id, func(copy *Tab) {
		if copy.accountScope != scope {
			copy.accountScope = scope
			i.touchLocked()
		}
		copy.accountScopeProvenanceUnknown = false
	})
}

// updateTabByID applies f to the current copy of the tab with this id, under the
// instance lock, and reports whether the tab was found. It re-finds the tab by
// id because replaceTabFieldLocked copies on write, so a pointer captured before
// an unlocked window may be stale. f runs with i.mu held and calls touchLocked
// itself when its change is one to persist.
func updateTabByID(i *Instance, id string, f func(*Tab)) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for idx, current := range i.Tabs {
		if current.ID == id {
			i.replaceTabFieldLocked(idx, f)
			return true
		}
	}
	return false
}
