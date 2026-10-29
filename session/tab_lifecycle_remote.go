package session

import (
	"errors"
	"time"

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

// tabIntentTTL bounds how long an uncertain create's identity is remembered. The
// intent is what lets a caller's retry join the one execution, and it survives
// the sweep adopting the tab (#5996) so that join works whichever of the retry
// and the sweep comes first; it cannot live forever, or a later request that
// really does want a second run would be answered by the first.
var tabIntentTTL = 10 * time.Minute

// tabCreateIntent is a remote create whose outcome the daemon could not see.
type tabCreateIntent struct {
	id      string
	kind    TabKind
	command string
	// requested is the caller's requested name (the retry-lookup key); name is
	// the display name the attempt claimed (used if the sweep adopts the tab).
	requested string
	name      string
	at        time.Time
	// adopted records that the sweep already put the sandbox's tab on the roster.
	// The intent stays until the retry consumes it (or it expires), but there is
	// nothing left for the sweep to do with it.
	adopted bool
}

// intentKey identifies an intent: the id AND the request shape it was minted
// for. Every operation on behalf of a request touches only the intent under its
// own key, whatever its outcome, so a different request that reuses the id can
// neither read, overwrite nor consume another request's record (#6030).
func intentKey(id string, kind TabKind, command, requestedName string) string {
	return id + "\x00" + TabKindName(kind) + "\x00" + command + "\x00" + requestedName
}

// intentHeldByOtherShapeLocked reports whether id is the recorded identity of an
// uncertain create for a DIFFERENT request that is still unresolved. An adopted
// intent is on the roster, where the id collision check judges any conflict. Caller holds i.mu.
func (i *Instance) intentHeldByOtherShapeLocked(id string, kind TabKind, command, requestedName string) bool {
	for _, in := range i.tabCreateIntents {
		if in.id == id && !in.adopted && (in.kind != kind || in.command != command || in.requested != requestedName) &&
			time.Since(in.at) <= tabIntentTTL {
			return true
		}
	}
	return false
}

// intentIDLocked returns the id an uncertain earlier attempt at this exact
// request minted, or "". Intents are kept per minted id, so two same-shape
// creates whose answers were both lost each keep their own (#6021); a retry
// takes the oldest one no other attempt is currently re-running, which is how a
// second retry reaches the second execution rather than joining the first; when
// every one is being re-run it joins the oldest of those.
// Caller holds i.mu.
func (i *Instance) intentIDLocked(kind TabKind, command, requestedName string) string {
	var free, running *tabCreateIntent
	older := func(a, b *tabCreateIntent) bool {
		return b == nil || a.at.Before(b.at) || (a.at.Equal(b.at) && a.id < b.id)
	}
	for key, in := range i.tabCreateIntents {
		if time.Since(in.at) > tabIntentTTL {
			delete(i.tabCreateIntents, key)
			continue
		}
		if in.kind != kind || in.command != command || in.requested != requestedName {
			continue
		}
		if _, inFlight := i.tabCreateInFlight[in.id]; inFlight {
			if older(in, running) {
				running = in
			}
		} else if older(in, free) {
			free = in
		}
	}
	// A shape with an intent never mints a new id: with none free, the retry
	// joins an attempt already in flight (and so its one execution) rather than
	// starting the command again (#6030).
	switch {
	case free != nil:
		return free.id
	case running != nil:
		return running.id
	}
	return ""
}

// consumeIntentLocked retires the intent a create has resolved: the one that
// minted id for THIS request shape — by a fresh create, a definitive refusal, or
// joining a tab already on the roster (the sweep's adoption). A different request
// that merely reused the id (and was refused for it) answers nothing about the
// original attempt, whose execution may still be running unseen, so its intent
// stays (#6030). Caller holds i.mu.
func (i *Instance) consumeIntentLocked(id string, kind TabKind, command, requestedName string) {
	delete(i.tabCreateIntents, intentKey(id, kind, command, requestedName))
}

// recordIntentLocked keeps id as the identity of this attempt after an uncertain
// create. Caller holds i.mu.
func (i *Instance) recordIntentLocked(id string, kind TabKind, command, requestedName, displayName string) {
	if i.tabCreateIntents == nil {
		i.tabCreateIntents = map[string]*tabCreateIntent{}
	}
	i.tabCreateIntents[intentKey(id, kind, command, requestedName)] = &tabCreateIntent{
		id: id, kind: kind, command: command, requested: requestedName, name: displayName, at: time.Now(),
	}
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

// dropAdoptedIntentsLocked ends the retry-join window of a tab that is now
// closed: an adopted intent left behind would point a later request at a closed
// (or re-created) id. It is applied only once the close is COMMITTED — a close
// whose commit fails rolls back to exactly the pre-close state, intents
// included. Caller holds i.mu.
func (i *Instance) dropAdoptedIntentsLocked(tabID string) {
	for key, in := range i.tabCreateIntents {
		if in.id == tabID && in.adopted {
			delete(i.tabCreateIntents, key)
		}
	}
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
func (i *Instance) ReconcileOffBoxTabs() (changed bool) {
	i.mu.RLock()
	rs := i.remoteTabServerCachedLocked()
	work := len(i.pendingRemoteClose) > 0
	for _, in := range i.tabCreateIntents {
		if !in.adopted && time.Since(in.at) <= tabIntentTTL {
			work = true
			break
		}
	}
	if !work {
		for _, t := range i.Tabs {
			if t.Kind == TabKindProcess && t.tmux == nil && t.Exit == nil && t.gen != "" {
				work = true
				break
			}
		}
	}
	remote := i.capabilitiesLocked().Workspace == WorkspaceRemote
	hasClient := i.remoteClient != nil
	i.mu.RUnlock()
	if !remote || !work {
		return false
	}
	if rs == nil && hasClient {
		// After a restart nothing has built the agent-server yet, and a pending
		// close must not wait on an unrelated operation to do it. Built only with
		// a live client: a client-less row would cache a dead server.
		rs, _ = i.AgentServer().(*remoteAgentServer)
	}
	if rs == nil {
		return false
	}
	// Warm the advertisement: after a daemon restart nothing has asked yet, and a
	// pending close must not wait on an unrelated operation to learn the tab API.
	rs.idNative()
	if !rs.hasFeatureCached(tabAPIFeature) {
		return false
	}
	roster, err := rs.rc.tabs()
	if err != nil {
		return false
	}
	byID := make(map[string]TabData, len(roster))
	for _, td := range roster {
		byID[td.ID] = td
	}
	changed = i.retryPendingRemoteClose(rs, byID)
	rosterChanged, adopted := i.applyOffBoxRoster(byID)
	for _, id := range adopted {
		// An adopted replacement may reuse a closed id: lift its terminal mark, as
		// the create path does, so its stream is usable (#5963).
		rs.clearTabGone(id)
	}
	if rosterChanged {
		i.markLoadRuntimeReplaced()
	}
	// The caller persists and publishes on true: nothing else writes a change the
	// sweep discovers, and a stable session would otherwise keep showing a missing
	// tab or a running command until an unrelated write.
	return changed || rosterChanged
}

// retryPendingRemoteClose spends each pending close. The generation check is the
// SANDBOX's, made atomically with the removal (the conditional close), so a
// retry cannot close a newer incarnation of a reused id between a check and the
// act (#5996). ErrTabGone — the id is absent, or held by another incarnation —
// confirms the recorded tab is already gone. The record is retired only on the
// sandbox's answer.
//
// A record with no generation (the sandbox had not reported one) cannot be
// fenced, so it falls back to the roster just read: closed only if the id is
// still present. That residual check-then-act exists solely for tabs created
// before generations; every tab a current sandbox creates carries one.
func (i *Instance) retryPendingRemoteClose(rs *remoteAgentServer, byID map[string]TabData) (retired bool) {
	for _, h := range i.PendingRemoteTabClose() {
		_, present := byID[h.TabID]
		if h.Gen != "" || present {
			if cerr := rs.rc.closeTab(h.TabID, h.Gen); cerr != nil && !errors.Is(cerr, ErrTabGone) {
				log.WarningLog.Printf("session %q: close of sandbox tab %q is still unconfirmed and will be retried: %v", i.Title, h.TabID, cerr)
				continue
			}
		}
		i.mu.Lock()
		i.retireRemoteCloseLocked(&h)
		i.mu.Unlock()
		log.InfoLog.Printf("session %q: sandbox tab %q close confirmed", i.Title, h.TabID)
		retired = true
	}
	return retired
}

// applyOffBoxRoster adopts the tabs uncertain creates started and stamps exit
// evidence, both under one acquisition. An exit is applied only to the row that
// is the same incarnation (id AND gen) and records no ending yet, so a
// replacement tab under a reused id never inherits its predecessor's (#5960).
// Reports whether anything durable changed.
func (i *Instance) applyOffBoxRoster(byID map[string]TabData) (changed bool, adopted []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for key, in := range i.tabCreateIntents {
		if time.Since(in.at) > tabIntentTTL {
			delete(i.tabCreateIntents, key)
			continue
		}
		td, ok := byID[in.id]
		// Only the intent whose request the sandbox's tab actually is: a different
		// shape recorded on the same id describes no execution there.
		if !ok || in.adopted || td.Command != in.command {
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
			adopted = append(adopted, in.id)
		}
		// The intent is NOT retired here. A caller whose create failed with an
		// unknown outcome will retry, and that retry must still resolve to this
		// id and join the one execution (#5996); adopting the row only makes the
		// join find it. The retry consumes the intent; the TTL bounds the rest.
		in.adopted = true
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
	return changed, adopted
}
