package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
)

// Restoring the surviving tabs of a restarted agent-server (W1.4, #6162).
//
// A crashed agent-server's shell and process tabs keep their tmux sessions, but
// the replacement builds a fresh instance holding only the adopted agent tab, so
// it cannot resolve their ids: streaming answers ErrTabGone and a close cannot
// stop the tmux process. The daemon alone knows the ids, gens and names; the
// sandbox alone knows whether a session still exists. So the daemon sends its
// roster with the adoption's Launch and the server rebinds each row to the tmux
// session the sandbox itself reported at create.
//
// Nothing is executed. A row is rebound only to a session tmux CONFIRMS exists
// (attach-only: Restore with no work dir cannot spawn), a session confirmed gone
// is left off the roster, and one tmux did not answer for is left unresolved. A
// command is carried as display metadata only.

const tabRestoreFeature = "tab-restore"

// TabRestoreFeature is exported for the agent-server's /v1/health advertisement.
const TabRestoreFeature = tabRestoreFeature

// TabRestoreReport says what RestoreSurvivingTabs did with each id.
type TabRestoreReport struct {
	Restored   []string // rebound to a live tmux session
	Gone       []string // tmux confirmed the session absent; not recreated
	Unresolved []string // no usable record, or tmux gave no answer
}

// restoreRosterLocked is the roster a restarted server is handed: the off-box
// shell/process rows with the sandbox's own tmux name, plus the ids that have
// none (created before the daemon recorded it) and so cannot be rebound. Caller
// holds i.mu.
func (i *Instance) restoreRosterLocked() (rows []TabData, unbound []*Tab) {
	for idx, tab := range i.Tabs {
		if idx == 0 || !i.isOffBoxTabLocked(tab) {
			continue
		}
		if tab.sandboxTmux == "" {
			unbound = append(unbound, tab)
			continue
		}
		rows = append(rows, TabData{ID: tab.ID, Gen: tab.gen, Name: tab.Name, Kind: tab.Kind, Command: tab.Command, TmuxName: tab.sandboxTmux})
	}
	return rows, unbound
}

// restoreRosterForRestart returns the rows to send, and flags every off-box tab
// the restarted server cannot be given back as unavailable. withFeature is
// whether the server advertises tab-restore: without it nothing is sent and
// every off-box tab is flagged, never given a binding the server cannot honour.
func (i *Instance) restoreRosterForRestart(withFeature bool) []TabData {
	i.mu.Lock()
	defer i.mu.Unlock()
	rows, unbound := i.restoreRosterLocked()
	flag := func(t *Tab) {
		if !t.restoreUnavailable {
			log.WarningLog.Printf("session %q: restarted agent-server cannot be given back %s tab %q (%s); it is shown unavailable and left running",
				i.Title, TabKindName(t.Kind), t.Name, t.ID)
		}
		t.restoreUnavailable = true
	}
	if !withFeature {
		for idx, t := range i.Tabs {
			if idx > 0 && i.isOffBoxTabLocked(t) {
				flag(t)
			}
		}
		return nil
	}
	for _, t := range unbound {
		flag(t)
	}
	return rows
}

// markRestored lifts the flag on ids the server now holds.
func (i *Instance) markRestored(ids []string) {
	if len(ids) == 0 {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, t := range i.Tabs {
		for _, id := range ids {
			if t.ID == id {
				t.restoreUnavailable = false
			}
		}
	}
}

// RestoreSurvivingTabs rebinds the rows to their surviving tmux sessions on a
// restarted agent-server's fresh instance. See the file comment.
func (i *Instance) RestoreSurvivingTabs(rows []TabData) TabRestoreReport {
	var rep TabRestoreReport
	i.mu.RLock()
	agentTmux := i.tmuxLocked()
	started := i.started
	i.mu.RUnlock()
	for _, row := range rows {
		if row.ID == "" || row.TmuxName == "" || agentTmux == nil || !started ||
			(row.Kind != TabKindShell && row.Kind != TabKindProcess) {
			rep.Unresolved = append(rep.Unresolved, row.ID)
			continue
		}
		i.mu.RLock()
		_, have := i.tabIndexByIDLocked(row.ID)
		i.mu.RUnlock()
		if have {
			continue
		}
		ts := agentTmux.NewSiblingSession(row.TmuxName, defaultShell())
		exists, known, err := ts.ProbeSessionStrict()
		switch {
		case err != nil || !known:
			rep.Unresolved = append(rep.Unresolved, row.ID)
			continue
		case !exists:
			rep.Gone = append(rep.Gone, row.ID)
			continue
		}
		// Attach-only: no work dir, so a session that vanished since the probe is
		// an error here, never a respawn.
		if err := ts.Restore(""); err != nil {
			log.WarningLog.Printf("restore tab %q: %v", row.ID, fmt.Errorf("rebind %s: %w", row.TmuxName, err))
			rep.Unresolved = append(rep.Unresolved, row.ID)
			continue
		}
		if row.Kind == TabKindProcess {
			ts.SetRemainOnExit()
		}
		i.mu.Lock()
		if _, dup := i.tabIndexByIDLocked(row.ID); !dup && i.started {
			i.Tabs = append(i.Tabs, &Tab{ID: row.ID, Name: row.Name, Kind: row.Kind, Command: row.Command, gen: row.Gen, tmux: ts})
			i.touchLocked()
			rep.Restored = append(rep.Restored, row.ID)
		}
		i.mu.Unlock()
	}
	return rep
}

// applyRestoreOutcome records the server's answer to a sent roster. A restored
// id is lifted out of the unavailable state; a gone id is left for the roster
// sweep to retire, because the server read an empty answer for it; any other id
// stays unavailable rather than being guessed either way.
func (i *Instance) applyRestoreOutcome(sent []TabData, restored, gone []string) {
	answered := map[string]bool{}
	for _, id := range restored {
		answered[id] = true
	}
	for _, id := range gone {
		answered[id] = true
	}
	i.markRestored(restored)
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, row := range sent {
		if answered[row.ID] {
			continue
		}
		if idx, ok := i.tabIndexByIDLocked(row.ID); ok {
			i.Tabs[idx].restoreUnavailable = true
		}
	}
}
