package session

import (
	"fmt"
	"strings"
)

// AttachVSCodeTab reflects a VS Code tab that the daemon's CreateTab RPC has
// already created and persisted. It is the metadata-only counterpart of
// AttachShellTab: no editor or tmux session is spawned here; the TUI appends a
// projection immediately so the new tab is visible without waiting for the next
// snapshot.
//
// tabID is the daemon-minted identity returned by CreateTab. An empty ID is the
// explicit mixed-version fallback for an older daemon; this projection stays
// name-keyed only until the next authoritative snapshot.
func (i *Instance) AttachVSCodeTab(name, tabID string) (*Tab, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("cannot attach a VS Code tab without a name")
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if tab, ok, err := i.resolveAttachedTabLocked(name, tabID); ok || err != nil {
		return tab, err
	}
	if spawnErr := i.tabSpawnBlockedLocked(); spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(i.started, i.tmuxLocked() != nil, i.gitWorktree != nil || i.capabilitiesLocked().RemoteEditor, TabKindVSCode); err != nil {
		return nil, err
	}

	tab := newVSCodeTab()
	tab.ID = tabID
	tab.Name = name
	i.Tabs = append(i.Tabs, tab)
	i.touchLocked()
	return tab, nil
}

// resolveAttachedTabLocked reconciles an instant-display attach with a
// snapshot that may have won the race. A non-empty daemon ID is authoritative:
// it follows a rename, adopts an older id-less row, and refuses a same-name row
// that belongs to a replacement tab. Empty preserves the older daemon's
// deliberate name-keyed compatibility path.
func (i *Instance) resolveAttachedTabLocked(name, tabID string) (*Tab, bool, error) {
	if tabID != "" {
		for _, tab := range i.Tabs {
			if tab.ID == tabID {
				return tab, true, nil
			}
		}
	}
	for idx, tab := range i.Tabs {
		if tab.Name != name {
			continue
		}
		if tabID == "" {
			return tab, true, nil
		}
		if tab.ID == "" {
			// Adopt the daemon's id through copy-on-write, not an in-place
			// tab.ID = tabID: this pointer may already be held by a GetTabs
			// snapshot whose callers read ID off-lock, so writing the field in
			// place races them (the instance.go copy-on-write invariant, #1930).
			// ReconcileTabsFromData adopts an id the same way; #2420.
			i.replaceTabFieldLocked(idx, func(c *Tab) { c.ID = tabID })
			return i.Tabs[idx], true, nil
		}
		return nil, true, fmt.Errorf(
			"cannot attach daemon tab %q (%s): that name now belongs to tab %s",
			name, tabID, tab.ID,
		)
	}
	return nil, false, nil
}

// AttachOffBoxTab reflects a tab the daemon's CreateTab RPC already committed on
// an off-box row (#6020). Such a row owns no tmux sessions — every tab is a
// tmux-less projection addressed by its stable id, exactly what
// reconcileProjectedTabs builds from a snapshot — so the local-only
// AttachShellTab, which needs a started tmux session and a worktree, can never
// apply. The projection is the one the next snapshot would produce; it exists
// only so the new tab is visible and selectable without waiting for the poll.
//
// An empty tabID is the older-daemon fallback and stays name-keyed until the next
// snapshot, as for AttachVSCodeTab.
func (i *Instance) AttachOffBoxTab(kind TabKind, name, tabID string) (*Tab, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("cannot attach a tab without a name")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.capabilitiesLocked().Workspace != WorkspaceRemote {
		return nil, fmt.Errorf("cannot attach an off-box tab to a session that runs locally")
	}
	if tab, ok, err := i.resolveAttachedTabLocked(name, tabID); ok || err != nil {
		return tab, err
	}
	if spawnErr := i.tabSpawnBlockedLocked(); spawnErr != nil {
		return nil, spawnErr
	}
	tab := &Tab{ID: tabID, Name: name, Kind: kind}
	i.Tabs = append(i.Tabs, tab)
	i.tabRosterGeneration++
	i.touchLocked()
	return tab, nil
}
