package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
)

// DropClosedTab removes the tab at idx from the in-memory list WITHOUT killing
// its tmux session (#960 PR 2). It is the no-kill counterpart of CloseTab, used
// when the daemon's CloseTab RPC has already torn the tmux session down: the
// daemon owns the kill+persist, and the TUI only needs to drop the now-dead tab
// from its local view for instant display. Killing again here would shell out a
// second tmux kill-session that errors ("session not found") on the already-gone
// session and surface a spurious failure. The agent tab (idx 0) is undroppable;
// errors on idx 0 or any out-of-range index, mirroring CloseTab.
func (i *Instance) DropClosedTab(idx int) error {
	i.mu.Lock()
	if idx <= 0 || idx >= len(i.Tabs) {
		i.mu.Unlock()
		return fmt.Errorf("tab cannot be closed")
	}
	tab := i.Tabs[idx]
	i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
	i.touchLocked()
	i.mu.Unlock()

	// Release the TUI-side attach PTY (ptmx fd + blocked cmd.Wait goroutine) the
	// dropped tab held, matching CloseTab. No kill: the daemon's CloseTab RPC
	// already tore the tmux session down (#960 PR 2), so CloseAttachOnly only
	// releases this client's attach resources. Done outside the lock so the tmux
	// teardown never runs while holding i.mu.
	if tab.tmux != nil {
		if err := tab.tmux.CloseAttachOnly(); err != nil {
			log.WarningLog.Printf("DropClosedTab: releasing attach client for tab %q: %v", tab.Name, err)
		}
	}
	return nil
}
