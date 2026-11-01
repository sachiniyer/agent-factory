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

// tabRestoreRetryFeature gates POST /v1/agent/restore-tabs: the same rebind
// against a running server, outside a launch.
const tabRestoreRetryFeature = "tab-restore-retry"

// TabRestoreRetryFeature is exported for the agent-server's /v1/health advertisement.
const TabRestoreRetryFeature = tabRestoreRetryFeature

// TabRestoreState is where an off-box tab stands after an agent-server restart.
// Every such tab is in exactly one state:
//
//	TabRestored (zero)  the server holds it; usable
//	TabRestoreGone      the sandbox confirmed its session absent; the sweep retires the row
//	TabRestoreUnknown   not yet answered: an old server, a probe that failed or has
//	                    not answered, or no recorded session. The row is kept, shown
//	                    unavailable, never retired, and retried when a server answers.
//
// Only Gone lets the roster sweep retire a row. The state is persisted, so a
// daemon restart does not turn an unknown tab into one the sweep deletes.
type TabRestoreState string

const (
	TabRestored       TabRestoreState = ""
	TabRestoreUnknown TabRestoreState = "unknown"
	TabRestoreGone    TabRestoreState = "gone"
)

// normalizeRestoreState reads a persisted value: anything but the two known
// debts is restored, except an unrecognised non-empty value, which is unknown
// (never trusted as bound).
func normalizeRestoreState(s TabRestoreState) TabRestoreState {
	switch s {
	case TabRestored, TabRestoreGone, TabRestoreUnknown:
		return s
	}
	return TabRestoreUnknown
}

// featureVerdict is a three-way advertisement answer: unlike a bool, it keeps a
// probe that has not answered apart from a server that lacks the feature.
type featureVerdict int

const (
	featureUnknown featureVerdict = iota
	featureAbsent
	featurePresent
)

// TabRestoreFeature is exported for the agent-server's /v1/health advertisement.
const TabRestoreFeature = tabRestoreFeature

// TabRestoreReport says what RestoreSurvivingTabs did with each id.
type TabRestoreReport struct {
	Restored   []string // rebound to a live tmux session
	Gone       []string // tmux confirmed the session absent; not recreated
	Unresolved []string // no usable record, or tmux gave no answer
}

// isRestorableRow is a staged off-box shell/process row carrying its sandbox
// session: the only PTY rows a reload keeps (see metadataTabsFrom).
func isRestorableRow(td TabData) bool {
	k := tabKindForData(td.Kind)
	return (k == TabKindShell || k == TabKindProcess) && td.SandboxTmux != "" && td.ID != ""
}

// materializeRestorableTabs moves the staged restorable rows onto the roster,
// behind the agent tab, as off-box tabs with no daemon-side binding. A reload
// stages them because the sandbox was not yet reached; the reattach is the one
// place that reaches it. Rows whose id is already on the roster are left alone,
// so a retried reattach adds nothing twice.
func (i *Instance) materializeRestorableTabs() {
	i.mu.Lock()
	defer i.mu.Unlock()
	var keep []TabData
	for _, td := range i.pendingMetadataTabs {
		if !isRestorableRow(td) {
			keep = append(keep, td)
			continue
		}
		if len(i.Tabs) == 0 {
			i.Tabs = []*Tab{newRemoteAgentTab()}
		}
		if _, have := i.tabIndexByIDLocked(td.ID); have {
			continue
		}
		i.Tabs = append(i.Tabs, &Tab{ID: td.ID, Name: td.Name, Kind: tabKindForData(td.Kind), Command: td.Command,
			Exit: tabExitFromData(td.Exit), gen: td.Gen, sandboxTmux: td.SandboxTmux, restoreState: normalizeRestoreState(td.RestoreState)})
		i.touchLocked()
	}
	i.pendingMetadataTabs = keep
}

// restoreRosterLocked is the roster a restarted server is handed: the off-box
// shell/process rows with the sandbox's own tmux name, plus the tabs that have
// none (created before the daemon recorded it) and so cannot be rebound. A tab
// already confirmed gone is in neither: it is never resurrected. Caller holds
// i.mu.
func (i *Instance) restoreRosterLocked() (rows []TabData, unbound []*Tab) {
	for idx, tab := range i.Tabs {
		if idx == 0 || !i.isOffBoxTabLocked(tab) || tab.restoreState == TabRestoreGone {
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

// restoreRosterForRestart is called as a restarted server is adopted. That
// server holds none of the daemon's off-box tabs, so every one not already
// confirmed gone becomes unknown — a durable debt — and the returned rows are
// the ones to send. Only the server's answer (applyRestoreOutcome) moves a tab
// out of unknown. verdict is the tab-restore advertisement: unless it is
// present nothing is sent and the debt simply stays, so a probe that has not
// answered is never mistaken for an old server (#6236) and neither is
// fabricated a binding.
func (i *Instance) restoreRosterForRestart(verdict featureVerdict) []TabData {
	i.mu.Lock()
	defer i.mu.Unlock()
	rows, _ := i.restoreRosterLocked()
	for idx, t := range i.Tabs {
		if idx == 0 || !i.isOffBoxTabLocked(t) || t.restoreState == TabRestoreGone {
			continue
		}
		if t.restoreState != TabRestoreUnknown {
			log.WarningLog.Printf("session %q: restarted agent-server does not hold %s tab %q (%s) yet; it is shown unavailable and left running",
				i.Title, TabKindName(t.Kind), t.Name, t.ID)
			t.restoreState = TabRestoreUnknown
			i.touchLocked()
		}
	}
	if verdict != featurePresent {
		return nil
	}
	return rows
}

// unknownRestoreRowsLocked is the roster of tabs still in the unknown state
// that carry a session name to rebind. Caller holds i.mu.
func (i *Instance) unknownRestoreRowsLocked() []TabData {
	var rows []TabData
	for idx, t := range i.Tabs {
		if idx == 0 || !i.isOffBoxTabLocked(t) || t.restoreState != TabRestoreUnknown || t.sandboxTmux == "" {
			continue
		}
		rows = append(rows, TabData{ID: t.ID, Gen: t.gen, Name: t.Name, Kind: t.Kind, Command: t.Command, TmuxName: t.sandboxTmux})
	}
	return rows
}

// retryUnknownTabRestores re-attempts the restore of every tab still unknown, on
// a server that is already running — the path a restart adoption's lost,
// unanswered or unsupported attempt leaves to (#6235, #6236). It reports whether
// any tab changed state. A server that does not advertise the retry route, or
// does not answer, leaves every tab unknown.
func (i *Instance) retryUnknownTabRestores(rs *remoteAgentServer) (changed bool) {
	i.mu.RLock()
	rows := i.unknownRestoreRowsLocked()
	i.mu.RUnlock()
	if len(rows) == 0 || rs.featureVerdict(tabRestoreRetryFeature) != featurePresent {
		return false
	}
	var resp agentLaunchResp
	if err := rs.rc.call("/v1/agent/restore-tabs", agentLifecycleReq{Tabs: rows}, &resp); err != nil {
		log.WarningLog.Printf("session %q: retrying the restore of %d sandbox tab(s) failed; they stay unavailable and are retried: %v", i.Title, len(rows), err)
		return false
	}
	return i.applyRestoreOutcome(rows, resp.TabsRestored, resp.TabsGone)
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
			// A retry after a lost launch response: the tab is already rebound on
			// this server. Report its true state rather than skip it, or the daemon
			// would read the silence as unavailable (#6229 review).
			i.reportRebound(&rep, row)
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

// sameTabIncarnation is the generation fence every application of an answer
// about a tab makes: an empty generation on either side cannot disprove the
// match, while two different ones mean the id was closed and re-created since
// the question was asked — so the answer is not about the tab now under it.
func sameTabIncarnation(rowGen, tabGen string) bool {
	return rowGen == "" || tabGen == "" || rowGen == tabGen
}

// applyRestoreOutcome records the server's answer to a sent roster: a restored
// id is bound (restored), a gone id is confirmed gone — the roster sweep then
// retires its row (#6237) — and any other id stays unknown, to be retried. It
// reports whether any tab changed state.
//
// The answer lands only on the incarnation the roster described: a tab closed
// and recreated under the same id while the request was in flight is a
// different one, and taking the dead generation's verdict would persist — a
// late tabs_gone would brand the live replacement gone for good (#6242).
func (i *Instance) applyRestoreOutcome(sent []TabData, restored, gone []string) (changed bool) {
	set := func(ids []string, to TabRestoreState) {
		for _, id := range ids {
			for _, row := range sent {
				if row.ID != id {
					continue
				}
				idx, ok := i.tabIndexByIDLocked(id)
				if !ok || !sameTabIncarnation(row.Gen, i.Tabs[idx].gen) {
					continue
				}
				if i.Tabs[idx].restoreState != to {
					i.Tabs[idx].restoreState = to
					changed = true
				}
			}
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	set(restored, TabRestored)
	set(gone, TabRestoreGone)
	if changed {
		i.touchLocked()
	}
	return changed
}

// reportRebound answers a row whose id this server already holds. The same
// incarnation on the same session is restored (or gone, if tmux now says so); a
// different incarnation or session under the id is unresolved, never claimed.
func (i *Instance) reportRebound(rep *TabRestoreReport, row TabData) {
	i.mu.RLock()
	idx, _ := i.tabIndexByIDLocked(row.ID)
	if idx >= len(i.Tabs) {
		i.mu.RUnlock()
		rep.Unresolved = append(rep.Unresolved, row.ID)
		return
	}
	tab := i.Tabs[idx]
	ts := tab.tmux
	same := ts != nil && ts.SanitizedName() == row.TmuxName && sameTabIncarnation(row.Gen, tab.gen)
	i.mu.RUnlock()
	if !same {
		rep.Unresolved = append(rep.Unresolved, row.ID)
		return
	}
	switch exists, known, err := ts.ProbeSessionStrict(); {
	case err != nil || !known:
		rep.Unresolved = append(rep.Unresolved, row.ID)
	case !exists:
		rep.Gone = append(rep.Gone, row.ID)
	default:
		rep.Restored = append(rep.Restored, row.ID)
	}
}
