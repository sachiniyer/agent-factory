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
// tri-state probe (probeRestoredTabSession, ProbeSessionForRestore in production: an unlinked
// socket under a possibly-live server is unknown, not absent, #6254) and
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
		stampTabExit(i, tab.ID, tab.tmux)
	}
	if err := tab.tmux.ReattachOnly(worktreePath, true); err != nil {
		log.WarningLog.Printf("reattach process tab %q for %q failed: %v", tab.Name, i.Title, err)
	}
}

// stampTabExit records a tab's exit when its pane's process is observed
// finished — a process tab's command, or a shell tab's $SHELL (#7944) — and
// reports whether it did. A stamp is durable evidence discovered
// while restoring, so it also enrolls the row for the daemon's load checkpoint:
// nothing else writes a row whose runtime was not replaced, and a reboot before
// any later mutation would lose the pane and the only record of how its command
// ended (#4506 review).
func stampTabExit(i *Instance, tabID string, tabTmux *tmux.TmuxSession) bool {
	dead, status, statusKnown, at, known := tabTmux.ProbePaneExit()
	if !known || !dead {
		return false
	}
	return recordTabExit(i, tabID, tabTmux, &TabExit{Status: status, StatusKnown: statusKnown, At: at})
}

// recordTabExit lands a probed exit on the tab's current copy — the update half
// of stampTabExit, separated so RecordTabExits can share one probe's answer
// across the bail check and the stamp without probing twice.
func recordTabExit(i *Instance, tabID string, tabTmux *tmux.TmuxSession, exit *TabExit) bool {
	// Fenced on the probed incarnation (#5960): the probe ran against tabTmux
	// outside the lock, and the id can since have been closed and re-created for
	// a different command. The id alone cannot tell the two apart; the tmux
	// handle can. A row that already records an ending keeps it.
	stamped := false
	updateTabByID(i, tabID, func(copy *Tab) {
		if copy.tmux != tabTmux || copy.Exit != nil {
			return
		}
		copy.Exit = exit
		stamped = true
		i.touchLocked()
	})
	if stamped {
		i.markLoadRuntimeReplaced()
	}
	return stamped
}

// clearTabExit drops a recorded exit when a fresh runtime replaces the one it
// describes (#7944): the stamp belonged to the OLD incarnation's end — a shell
// killed by tmux-server loss whose replacement just launched — and keeping it
// would read the live shell as dead on every surface forever.
func clearTabExit(i *Instance, tabID string) {
	updateTabByID(i, tabID, func(copy *Tab) {
		if copy.Exit == nil {
			return
		}
		copy.Exit = nil
		i.touchLocked()
	})
}

// exitProbe is one tab worth probing for exit evidence: its stable id (the
// updateTabByID key) and the tmux handle captured under i.mu, so the probe
// outside the lock races neither a tab close nor a rebinding.
type exitProbe struct {
	id   string
	kind TabKind
	tmux *tmux.TmuxSession
}

// probeTabLiveness is the exit-evidence probe for a shell tab (probeLiveness,
// #4135): the server answered AND the session is gone — and only that —
// proves the shell died. A timeout, an unlinked socket under a possibly-live
// server, EACCES, or any other non-answer is unknown, never dead: absence
// must be PROVEN, never assumed (#7944). A package var so tests can inject
// the answer matrix.
var probeTabLiveness = func(session *tmux.TmuxSession) (bool, bool) {
	return session.ProbeSessionLiveness()
}

// stampShellTabExit records a shell tab's exit when its tmux session is
// provably gone. A shell is never held by remain-on-exit — restore replaces a
// missing shell outright (#991), so there is no dead pane to read a status
// from: the stamp is "the shell ended", status unknown, at probe time. The
// recordTabExit incarnation fence applies unchanged: a concurrent respawn
// that swapped the handle makes the stamp skip rather than land on the
// replacement.
func stampShellTabExit(i *Instance, tabID string, tabTmux *tmux.TmuxSession) bool {
	exists, known := probeTabLiveness(tabTmux)
	if !known || exists {
		return false
	}
	return recordTabExit(i, tabID, tabTmux, &TabExit{At: time.Now()})
}

// KickShellExitProbe starts the local shell-exit refresh on its own goroutine
// and returns at once — the snapshot projection path calls it on every read,
// so it must never wait on tmux (#7944): projection latency cannot depend on
// tmux responsiveness. Once in flight per instance: a death found this pass
// is stamped for the NEXT read.
func (i *Instance) KickShellExitProbe() {
	if !i.shellExitProbeRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer i.shellExitProbeRunning.Store(false)
		i.probeShellExits()
	}()
}

// probeShellExits is KickShellExitProbe's synchronous body — the same
// proven-absence evidence RecordTabExits gathers for a shell tab, probed on
// the background pass. Tests call it directly rather than racing the goroutine.
func (i *Instance) probeShellExits() {
	i.mu.RLock()
	var probes []exitProbe
	for _, tab := range i.Tabs {
		if tab.Kind != TabKindShell || tab.Exit != nil || tab.tmux == nil {
			continue
		}
		probes = append(probes, exitProbe{id: tab.ID, tmux: tab.tmux})
	}
	i.mu.RUnlock()
	for _, p := range probes {
		stampShellTabExit(i, p.id, p.tmux)
	}
}

// RecordTabExits probes every PTY-bearing tab (process, and since #7944 shell)
// with no recorded exit and stamps the ones whose end tmux can prove
// (#5571 W4.3). It is the roster-read counterpart of stampTabExit's
// restore path: exit evidence is only discovered when something looks, and on
// a LIVE instance nothing looks — so a tab whose process exits while the
// agent-server runs would report an un-stamped Exit on /v1/agent/tabs forever. The roster handler runs this
// first. The evidence differs by kind: a process tab's held dead pane answers
// pane_dead/status (#4479); a shell's session is gone outright and only a
// probeLiveness-proven absence counts (unproven absence would fabricate the
// record this field exists to keep honest). Inert or tmux-less rows are
// skipped rather than probed (restoreProcessTab's rule). The eligible rows
// AND their tmux handles are captured under i.mu: the kill path nils tab.tmux
// under the same lock (clearClosedTmuxRefs), so reading the handle unlocked
// races it and could hand a probe a nil receiver; the captured handle stays
// valid past a concurrent kill — its probe just reports the now-gone session
// as such — and the ID-based update lands on the row's current copy or skips
// it when the row already records an ending.
func (i *Instance) RecordTabExits() {
	i.mu.RLock()
	var probes []exitProbe
	for _, tab := range i.Tabs {
		if (tab.Kind != TabKindProcess && tab.Kind != TabKindShell) || tab.Exit != nil || tab.inert || tab.tmux == nil {
			continue
		}
		probes = append(probes, exitProbe{id: tab.ID, kind: tab.Kind, tmux: tab.tmux})
	}
	i.mu.RUnlock()
	// A shared wall-clock budget bounds the pass: a wedged tmux makes each
	// probe pay its full bounded wait (~10s). The check is time spent, not
	// answer kind — a "can't find session" answer is fast and free — so one
	// stalled probe spends the budget and the pass ends after it instead of
	// multiplying the stall by tab count.
	start := time.Now()
	for _, p := range probes {
		if time.Since(start) > recordExitsBudget {
			break
		}
		if p.kind == TabKindShell {
			stampShellTabExit(i, p.id, p.tmux)
			continue
		}
		dead, status, statusKnown, at, known := p.tmux.ProbePaneExit()
		if !known {
			continue
		}
		if dead {
			recordTabExit(i, p.id, p.tmux, &TabExit{Status: status, StatusKnown: statusKnown, At: at})
		}
	}
}

// recordExitsBudget is the shared wall-clock limit for one RecordTabExits
// pass — generous for a healthy server (a probe answers in milliseconds even
// over many tabs), and it only ever truncates the pass when tmux is answering
// so slowly that the remaining probes would pay the same wait.
const recordExitsBudget = 4 * time.Second

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
		i.markLoadRuntimeReplaced()
	}
}

// keepFinishedProcessPane records a process tab's exit if its command has
// finished, and reports whether the pane can be kept rather than stopped:
// the command finished and nothing it started is still running, so the pane
// carries no process on any identity and its output stays readable. A stop
// would destroy that output and the exit status for nothing (#4506 review).
func keepFinishedProcessPane(i *Instance, tab *Tab) bool {
	if tab.Exit == nil {
		stampTabExit(i, tab.ID, tab.tmux)
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
