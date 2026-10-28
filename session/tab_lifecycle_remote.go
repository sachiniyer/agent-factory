package session

import (
	"errors"

	"github.com/sachiniyer/agent-factory/log"
)

// The off-box tab lifecycle (#5571; #5952, #5955-#5961, #5963). The sandbox's
// agent-server is the authority on its own tabs and addresses them by the
// daemon's stable id, so every guarantee here is keyed by that id (plus a
// per-incarnation gen where the id alone cannot tell two tabs apart):
//
//   - create: an uncertain outcome keeps its minted id as an intent, so a retry
//     reuses it and the sandbox's id idempotency observes the one execution;
//   - close: a close the sandbox never confirmed leaves a durable record that
//     the status sweep retries until the sandbox answers or says the tab is gone;
//   - exit: refreshed from the sandbox roster and applied only to the
//     incarnation that produced it;
//   - streams: a closing or closed id is terminal (see closeTabStream).
//
// This file holds the daemon-side bookkeeping those rules share. Nothing here
// holds i.mu across a network call.

// tabCreateIntent is a remote create whose outcome the daemon could not see.
type tabCreateIntent struct {
	id      string
	kind    TabKind
	command string
	name    string
}

func tabIntentKey(kind TabKind, command, requestedName string) string {
	return TabKindName(kind) + "\x00" + command + "\x00" + requestedName
}

// intentIDLocked returns the id an uncertain earlier attempt at this exact
// request minted, or "". Caller holds i.mu.
func (i *Instance) intentIDLocked(kind TabKind, command, requestedName string) string {
	if in := i.tabCreateIntents[tabIntentKey(kind, command, requestedName)]; in != nil {
		return in.id
	}
	return ""
}

// recordIntentLocked keeps id as the identity of this request after an uncertain
// create. Caller holds i.mu.
func (i *Instance) recordIntentLocked(id string, kind TabKind, command, requestedName, displayName string) {
	if i.tabCreateIntents == nil {
		i.tabCreateIntents = map[string]*tabCreateIntent{}
	}
	i.tabCreateIntents[tabIntentKey(kind, command, requestedName)] = &tabCreateIntent{
		id: id, kind: kind, command: command, name: displayName,
	}
}

func (i *Instance) clearIntentLocked(kind TabKind, command, requestedName string) {
	delete(i.tabCreateIntents, tabIntentKey(kind, command, requestedName))
}

// createOutcomeUncertain reports whether a failed create leaves the sandbox's
// state unknown. An answer from the agent-server (it ran the request and
// refused) is definitive; anything else — a timeout, a dropped connection — may
// have happened on the far side (#5958).
func createOutcomeUncertain(err error) bool {
	var answered *agentAnswered
	return err != nil && !errors.As(err, &answered)
}

// isOffBoxTab reports whether tab is a shell/process tab driven through the
// sandbox rather than a local tmux session. Caller holds i.mu.
func (i *Instance) isOffBoxTabLocked(tab *Tab) bool {
	return tab != nil && tab.tmux == nil && tab.ID != "" &&
		(tab.Kind == TabKindShell || tab.Kind == TabKindProcess) &&
		i.capabilitiesLocked().Workspace == WorkspaceRemote
}

// stageRemoteCloseLocked records the pending close of an off-box tab in the same
// critical section that removes its row, so the roster never shows a tab as gone
// while nothing names a command that may still be running (#5957). Nil for a
// tab that is not off-box. Caller holds i.mu.
func (i *Instance) stageRemoteCloseLocked(tab *Tab) *RemoteTabCleanupData {
	if !i.isOffBoxTabLocked(tab) {
		return nil
	}
	h := RemoteTabCleanupData{TabID: tab.ID, Gen: tab.gen}
	for _, p := range i.pendingRemoteClose {
		if p == h {
			return &h
		}
	}
	i.pendingRemoteClose = append(i.pendingRemoteClose, h)
	i.touchLocked()
	return &h
}

// retireRemoteCloseLocked drops a confirmed pending close. Caller holds i.mu.
func (i *Instance) retireRemoteCloseLocked(h *RemoteTabCleanupData) {
	if h == nil {
		return
	}
	var kept []RemoteTabCleanupData
	for _, p := range i.pendingRemoteClose {
		if p != *h {
			kept = append(kept, p)
		}
	}
	if len(kept) != len(i.pendingRemoteClose) {
		i.pendingRemoteClose = kept
		i.touchLocked()
	}
}

// remoteCloseDataLocked projects the pending closes for storage.
func (i *Instance) remoteCloseDataLocked() []RemoteTabCleanupData {
	if len(i.pendingRemoteClose) == 0 {
		return nil
	}
	return append([]RemoteTabCleanupData(nil), i.pendingRemoteClose...)
}

// restoredRemoteClose rebuilds the pending closes from a stored record, dropping
// entries with no id (nothing to address).
func restoredRemoteClose(data []RemoteTabCleanupData) []RemoteTabCleanupData {
	var out []RemoteTabCleanupData
	for _, d := range data {
		if d.TabID != "" {
			out = append(out, d)
		}
	}
	return out
}

// PendingRemoteTabClose returns a copy of the unconfirmed off-box closes.
func (i *Instance) PendingRemoteTabClose() []RemoteTabCleanupData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.remoteCloseDataLocked()
}

// ReconcileOffBoxTabs brings the daemon's off-box tab state back in line with
// the sandbox's roster: it retires or retries pending closes, adopts a tab an
// uncertain create did start, and refreshes process-tab exit evidence. It is
// the one place that reads that roster, and it is a no-op for an instance with
// nothing to reconcile. The status sweep calls it per off-box instance; it runs
// no network call under i.mu and a failed roster read changes nothing, so every
// step simply waits for the next sweep.
func (i *Instance) ReconcileOffBoxTabs() {
	i.mu.RLock()
	rs := i.remoteTabServerCachedLocked()
	work := len(i.pendingRemoteClose) > 0 || len(i.tabCreateIntents) > 0
	if !work {
		for _, t := range i.Tabs {
			if t.Kind == TabKindProcess && t.tmux == nil && t.Exit == nil && t.gen != "" {
				work = true
				break
			}
		}
	}
	remote := i.capabilitiesLocked().Workspace == WorkspaceRemote
	i.mu.RUnlock()
	if rs == nil || !remote || !work {
		return
	}
	// Warm the advertisement: after a daemon restart nothing has asked yet, and a
	// pending close must not wait on an unrelated operation to learn the tab API.
	rs.idNative()
	if !rs.hasFeatureCached(tabAPIFeature) {
		return
	}
	roster, err := rs.rc.tabs()
	if err != nil {
		return
	}
	byID := make(map[string]TabData, len(roster))
	for _, td := range roster {
		byID[td.ID] = td
	}
	i.retryPendingRemoteClose(rs, byID)
	if i.applyOffBoxRoster(byID) {
		i.markLoadRuntimeReplaced()
	}
}

// retryPendingRemoteClose spends each pending close against the roster just
// read. The roster is authoritative: an id it lacks, or holds under another
// incarnation, is already closed; an id it holds is closed now, and the record
// is retired only on the sandbox's confirmation.
func (i *Instance) retryPendingRemoteClose(rs *remoteAgentServer, byID map[string]TabData) {
	for _, h := range i.PendingRemoteTabClose() {
		td, present := byID[h.TabID]
		if present && (h.Gen == "" || td.Gen == "" || td.Gen == h.Gen) {
			if cerr := rs.rc.closeTab(h.TabID); cerr != nil && !errors.Is(cerr, ErrTabGone) {
				log.WarningLog.Printf("session %q: close of sandbox tab %q is still unconfirmed and will be retried: %v", i.Title, h.TabID, cerr)
				continue
			}
		}
		i.mu.Lock()
		i.retireRemoteCloseLocked(&h)
		i.mu.Unlock()
		log.InfoLog.Printf("session %q: sandbox tab %q close confirmed", i.Title, h.TabID)
	}
}

// applyOffBoxRoster adopts the tabs uncertain creates started and stamps exit
// evidence, both under one acquisition. An exit is applied only to the row that
// is the same incarnation (id AND gen) and records no ending yet, so a
// replacement tab under a reused id never inherits its predecessor's (#5960).
// Reports whether anything durable changed.
func (i *Instance) applyOffBoxRoster(byID map[string]TabData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	changed := false
	for key, in := range i.tabCreateIntents {
		td, ok := byID[in.id]
		if !ok {
			continue
		}
		if _, have := i.tabIndexByIDLocked(in.id); !have && i.started && i.tabSpawnBlockedLocked() == nil {
			name := uniqueTabNameReserving(i.Tabs, i.tabDisplayClaims, in.name)
			i.Tabs = append(i.Tabs, &Tab{
				ID: in.id, Name: name, Kind: in.kind, Command: in.command,
				Exit: tabExitFromData(td.Exit), gen: td.Gen,
			})
			i.touchLocked()
			changed = true
		}
		delete(i.tabCreateIntents, key)
	}
	for idx, tab := range i.Tabs {
		if tab.Kind != TabKindProcess || tab.tmux != nil || tab.Exit != nil || tab.gen == "" {
			continue
		}
		td, ok := byID[tab.ID]
		if !ok || td.Gen != tab.gen || td.Exit == nil {
			continue
		}
		exit := tabExitFromData(td.Exit)
		i.replaceTabFieldLocked(idx, func(c *Tab) { c.Exit = exit })
		i.touchLocked()
		changed = true
	}
	return changed
}
