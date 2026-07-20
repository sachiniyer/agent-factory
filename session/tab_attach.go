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
// The stable id is deliberately left empty. The daemon owns tab identity and the
// next ReconcileTabsFromData joins this id-less projection to the authoritative
// row by name, exactly as it does for AttachShellTab. Minting a competing local id
// would make the reconcile read the same tab as a remove+add and close its newly
// opened pane.
func (i *Instance) AttachVSCodeTab(name string) (*Tab, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("cannot attach a VS Code tab without a name")
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	for _, tab := range i.Tabs {
		if tab.Name == name {
			return tab, nil
		}
	}
	if spawnErr := i.tabSpawnBlockedLocked(); spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(i.started, i.tmuxLocked() != nil, i.gitWorktree != nil); err != nil {
		return nil, err
	}
	if len(i.Tabs) >= maxTabs {
		return nil, fmt.Errorf("max %d tabs per session", maxTabs)
	}

	tab := newVSCodeTab()
	tab.ID = ""
	tab.Name = name
	i.Tabs = append(i.Tabs, tab)
	return tab, nil
}
