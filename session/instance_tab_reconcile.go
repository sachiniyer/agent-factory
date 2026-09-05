package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// ReconcileTabsFromData updates this started local instance's tab list to match
// `target`, the daemon's authoritative serialized tab list (#960 PR 3). The
// daemon is the single owner of tab state, so the TUI mirrors it: tabs the
// daemon added out-of-band (present in target, absent locally) are reconnected
// to their EXACT persisted tmux session by name — like restoreLocalTabs — and
// appended, so an out-of-band tab appears in the running TUI and is immediately
// previewable/attachable (the #959 "live display" fix). A tmux-less kind (web,
// vscode — see TabKind.HasTmux) has no session to reconnect and is appended
// directly, so it lands in the TUI as its placeholder pane rather than being
// mistaken for a tab whose tmux session went missing; tabs the daemon closed
// (absent from target) are dropped locally WITHOUT re-killing their tmux session
// (the daemon already tore it down — killing again would error on the gone
// session). The agent tab (index 0) is never added or dropped: it is the
// instance's own session and is always present. Returns whether the local list
// changed. A no-op for a not-started instance, one without an agent session, or
// a remote instance (callers skip backends without TabManagement — remote tabs
// come from hook config, not the snapshot). Per-tab reconnect failures are collected into the
// returned error after every other change is applied, so one bad tab can't wedge
// the reconcile.
func (i *Instance) ReconcileTabsFromData(target []TabData) (bool, error) {
	i.mu.RLock()
	started := i.started
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	program := i.Program
	i.mu.RUnlock()

	if !started || agentTmux == nil || gw == nil {
		return false, nil
	}
	worktreePath := gw.GetWorktreePath()

	changed := false

	// The reconcile keys on the STABLE TAB ID (#1738), not the name
	// (#1886/#1905). Names are reused on close+recreate, so a name-keyed reconcile
	// reported "unchanged" for an out-of-band close+recreate and then silently
	// re-pointed the local tab's id at the NEW tab — leaving an open pane bound to
	// a tab that no longer exists, showing a different process. Keyed on the id,
	// that is a drop of the old id plus an add of the new one, which reports a
	// change and lets the pane layer close the orphaned pane.
	//
	// Name remains the join key ONLY where there is no id to key on: a local tab
	// materialized from an older daemon's CreateTab response adopts the daemon's
	// below, and a legacy roster row written before #1738 has none.
	// A local id that matches NO target id is treated as a different tab (drop +
	// add) rather than adopting the target's id: those two cases are
	// indistinguishable from here, and re-pointing the id is the silent-wrong-target
	// failure #1886 is about, whereas drop+add is a visible, self-healing blip.

	// Adopt the daemon's authoritative id for ID-LESS local tabs, by name-join. The
	// daemon is the single owner of tab identity (#960); this is the compatibility
	// bootstrap for an older daemon response or legacy row. Runs FIRST so the
	// id-keyed passes below see it. Not a visible change: the id is internal
	// addressing, not display state.
	//
	// The AGENT tab additionally adopts over a NON-EMPTY local id, because it is the
	// only row the id-keyed passes below can never repair: it is never dropped or
	// re-added (it is the instance's own session, always at index 0), so the
	// close+recreate that heals a diverged id for every other kind cannot reach it,
	// and a stale id would stick FOREVER. That divergence is ordinary, not exotic:
	// restoreLocalTabs MINTS an id for a legacy pre-#1738 row, so a TUI and a daemon
	// loading the same id-less record independently mint DIFFERENT ids — one plain
	// daemon restart (every upgrade does one) over a not-yet-persisted backfill is
	// enough. From then on every preview/live/attach addresses the agent by an id the
	// daemon cannot resolve, and because the caller DID supply a tab_id there is no
	// ordinal fallback — it is ErrTabGone (see TabAddressableServer), i.e. a blank,
	// unattachable agent pane with no way out.
	//
	// Name is the right join key here precisely where id is not: both sides derive
	// the agent tab's name from the SAME persisted record, so it agrees even when the
	// independently-minted ids do not. The pane layer is deliberately blind to this
	// heal — paneTabKeys keys the agent slot by name, so correcting the id does not
	// read as "the tab vanished" and close the pane — while liveBindCandidate keys
	// the live stream ON the id, so the adoption itself re-dials the pane onto the
	// working id. That is the self-heal.
	i.mu.Lock()
	for _, td := range target {
		if td.ID == "" {
			continue
		}
		for idx, t := range i.Tabs {
			if t.Name != td.Name || t.ID == td.ID {
				continue
			}
			// Guard the adopt-over-non-empty to the agent row on BOTH sides, and to
			// index 0 — the position that defines the agent tab here. For every other
			// kind a local id matching no target id is ambiguous (legacy mismatch vs
			// close+recreate), and the drop+add below deliberately owns that case.
			agentRow := idx == 0 && t.Kind == TabKindAgent && td.Kind == TabKindAgent
			if t.ID == "" || agentRow {
				i.replaceTabFieldLocked(idx, func(c *Tab) { c.ID = td.ID })
			}
		}
	}
	// Rename in place by stable id (#1905): a tab whose id is unchanged but whose
	// name changed out-of-band (a rename on another client, #1813) keeps its live
	// tmux session, its slot, and any open pane bound to it — only its name, and
	// so the label derived from it, changes. Without this a rename reads as "old
	// name gone, new name added", which drops the tab and re-adds it at the END
	// of the roster, blipping its PTY and reordering it.
	for _, td := range target {
		if td.ID == "" {
			continue
		}
		for idx, t := range i.Tabs {
			if t.ID == td.ID && t.Name != td.Name {
				i.replaceTabFieldLocked(idx, func(c *Tab) { c.Name = td.Name })
				changed = true
			}
		}
	}
	i.mu.Unlock()

	targetIDs := make(map[string]bool, len(target))
	targetNames := make(map[string]bool, len(target))
	// Names of target rows carrying NO id — a legacy roster written before #1738.
	// Such a row cannot be id-matched, so name is the only key it has, and a local
	// tab it covers must survive on the name alone. Without this the local tab
	// (whose id was minted on add) is dropped and re-added on EVERY poll.
	targetNamesWithoutID := make(map[string]bool, len(target))
	for _, td := range target {
		if td.ID != "" {
			targetIDs[td.ID] = true
		} else {
			targetNamesWithoutID[td.Name] = true
		}
		targetNames[td.Name] = true
	}

	// Drop local non-agent tabs the daemon no longer lists. A tab survives if the
	// daemon still lists its stable id, or — only when the daemon's row for that
	// name carries no id at all — its name. No kill: the daemon owns the teardown
	// and already closed the tmux session (#960 PR 3).
	i.mu.RLock()
	var dropIDs, dropNames []string
	for idx := 1; idx < len(i.Tabs); idx++ {
		switch t := i.Tabs[idx]; {
		case t.ID != "":
			if !targetIDs[t.ID] && !targetNamesWithoutID[t.Name] {
				dropIDs = append(dropIDs, t.ID)
			}
		case !targetNames[t.Name]:
			dropNames = append(dropNames, t.Name)
		}
	}
	i.mu.RUnlock()
	for _, id := range dropIDs {
		if i.dropTabByID(id) {
			changed = true
		}
	}
	for _, name := range dropNames {
		if i.dropTabByName(name) {
			changed = true
		}
	}

	// Snapshot what survived: the add pass keys "already present" on the id, so it
	// must be read AFTER the drops above (a close+recreate frees the reused name
	// there, and only then may the new id be added).
	i.mu.RLock()
	localIDs := make(map[string]bool, len(i.Tabs))
	localNames := make(map[string]bool, len(i.Tabs))
	for _, t := range i.Tabs {
		if t.ID != "" {
			localIDs[t.ID] = true
		}
		localNames[t.Name] = true
	}
	i.mu.RUnlock()

	// Add daemon-listed tabs missing locally, reconnecting each to its exact
	// persisted tmux session by name so it is immediately attachable.
	var firstErr error
	for _, td := range target {
		if td.Kind == TabKindAgent {
			continue
		}
		if td.ID != "" {
			if localIDs[td.ID] {
				continue
			}
		} else if localNames[td.Name] {
			continue // legacy roster row: the name is the only key it has
		}
		kind := tabKindForData(td.Kind)
		// A tmux-less kind (web, vscode) is materialized by the append alone — there
		// is no session to reconnect, exactly as restoreLocalTabs builds it on load.
		// Skipping it on its empty TmuxName (as this loop once did) read "" as a
		// missing session rather than a kind that never has one, so a web/vscode tab
		// created out-of-band stayed invisible in a running TUI until a full rebuild
		// — even though #1815 now delivers the roster that carries it, and even though
		// the DROP side above already removed such a tab by name. See TabKind.HasTmux.
		var ts *tmux.TmuxSession
		if kind.HasTmux() {
			if td.TmuxName == "" || worktreePath == "" {
				continue
			}
			// The sibling inherits the agent session's PTY factory / executor (real
			// in production, mock in tests), binding to the EXACT persisted name.
			// ATTACH-ONLY: pass empty workDir so a missing session errors instead of
			// re-spawning (#1152). Like AttachShellTab, this is a pure TUI-side
			// projection of daemon-owned tabs; the daemon is the single writer that
			// owns every spawn (#960). If the daemon killed the session in the race
			// window, re-spawning here would orphan a tmux session over the deleted
			// worktree. Skip the tab on failure and let the next snapshot reconcile it.
			ts = agentTmux.NewSiblingSession(td.TmuxName, tabProgram(kind, td.Command, program))
			if err := ts.Restore(""); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("failed to reconnect tab %q: %w", td.Name, err)
				}
				continue
			}
		}
		id := td.ID
		if id == "" {
			id = newTabID()
		}
		// URL rides along for a web tab (a vscode tab has none by design — its target
		// is resolved at proxy time), or the pane would have nothing to iframe.
		tab := &Tab{ID: id, Name: td.Name, Kind: kind, Command: td.Command, URL: td.URL, tmux: ts}
		// Adopt under the write lock, re-checking BOTH the already-present dedupe (a
		// concurrent reconcile/AddTab may have added this tab while we reconnected
		// outside the lock) and the teardown fence a Kill/archive can have raised in
		// that same window (#2100). See appendReconciledTab.
		if i.appendReconciledTab(td.ID, td.Name, tab) {
			changed = true
		}
	}

	// Reorder LAST, once the local set matches the daemon's. A pure reorder leaves
	// every id and name unchanged, so every pass above is a no-op for it and the
	// order would never reach a running TUI until restart (#1813). Permuting to the
	// daemon's authoritative order here is what closes that gap.
	if i.reorderTabsFromData(target) {
		changed = true
	}

	return changed, firstErr
}

// dropTabByName removes the named non-agent tab from the in-memory list WITHOUT
// killing its tmux session — the no-kill counterpart of CloseTab used by
// ReconcileTabsFromData when the daemon has already torn the session down (#960
// PR 3). Returns whether a tab was removed. The agent tab (index 0) is never
// dropped.
func (i *Instance) dropTabByName(name string) bool {
	return i.dropTabWhere(func(t *Tab) bool { return t.Name == name }, "name "+name)
}

// dropTabByID is dropTabByName keyed on the stable id (#1738) — what the
// id-keyed snapshot reconcile drops on, so a tab whose id left the daemon's
// roster goes even when a NEW tab has already reused its name (#1886).
func (i *Instance) dropTabByID(id string) bool {
	if id == "" {
		return false
	}
	return i.dropTabWhere(func(t *Tab) bool { return t.ID == id }, "id "+id)
}

// dropTabWhere removes the first non-agent tab matching pred and releases its
// TUI-side attach PTY. label names the match for the log line only.
func (i *Instance) dropTabWhere(pred func(*Tab) bool, label string) bool {
	i.mu.Lock()
	var dropped *Tab
	for idx := 1; idx < len(i.Tabs); idx++ {
		if pred(i.Tabs[idx]) {
			dropped = i.Tabs[idx]
			i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
			break
		}
	}
	i.mu.Unlock()
	if dropped == nil {
		return false
	}
	// Release the TUI-side attach PTY the dropped tab held — its ptmx fd and the
	// blocked cmd.Wait goroutine — mirroring CloseTab/AttachShellTab. No kill: the
	// daemon already tore the tmux session down (#960 PR 3), so this only releases
	// this client's attach resources. Done outside the lock so the tmux teardown
	// never runs while holding i.mu.
	if dropped.tmux != nil {
		if err := dropped.tmux.CloseAttachOnly(); err != nil {
			log.WarningLog.Printf("dropTab (%s): releasing attach client: %v", label, err)
		}
	}
	return true
}
